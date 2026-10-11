package demo

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"repolens/internal/report"
	"repolens/internal/store"
)

var demoNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func generated(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := Generate(context.Background(), st, demoNow); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDemoFeedsEveryReport(t *testing.T) {
	st := generated(t)
	ctx := context.Background()
	pf, err := report.Portfolio(ctx, st.DB, demoNow)
	if err != nil || len(pf) != len(repos) {
		t.Fatalf("portfolio = %d rows, %v", len(pf), err)
	}
	av, err := report.AgentsFor(ctx, st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if av.Totals.Sessions < 100 || len(av.Trends["skill"].Names) == 0 || av.Durations.Sessions == 0 || len(av.ConfigTimeline) == 0 {
		t.Errorf("agents view too thin: sessions %d, trends %v", av.Totals.Sessions, av.Trends)
	}
	var inSession, trailer int
	if err := st.DB.QueryRow(`SELECT SUM(in_session), SUM(agents<>'') FROM commits`).Scan(&inSession, &trailer); err != nil {
		t.Fatal(err)
	}
	if inSession == 0 || trailer == 0 {
		t.Errorf("attribution not computed: in session %d, trailer %d", inSession, trailer)
	}
	people, err := report.People(ctx, st.DB)
	if err != nil {
		t.Fatal(err)
	}
	// "Octo Cat" and the noreply login "octocat" merge into one person.
	for _, p := range people.People {
		if p.Name == "octocat" {
			t.Errorf("noreply identity not merged: %+v", p.Identities)
		}
	}
	b, err := json.Marshal([]any{pf, av, people})
	if err != nil {
		t.Fatal(err)
	}
	// Every e-mail and path is invented: example.com / GitHub noreply addresses and /home/octo.
	for _, m := range regexp.MustCompile(`[\w.+\[\]-]+@[\w.-]+`).FindAllString(string(b), -1) {
		if !strings.HasSuffix(m, "@example.com") && !strings.HasSuffix(m, "@users.noreply.github.com") {
			t.Errorf("demo data contains a real-looking e-mail %q", m)
		}
	}
	for _, m := range regexp.MustCompile(`/home/[\w.-]+`).FindAllString(string(b), -1) {
		if m != "/home/octo" {
			t.Errorf("demo data contains the path %q", m)
		}
	}
}

func TestDemoIsDeterministicAndNeedsAnEmptyDatabase(t *testing.T) {
	count := func(st *store.Store) (n int) {
		if err := st.DB.QueryRow(`SELECT COUNT(*) + (SELECT COUNT(*) FROM agent_session) FROM commits`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	a, b := generated(t), generated(t)
	if count(a) != count(b) {
		t.Errorf("two generations differ: %d vs %d", count(a), count(b))
	}
	if err := Generate(context.Background(), a, demoNow); err == nil {
		t.Error("generating into a non-empty database must fail")
	}
}
