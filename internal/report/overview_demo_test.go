package report_test

import (
	"context"
	"fmt"
	"testing"

	"repolens/internal/report"
)

func TestSummaryComparesWithThePreviousPeriod(t *testing.T) {
	since := demoNow.AddDate(0, 0, -30).Unix()
	s, err := report.Summary(context.Background(), demoDB, demoNow, since)
	if err != nil || s.Previous == nil {
		t.Fatalf("summary: %+v, %v", s, err)
	}
	end := demoNow.Unix() + 1
	prevFrom := since - (end - since)
	if want := one(t, `SELECT COUNT(*) FROM commits WHERE author_time>=? AND author_time<?`, since, end); int64(s.Current.Commits) != want {
		t.Errorf("current commits = %d, want %d", s.Current.Commits, want)
	}
	if want := one(t, `SELECT COUNT(*) FROM commits WHERE author_time>=? AND author_time<?`, prevFrom, since); int64(s.Previous.Commits) != want {
		t.Errorf("previous commits = %d, want %d", s.Previous.Commits, want)
	}
	if want := one(t, `SELECT COUNT(*) FROM agent_session WHERE parent_id IS NULL AND started_at>=? AND started_at<?`, prevFrom, since); int64(s.Previous.Sessions) != want {
		t.Errorf("previous sessions = %d, want %d", s.Previous.Sessions, want)
	}
	wantFresh := one(t, `SELECT SUM(m.input+m.output) FROM session_model m JOIN agent_session c ON c.id=m.session_id
		JOIN agent_session s ON s.id=COALESCE(c.parent_id, c.id) WHERE s.started_at>=? AND s.started_at<?`, since, end)
	if s.Current.Fresh != wantFresh || s.Current.Fresh >= s.Current.Tokens {
		t.Errorf("fresh tokens = %d, want %d (input + output, below all tokens %d)", s.Current.Fresh, wantFresh, s.Current.Tokens)
	}
	all, err := report.Summary(context.Background(), demoDB, demoNow, 0)
	if err != nil || all.Previous != nil {
		t.Errorf("all-time summary must have no previous period: %+v %v", all, err)
	}
}

func TestAttentionOnDemoData(t *testing.T) {
	items, err := report.Attention(context.Background(), demoDB, demoNow)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]report.AttentionItem{}
	for i, it := range items {
		kinds[it.Kind] = it
		if i > 0 && items[i-1].Severity == report.SeverityInfo && it.Severity != report.SeverityInfo {
			t.Error("items are not ordered by severity")
		}
	}
	// fjord's last CI run failed; Codex has no price; the legacy-import skill was never used.
	if ci, ok := kinds["ci"]; !ok || ci.RepoID != repoID(t, "fjord") {
		t.Errorf("failing CI not reported: %+v", items)
	}
	for _, k := range []string{"unpriced", "unused_config", "collect_errors"} {
		if _, ok := kinds[k]; !ok {
			t.Errorf("%s missing from %+v", k, items)
		}
	}
}

