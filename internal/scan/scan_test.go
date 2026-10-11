package scan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
}

func TestFindGroupsWorktreesAndSkipsJunk(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	initRepo(t, filepath.Join(root, "a"))
	run(t, filepath.Join(root, "a"), "remote", "add", "origin", "git@github.com:Me/A.git")
	run(t, filepath.Join(root, "a"), "worktree", "add", "-q", "-b", "feat", filepath.Join(outside, "a-feat"))
	run(t, filepath.Join(root, "a"), "worktree", "add", "-q", "-b", "inner", filepath.Join(root, "a-inner"))
	initRepo(t, filepath.Join(root, "group", "b"))
	initRepo(t, filepath.Join(root, "x", "node_modules", "dep"))
	initRepo(t, filepath.Join(root, "1", "2", "3", "too-deep"))

	repos, st, err := Find(context.Background(), root, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 {
		t.Fatalf("errors: %v", st.Errors)
	}
	if len(repos) != 2 {
		t.Fatalf("repos = %+v, want 2", repos)
	}
	a := repos[0]
	if a.Path != filepath.Join(root, "a") || !a.HasRemote || a.Remote.Key() != "github.com/me/a" {
		t.Errorf("repo a = %+v", a)
	}
	if len(a.Worktrees) != 2 {
		t.Fatalf("worktrees = %+v, want 2", a.Worktrees)
	}
	branches := map[string]string{}
	for _, w := range a.Worktrees {
		branches[w.Branch] = w.Path
	}
	if branches["feat"] != filepath.Join(outside, "a-feat") || branches["inner"] != filepath.Join(root, "a-inner") {
		t.Errorf("worktree branches = %v", branches)
	}
	if b := repos[1]; b.HasRemote || b.Path != filepath.Join(root, "group", "b") {
		t.Errorf("repo b = %+v", b)
	}
}

func TestParseWorktreesSkipsBare(t *testing.T) {
	out := "worktree /r\nbare\n\nworktree /w\nHEAD abc\nbranch refs/heads/x\n"
	got := parseWorktrees(out)
	if len(got) != 1 || got[0].Path != "/w" || got[0].Branch != "x" {
		t.Errorf("got %+v", got)
	}
}

func TestFindSkipsExcludedDirectories(t *testing.T) {
	root := t.TempDir()
	initRepo(t, filepath.Join(root, "keep"))
	initRepo(t, filepath.Join(root, "data", "inner"))
	skipped := filepath.Join(root, "data")
	repos, _, err := Find(context.Background(), root, 3, func(p string) bool { return p == skipped })
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || filepath.Base(repos[0].Path) != "keep" {
		t.Errorf("repos = %+v, want only keep", repos)
	}
}
