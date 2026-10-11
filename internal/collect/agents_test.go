package collect

import (
	"testing"
	"time"

	"repolens/internal/transcript"
)

func TestMatcherPrefersPathThenEncodedName(t *testing.T) {
	m := &matcher{roots: []matchRoot{
		{path: "/home/u/projects/atlas", repoID: 1},
		{path: "/home/u/.herdr/worktrees/atlas/feat-x", repoID: 1, worktree: true},
		{path: "/home/u/projects/nb", repoID: 2},
	}}
	m.encoded = map[string]matchRoot{}
	for _, r := range m.roots {
		m.encoded[claudeEncode(r.path)] = r
	}
	cases := []struct {
		cwd, file string
		id        any
		wt        bool
	}{
		{"/home/u/projects/atlas/src", "", int64(1), false},
		{"/home/u/projects/atlas-other", "", nil, false},
		{"/home/u/.herdr/worktrees/atlas/feat-x", "", int64(1), true},
		{"/tmp/claude-1000/-home-u-projects-nb/abc/scratchpad/x", "", int64(2), false},
		{"/somewhere/else", "/home/u/.claude/projects/-home-u-projects-nb/s.jsonl", int64(2), false},
		{"/somewhere/else", "/home/u/.claude/projects/-home-u/s.jsonl", nil, false},
	}
	for _, tc := range cases {
		id, wt := m.match(tc.cwd, tc.file)
		if id != tc.id || wt != tc.wt {
			t.Errorf("match(%q,%q) = %v,%v want %v,%v", tc.cwd, tc.file, id, wt, tc.id, tc.wt)
		}
	}
	if got := claudeEncode("/home/u/.herdr/worktrees/atlas/feat-x"); got != "-home-u--herdr-worktrees-atlas-feat-x" {
		t.Errorf("encode = %q", got)
	}
}

// Audit P1: API history is fetched from the last sync (minus an overlap), never from the newest
// commit date, which a future-dated commit would push out of reach.
func TestHistorySinceUsesLastSync(t *testing.T) {
	sync := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if got := historySince(time.Time{}); !got.IsZero() {
		t.Errorf("first sync since = %v, want zero (everything)", got)
	}
	if got := historySince(sync); !got.Equal(sync.Add(-historyOverlap)) {
		t.Errorf("since = %v, want last sync minus overlap", got)
	}
}

func TestSummaryCountsTopLevelSessionsOnly(t *testing.T) {
	sessions := []*transcript.Session{{ID: "a"}, {ID: "b", Parent: "a"}, {ID: "c"}}
	if n := topLevel(sessions); n != 2 {
		t.Errorf("top-level sessions = %d, want 2", n)
	}
}
