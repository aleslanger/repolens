// Package scan discovers git repositories under a directory and groups linked worktrees.
package scan

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"repolens/internal/privacy"
	"repolens/internal/remote"
)

type Worktree struct {
	Path   string
	Branch string
}

type Repo struct {
	Path      string // top level of the main working tree
	CommonDir string
	Remote    remote.Remote
	HasRemote bool
	Worktrees []Worktree // linked worktrees, which may live outside the scanned root
}

type Stats struct {
	Candidates int
	Errors     []Problem
}

// Problem is a directory the scan could not read, kept apart from the message so it can be
// excluded from later scans.
type Problem struct {
	Path, Err string
}

var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".venv": true, "venv": true, "target": true,
	"dist": true, "build": true, ".cache": true, ".next": true, ".gradle": true, ".idea": true,
}

// Find walks root up to maxDepth directory levels and returns repositories sorted by path.
// Directories for which skip returns true are not entered (excluded paths); skip may be nil.
func Find(ctx context.Context, root string, maxDepth int, skip func(string) bool) ([]Repo, Stats, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, Stats{}, fmt.Errorf("resolve %s: %w", root, err)
	}
	var st Stats
	byCommon := map[string]*Repo{}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			st.Errors = append(st.Errors, Problem{Path: path, Err: err.Error()})
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (skipDirs[d.Name()] || d.Name() == ".git" || privacy.Forbidden(path) || (skip != nil && skip(path))) {
			return filepath.SkipDir
		}
		if depth(root, path) > maxDepth {
			return filepath.SkipDir
		}
		if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
			return nil
		}
		st.Candidates++
		if err := addCandidate(ctx, path, byCommon); err != nil {
			st.Errors = append(st.Errors, Problem{Path: path, Err: err.Error()})
		}
		return nil
	})
	if walkErr != nil {
		return nil, st, fmt.Errorf("walk %s: %w", root, walkErr)
	}
	repos := make([]Repo, 0, len(byCommon))
	for _, r := range byCommon {
		repos = append(repos, *r)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Path < repos[j].Path })
	return repos, st, nil
}

func depth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

func addCandidate(ctx context.Context, dir string, byCommon map[string]*Repo) error {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common := strings.TrimSpace(out)
	if _, done := byCommon[common]; done {
		return nil
	}
	repo, err := describe(ctx, dir, common)
	if err != nil {
		return err
	}
	byCommon[common] = repo
	return nil
}

func describe(ctx context.Context, dir, common string) (*Repo, error) {
	wtOut, err := git(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	wts := parseWorktrees(wtOut)
	if len(wts) == 0 {
		return nil, fmt.Errorf("%s: no worktrees reported", dir)
	}
	r := &Repo{Path: wts[0].Path, CommonDir: common, Worktrees: wts[1:]}
	// A missing origin is normal for local-only repositories.
	if url, err := git(ctx, dir, "config", "--get", "remote.origin.url"); err == nil {
		r.Remote, r.HasRemote = remote.Parse(url)
	}
	return r, nil
}

// parseWorktrees reads `git worktree list --porcelain`; the first entry is the main tree.
func parseWorktrees(out string) []Worktree {
	var wts []Worktree
	for block := range strings.SplitSeq(strings.TrimSpace(out), "\n\n") {
		var wt Worktree
		bare := false
		for line := range strings.SplitSeq(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				wt.Path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "branch "):
				wt.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
			case line == "bare":
				bare = true
			}
		}
		if wt.Path != "" && !bare {
			wts = append(wts, wt)
		}
	}
	return wts
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
