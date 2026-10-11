// Package gitlog runs and parses `git log --numstat` in a delimiter-safe format.
package gitlog

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	recordSep  = '\x1e'
	fieldSep   = "\x1f"
	trailerSep = "\x1d"
	// fieldCount is the number of \x1f-separated fields, numstat block included.
	fieldCount = 9
	maxRecord  = 64 << 20
)

// Format is the --format argument matching Parse.
const Format = "%x1e%H%x1f%an%x1f%ae%x1f%aI%x1f%cI%x1f%P%x1f%s%x1f%(trailers:unfold,separator=%x1d)%x1f"

type FileStat struct {
	Path           string
	Added, Deleted int
	Binary         bool
}

type Trailer struct{ Key, Value string }

type Commit struct {
	Hash                    string
	AuthorName, AuthorEmail string
	AuthorTime, CommitTime  time.Time
	Parents                 []string
	Subject                 string
	Trailers                []Trailer
	Files                   []FileStat
}

func (c Commit) IsMerge() bool { return len(c.Parents) > 1 }

func (c Commit) Additions() int {
	n := 0
	for _, f := range c.Files {
		n += f.Added
	}
	return n
}

func (c Commit) Deletions() int {
	n := 0
	for _, f := range c.Files {
		n += f.Deleted
	}
	return n
}

// Stats counts what Parse saw; bad records are skipped, not fatal.
type Stats struct {
	Records    int
	BadRecords int
	BadLines   int
}

// Parse streams commits from git log output produced with Format and --numstat.
func Parse(r io.Reader, fn func(Commit) error) (Stats, error) {
	var st Stats
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), maxRecord)
	sc.Split(splitRecords)
	for sc.Scan() {
		rec := sc.Text()
		if strings.TrimSpace(rec) == "" {
			continue
		}
		st.Records++
		c, badLines, err := parseRecord(rec)
		st.BadLines += badLines
		if err != nil {
			st.BadRecords++
			continue
		}
		if err := fn(c); err != nil {
			return st, err
		}
	}
	if err := sc.Err(); err != nil {
		return st, fmt.Errorf("read git log: %w", err)
	}
	return st, nil
}

func splitRecords(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, recordSep); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func parseRecord(rec string) (Commit, int, error) {
	f := strings.SplitN(rec, fieldSep, fieldCount)
	if len(f) != fieldCount {
		return Commit{}, 0, fmt.Errorf("expected %d fields, got %d", fieldCount, len(f))
	}
	at, err := time.Parse(time.RFC3339, f[3])
	if err != nil {
		return Commit{}, 0, fmt.Errorf("author time: %w", err)
	}
	ct, err := time.Parse(time.RFC3339, f[4])
	if err != nil {
		return Commit{}, 0, fmt.Errorf("commit time: %w", err)
	}
	files, bad := parseNumstat(f[8])
	return Commit{
		Hash:        f[0],
		AuthorName:  f[1],
		AuthorEmail: f[2],
		AuthorTime:  at,
		CommitTime:  ct,
		Parents:     strings.Fields(f[5]),
		Subject:     f[6],
		Trailers:    parseTrailers(f[7]),
		Files:       files,
	}, bad, nil
}

func parseTrailers(s string) []Trailer {
	var out []Trailer
	for t := range strings.SplitSeq(s, trailerSep) {
		k, v, ok := strings.Cut(strings.TrimSpace(t), ":")
		if !ok {
			continue
		}
		out = append(out, Trailer{Key: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
	}
	return out
}

func parseNumstat(block string) ([]FileStat, int) {
	var files []FileStat
	bad := 0
	for line := range strings.SplitSeq(block, "\n") {
		if line == "" {
			continue
		}
		p := strings.SplitN(line, "\t", 3)
		if len(p) != 3 {
			bad++
			continue
		}
		if p[0] == "-" && p[1] == "-" {
			files = append(files, FileStat{Path: p[2], Binary: true})
			continue
		}
		a, errA := strconv.Atoi(p[0])
		d, errD := strconv.Atoi(p[1])
		if errA != nil || errD != nil {
			bad++
			continue
		}
		files = append(files, FileStat{Path: p[2], Added: a, Deleted: d})
	}
	return files, bad
}

var conventional = regexp.MustCompile(`^([A-Za-z]+)(\([^)]*\))?!?:`)

var knownTypes = map[string]bool{
	"feat": true, "fix": true, "refactor": true, "docs": true, "test": true,
	"chore": true, "perf": true, "ci": true, "build": true, "style": true, "revert": true,
}

// ClassifyType maps a subject to its conventional-commit type, "merge" or "other".
func ClassifyType(subject string, isMerge bool) string {
	if isMerge {
		return "merge"
	}
	if strings.HasPrefix(subject, "Revert \"") {
		return "revert"
	}
	m := conventional.FindStringSubmatch(subject)
	if m == nil {
		return "other"
	}
	t := strings.ToLower(m[1])
	if !knownTypes[t] {
		return "other"
	}
	return t
}

// agentPatterns maps a lowercase substring of a trailer value or author to an agent family.
var agentPatterns = []struct{ needle, agent string }{
	{"anthropic.com", "claude"},
	{"claude", "claude"},
	{"copilot", "copilot"},
	{"codex", "codex"},
	{"openai", "codex"},
	{"cursor", "cursor"},
	{"gemini", "gemini"},
	{"devin", "devin"},
	{"aider", "aider"},
}

var agentTrailerKeys = map[string]bool{"co-authored-by": true, "agent": true, "ai-agent": true, "generated-by": true, "assisted-by": true}

// DetectAgents returns the sorted agent families that participated in a commit.
func DetectAgents(c Commit) []string {
	seen := map[string]bool{}
	match := func(s string) {
		s = strings.ToLower(s)
		for _, p := range agentPatterns {
			if strings.Contains(s, p.needle) {
				seen[p.agent] = true
				return
			}
		}
	}
	for _, t := range c.Trailers {
		if agentTrailerKeys[strings.ToLower(t.Key)] {
			match(t.Value)
		}
	}
	if strings.HasSuffix(strings.ToLower(c.AuthorName), "[bot]") {
		match(c.AuthorName + " " + c.AuthorEmail)
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ModuleDir returns the first `depth` directory components of a file path.
func ModuleDir(path string, depth int) string {
	parts := strings.Split(path, "/")
	dirs := parts[:len(parts)-1]
	if len(dirs) == 0 {
		return "(root)"
	}
	if len(dirs) > depth {
		dirs = dirs[:depth]
	}
	return strings.Join(dirs, "/")
}

// Log runs git log on repoDir for commits reachable from revs but not from exclude.
// core.fsmonitor is disabled because repositories are not trusted.
func Log(ctx context.Context, repoDir string, revs, exclude []string, fn func(Commit) error) (Stats, error) {
	args := []string{"-C", repoDir, "-c", "core.quotepath=off", "-c", "core.fsmonitor=false",
		"log", "--no-renames", "--numstat", "--format=" + Format}
	args = append(args, revs...)
	for _, x := range exclude {
		args = append(args, "^"+x)
	}
	cmd := exec.CommandContext(ctx, "git", append(args, "--")...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Stats{}, fmt.Errorf("git log pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Stats{}, fmt.Errorf("start git log: %w", err)
	}
	st, perr := Parse(out, fn)
	if perr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return st, perr
	}
	if err := cmd.Wait(); err != nil {
		return st, fmt.Errorf("git log %s: %w: %s", repoDir, err, strings.TrimSpace(stderr.String()))
	}
	return st, nil
}
