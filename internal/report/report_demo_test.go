package report_test

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"repolens/internal/demo"
	"repolens/internal/report"
	"repolens/internal/store"
)

// The demo database feeds every report; expected values are recomputed here from raw rows,
// independently of the report SQL.

var (
	demoDB  *sql.DB
	demoNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
)

func TestMain(m *testing.M) {
	os.Exit(runWithDemo(m))
}

func runWithDemo(m *testing.M) int {
	dir, err := os.MkdirTemp("", "repolens-report")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "demo.db"))
	if err != nil {
		panic(err)
	}
	defer st.Close()
	if err := demo.Generate(context.Background(), st, demoNow); err != nil {
		panic(err)
	}
	demoDB = st.DB
	return m.Run()
}

func ints(t *testing.T, q string, args ...any) []int64 {
	t.Helper()
	rows, err := demoDB.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func one(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	v := ints(t, q, args...)
	if len(v) != 1 {
		t.Fatalf("%s: %d rows", q, len(v))
	}
	return v[0]
}

func repoID(t *testing.T, name string) int64 {
	return one(t, `SELECT id FROM repo WHERE name=?`, name)
}

func TestRepoDetailAgreesWithRawRows(t *testing.T) {
	ctx := context.Background()
	id := repoID(t, "beacon")
	d, err := report.RepoDetailFor(ctx, demoDB, demoNow, id)
	if err != nil || d == nil {
		t.Fatalf("detail: %v", err)
	}
	var typed, added int64
	for _, x := range d.DailyTypes {
		typed += int64(x.Count)
	}
	for _, x := range d.DailyLines {
		added += int64(x.Additions)
	}
	if want := one(t, `SELECT COUNT(*) FROM commits WHERE repo_id=?`, id); typed != want || int64(d.Repo.Commits) != want {
		t.Errorf("commits by type = %d, repo = %d, want %d", typed, d.Repo.Commits, want)
	}
	if want := one(t, `SELECT SUM(additions) FROM commits WHERE repo_id=? AND is_merge=0`, id); added != want {
		t.Errorf("added lines = %d, want %d (merges excluded)", added, want)
	}
	var authored int64
	for _, a := range d.Authors {
		authored += int64(a.Commits)
	}
	if want := one(t, `SELECT COUNT(*) FROM commits WHERE repo_id=?`, id); authored != want {
		t.Errorf("author commits = %d, want %d", authored, want)
	}
	for i := 1; i < len(d.Recent); i++ {
		if d.Recent[i].Time > d.Recent[i-1].Time {
			t.Fatal("recent commits are not newest first")
		}
	}
	if len(d.Milestones) != 3 || len(d.Labels) != 5 || len(d.Languages) != 2 || len(d.Worktrees) != 1 {
		t.Errorf("milestones %d labels %d languages %d worktrees %d", len(d.Milestones), len(d.Labels), len(d.Languages), len(d.Worktrees))
	}
	var opened int64
	for _, w := range d.Issues {
		opened += int64(w.IssuesOpened + w.PRsOpened)
	}
	if want := one(t, `SELECT COUNT(*) FROM gh_issue WHERE repo_id=?`, id); opened != want {
		t.Errorf("opened issues+PRs over weeks = %d, want %d", opened, want)
	}
	if want := one(t, `SELECT COUNT(*) FROM gh_issue WHERE repo_id=? AND is_pr=0 AND state='OPEN'`, id); int64(d.IssueTimes.OpenIssues) != want {
		t.Errorf("open issues = %d, want %d", d.IssueTimes.OpenIssues, want)
	}
	// Median merge and close times from raw timestamps, for every repository (odd and even counts).
	even := false
	for _, rid := range ints(t, `SELECT id FROM repo WHERE in_github=1`) {
		rd, err := report.RepoDetailFor(ctx, demoDB, demoNow, rid)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			q   string
			got *float64
		}{
			{`SELECT merged_at - created_at FROM gh_issue WHERE repo_id=? AND is_pr=1 AND state='MERGED'`, rd.IssueTimes.MedianPRMergeHours},
			{`SELECT closed_at - created_at FROM gh_issue WHERE repo_id=? AND is_pr=0 AND state='CLOSED'`, rd.IssueTimes.MedianIssueCloseHours},
		} {
			var hours []float64
			for _, x := range ints(t, c.q, rid) {
				hours = append(hours, float64(x)/3600)
			}
			if len(hours) == 0 {
				continue
			}
			sort.Float64s(hours)
			want := hours[len(hours)/2]
			if len(hours)%2 == 0 {
				even = true
				want = (hours[len(hours)/2-1] + hours[len(hours)/2]) / 2
			}
			if c.got == nil || math.Abs(*c.got-want) > 1e-9 {
				t.Errorf("repo %d median = %v, want %v", rid, c.got, want)
			}
		}
	}
	if !even {
		t.Error("the demo data no longer has an even count; the even-median case is untested")
	}
	for _, c := range d.CI {
		succ := one(t, `SELECT COUNT(*) FROM gh_run WHERE repo_id=? AND workflow=? AND conclusion='success'`, id, c.Workflow)
		fail := one(t, `SELECT COUNT(*) FROM gh_run WHERE repo_id=? AND workflow=? AND conclusion='failure'`, id, c.Workflow)
		if c.SuccessRate == nil || math.Abs(*c.SuccessRate-float64(succ)/float64(succ+fail)) > 1e-9 {
			t.Errorf("%s success rate = %v, want %d/%d (cancelled runs do not count)", c.Workflow, c.SuccessRate, succ, succ+fail)
		}
	}
	if d.Agents == nil || d.Agents.Totals.Sessions != int(one(t, `SELECT COUNT(*) FROM agent_session WHERE repo_id=? AND parent_id IS NULL`, id)) {
		t.Error("repository agent view does not match its sessions")
	}
	if missing, err := report.RepoDetailFor(ctx, demoDB, demoNow, 9999); err != nil || missing != nil {
		t.Errorf("unknown repository = %v, %v", missing, err)
	}
}

