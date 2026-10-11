package report

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"repolens/internal/store"
)

func trendStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustExec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.DB.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

const (
	monday = 1789344000 // 2026-09-14 00:00 UTC, a Monday
	week   = 7 * 86400
)

func TestUsageTrendGroupsWeeksAndFoldsTail(t *testing.T) {
	st := trendStore(t)
	// Seven skills in week 1 (the top six stay, the rarest folds into "other"), one in week 2.
	for i := range 7 {
		id := fmt.Sprintf("claude:s%d", i)
		mustExec(t, st, `INSERT INTO agent_session(id,tool,cwd,started_at,ended_at,source_file) VALUES(?,?,?,?,?,?)`, id, "claude", "/x", monday+3600, monday+7200, "f")
		mustExec(t, st, `INSERT INTO session_counter(session_id,kind,name,count) VALUES(?,?,?,?)`, id, "skill", fmt.Sprintf("sk%d", i), 10-i)
	}
	// Week 2's session starts on Wednesday; it belongs to the week of Monday 2026-09-21.
	wed := int64(monday + week + 2*86400 + 3600)
	mustExec(t, st, `INSERT INTO agent_session(id,tool,cwd,started_at,ended_at,source_file) VALUES('claude:w2','claude','/x',?,?,'f')`, wed, wed+3600)
	mustExec(t, st, `INSERT INTO session_counter(session_id,kind,name,count) VALUES('claude:w2','skill','sk0',5)`)
	// A child thread's counters belong to the parent's work and are counted too.
	mustExec(t, st, `INSERT INTO agent_session(id,tool,parent_id,cwd,started_at,ended_at,source_file) VALUES('codex:c','codex','claude:w2','/x',?,?,'f')`, monday+week+3700, monday+week+3800)
	mustExec(t, st, `INSERT INTO session_counter(session_id,kind,name,count) VALUES('codex:c','skill','sk0',1)`)

	v, err := AgentsFor(context.Background(), st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := v.Trends["skill"]
	if len(tr.Names) != 7 || tr.Names[0] != "sk0" || tr.Names[6] != otherName {
		t.Fatalf("names = %v, want six top skills then %q", tr.Names, otherName)
	}
	if len(tr.Weeks) != 2 || tr.Weeks[0] != "2026-09-14" || tr.Weeks[1] != "2026-09-21" {
		t.Fatalf("weeks = %v, want the Mondays 2026-09-14 and 2026-09-21", tr.Weeks)
	}
	if tr.Counts[0][0] != 10 || tr.Counts[0][1] != 6 || tr.Counts[6][0] != 4 {
		t.Errorf("week-1 counts = sk0 %d, sk0 week2 %d, other %d; want 10, 6, 4", tr.Counts[0][0], tr.Counts[0][1], tr.Counts[6][0])
	}
}

func TestSessionDurations(t *testing.T) {
	st := trendStore(t)
	mins := []int64{2, 10, 20, 45, 90, 180, 300, 3, 4, 12}
	for i, m := range mins {
		mustExec(t, st, `INSERT INTO agent_session(id,tool,cwd,started_at,ended_at,active_ms,source_file) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("claude:d%d", i), "claude", "/x", monday, monday+m*60, m*60000, "f")
	}
	v, err := AgentsFor(context.Background(), st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := v.Durations
	// Sorted: 2 3 4 10 12 20 45 90 180 300 minutes; nearest-rank median = 12, p90 = 180.
	if d.Sessions != 10 || d.MedianMs != 12*60000 || d.P90Ms != 180*60000 {
		t.Errorf("durations = %+v", d)
	}
	want := []int{3, 2, 1, 1, 1, 1, 1} // <5, 5–15, 15–30, 30–60, 1–2 h, 2–4 h, >4 h
	for i, b := range d.Buckets {
		if b.Sessions != want[i] {
			t.Errorf("bucket %s = %d, want %d", b.Label, b.Sessions, want[i])
		}
	}
}

func TestConfigTimeline(t *testing.T) {
	st := trendStore(t)
	mustExec(t, st, `INSERT INTO repo(id,key,name,in_local) VALUES(1,'local:/r','r',1)`)
	sep, oct := int64(1788220800), int64(1790899200) // 2026-09-01, 2026-10-02 UTC
	mustExec(t, st, `INSERT INTO agent_config(repo_id,scope,tool,kind,name,path,first_time,last_time,changes) VALUES
		(1,'project','claude','skill','a','p1',?,?,3),
		(1,'project','claude','skill','b','p2',?,?,1),
		(1,'project','claude','agent','c','p3',?,?,2),
		(NULL,'global','claude','skill','g','p4',NULL,NULL,0)`, sep, oct, oct, oct, sep, sep)
	v, err := AgentsFor(context.Background(), st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ConfigMonth{}
	for _, m := range v.ConfigTimeline {
		got[m.Month+"|"+m.Kind] = m
	}
	if got["2026-09|skill"].Created != 1 || got["2026-10|skill"].Created != 1 || got["2026-10|skill"].Changed != 1 ||
		got["2026-09|agent"].Created != 1 || got["2026-09|agent"].Changed != 0 || len(v.ConfigTimeline) != 3 {
		t.Errorf("timeline = %+v", v.ConfigTimeline)
	}
}

func TestDurationBucketBoundsAreExclusive(t *testing.T) {
	st := trendStore(t)
	for i, m := range []int64{5, 15, 240} {
		mustExec(t, st, `INSERT INTO agent_session(id,tool,cwd,started_at,ended_at,active_ms,source_file) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("claude:b%d", i), "claude", "/x", monday, monday+m*60, m*60000, "f")
	}
	v, err := AgentsFor(context.Background(), st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := v.Durations.Buckets
	// Exactly 5 min opens "5–15 min", exactly 15 min opens "15–30 min", exactly 4 h is "> 4 h".
	if b[0].Sessions != 0 || b[1].Sessions != 1 || b[2].Sessions != 1 || b[6].Sessions != 1 {
		t.Errorf("buckets = %+v", b)
	}
}