func TestSessionsFilterAndPage(t *testing.T) {
	ctx := context.Background()
	codex, err := report.Sessions(ctx, demoDB, report.SessionFilter{Tool: "codex", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := one(t, `SELECT COUNT(*) FROM agent_session WHERE tool='codex' AND parent_id IS NULL`); int64(codex.Total) != want || len(codex.Rows) != 10 {
		t.Errorf("codex: total %d (want %d), rows %d", codex.Total, want, len(codex.Rows))
	}
	next, err := report.Sessions(ctx, demoDB, report.SessionFilter{Tool: "codex", Limit: 10, Offset: 10})
	if err != nil || next.Rows[0].ID == codex.Rows[0].ID || next.Rows[0].StartedAt > codex.Rows[9].StartedAt {
		t.Errorf("second page overlaps the first: %v", err)
	}
	id := repoID(t, "atlas")
	opus, err := report.Sessions(ctx, demoDB, report.SessionFilter{RepoID: id, Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	want := one(t, `SELECT COUNT(DISTINCT s.id) FROM agent_session s JOIN session_model m ON m.session_id=s.id WHERE s.repo_id=? AND m.model='claude-opus-5-5'`, id)
	if int64(opus.Total) != want {
		t.Errorf("atlas opus sessions = %d, want %d", opus.Total, want)
	}
}

func TestPersonDetailAndPeriods(t *testing.T) {
	ctx := context.Background()
	since := demoNow.AddDate(0, 0, -60).Unix()
	people, err := report.PeopleSince(ctx, demoDB, since)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	var octo report.PersonRow
	for _, p := range people.People {
		total += int64(p.Commits)
		inRepos := 0
		for _, r := range p.Repos {
			inRepos += r.Commits
		}
		if inRepos != p.Commits {
			t.Errorf("%s: commits per repository %d != commits %d in the period", p.Name, inRepos, p.Commits)
		}
		if p.Name == "Octo Cat" {
			octo = p
		}
	}
	if want := one(t, `SELECT COUNT(*) FROM commits WHERE author_time>=?`, since); total != want {
		t.Errorf("people commits in period = %d, want %d", total, want)
	}
	d, err := report.PersonDetail(ctx, demoDB, octo.PersonID, since)
	if err != nil || d == nil {
		t.Fatalf("person detail: %v", err)
	}
	typed := 0
	for _, n := range d.Types {
		typed += n
	}
	if typed != octo.Commits || len(d.Recent) == 0 || len(d.ByRepo) == 0 {
		t.Errorf("types sum %d, commits %d, recent %d", typed, octo.Commits, len(d.Recent))
	}
	for _, c := range d.Recent {
		if c.Time < since || c.Repo == "" {
			t.Fatalf("recent commit outside the period or without repository: %+v", c)
		}
	}
	if missing, err := report.PersonDetail(ctx, demoDB, 99999, 0); err != nil || missing != nil {
		t.Errorf("unknown person = %v, %v", missing, err)
	}
}

func TestRepoDetailSinceAndOpenItems(t *testing.T) {
	ctx := context.Background()
	id := repoID(t, "beacon")
	since := demoNow.AddDate(0, 0, -30).Unix()
	d, err := report.RepoDetailSince(ctx, demoDB, demoNow, id, since)
	if err != nil {
		t.Fatal(err)
	}
	if want := one(t, `SELECT COUNT(*) FROM commits WHERE repo_id=? AND author_time>=?`, id, since); int64(d.Repo.Commits) != want {
		t.Errorf("commits in period = %d, want %d", d.Repo.Commits, want)
	}
	var authored int
	for _, a := range d.Authors {
		authored += a.Commits
	}
	if authored != d.Repo.Commits {
		t.Errorf("authors %d != commits %d", authored, d.Repo.Commits)
	}
	if want := one(t, `SELECT COUNT(*) FROM gh_issue WHERE repo_id=? AND state='OPEN'`, id); int64(len(d.Open)) != want {
		t.Errorf("open items = %d, want %d", len(d.Open), want)
	}
	for i := 1; i < len(d.Open); i++ {
		if d.Open[i].Created < d.Open[i-1].Created {
			t.Fatal("open items are not oldest first")
		}
	}
	if d.Repo.ScannedAt == 0 && d.Repo.InLocal {
		t.Log("demo data has no scan time; freshness is covered by the collect tests")
	}
}

func TestPortfolioTokensPerRepository(t *testing.T) {
	since := demoNow.AddDate(0, 0, -90).Unix()
	pf, err := report.PortfolioSince(context.Background(), demoDB, demoNow, since)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pf {
		q := `SELECT IFNULL(SUM(%s),0) FROM session_model m JOIN agent_session c ON c.id=m.session_id
			JOIN agent_session s ON s.id=COALESCE(c.parent_id, c.id) WHERE s.repo_id=? AND s.started_at>=?`
		fresh := one(t, fmt.Sprintf(q, "m.input+m.output"), r.ID, since)
		all := one(t, fmt.Sprintf(q, "m.input+m.output+m.cache_read+m.cache_write"), r.ID, since)
		if r.FreshTokens != fresh || r.Tokens != all {
			t.Errorf("%s: tokens %d/%d, want %d/%d", r.Name, r.FreshTokens, r.Tokens, fresh, all)
		}
	}
}