func TestPortfolioFlagsAndSparks(t *testing.T) {
	pf, err := report.Portfolio(context.Background(), demoDB, demoNow)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]report.PortfolioRow{}
	for _, r := range pf {
		byName[r.Name] = r
	}
	if !byName["fjord"].Fork || !byName["ember"].Archived || byName["comet"].InLocal || !byName["notes"].InLocal || byName["notes"].InGitHub {
		t.Errorf("flags wrong: %+v", byName)
	}
	for name, r := range byName {
		if len(r.Spark) != 26 {
			t.Errorf("%s spark has %d weeks", name, len(r.Spark))
		}
	}
	// Sparklines cover the last 26 weeks of commits.
	var spark int64
	for _, v := range byName["atlas"].Spark {
		spark += int64(v)
	}
	since := demoNow.AddDate(0, 0, -26*7).Unix()
	if want := one(t, `SELECT COUNT(*) FROM commits WHERE repo_id=? AND author_time >= ?`, repoID(t, "atlas"), since); math.Abs(float64(spark-want)) > float64(want)/20 {
		t.Errorf("atlas spark sum = %d, want about %d", spark, want)
	}
}

func TestPeopleCoverEveryCommit(t *testing.T) {
	v, err := report.People(context.Background(), demoDB)
	if err != nil {
		t.Fatal(err)
	}
	var commits int64
	bots := 0
	for _, p := range v.People {
		commits += int64(p.Commits)
		if p.Bot {
			bots++
		}
	}
	if want := one(t, `SELECT COUNT(*) FROM commits`); commits != want {
		t.Errorf("people commits = %d, want %d", commits, want)
	}
	if bots == 0 {
		t.Error("dependabot and Copilot must be bots")
	}
}

func TestStatusAndLastRun(t *testing.T) {
	ctx := context.Background()
	s, err := report.StatusFor(ctx, demoDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Runs) != 4 || len(s.Issues) != 2 || s.Runs[0].StartedAt < s.Runs[1].StartedAt {
		t.Errorf("status = %d runs, %d issues", len(s.Runs), len(s.Issues))
	}
	last, err := report.LastRun(ctx, demoDB)
	if err != nil || last == nil || last.FinishedAt != one(t, `SELECT MAX(finished_at) FROM collect_run`) {
		t.Errorf("last run = %+v, %v", last, err)
	}
}

func TestEmptyDatabaseHasNoLastRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if last, err := report.LastRun(context.Background(), st.DB); err != nil || last != nil {
		t.Errorf("last run = %+v, %v; want nil", last, err)
	}
}
