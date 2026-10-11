package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"repolens/internal/demo"
	"repolens/internal/report"
	"repolens/internal/store"
)

func demoStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := demo.Generate(context.Background(), st, time.Now()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestReadEndpointsOnDemoData(t *testing.T) {
	st := demoStore(t)
	h := New(st.DB).Handler(map[string]bool{testHost: true})
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = testHost
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	var pf []report.PortfolioRow
	if rec := get("/api/portfolio"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &pf) != nil || len(pf) != 7 {
		t.Fatalf("portfolio: %d %s", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}
	var agents report.AgentView
	if rec := get("/api/agents?days=90"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &agents) != nil || len(agents.Sessions) == 0 {
		t.Fatalf("agents: %d", rec.Code)
	}
	for path, want := range map[string]int{
		"/api/repo/1":                           http.StatusOK,
		"/api/repo/999":                         http.StatusNotFound,
		"/api/repo/x":                           http.StatusBadRequest,
		"/api/repo/-1":                          http.StatusBadRequest,
		"/api/people":                           http.StatusOK,
		"/api/status":                           http.StatusOK,
		"/api/lastrun":                          http.StatusOK,
		"/api/session/" + agents.Sessions[0].ID: http.StatusOK,
		"/static/app.js":                        http.StatusOK,
		"/":                                     http.StatusOK,
		"/api/settings":                         http.StatusNotFound, // read-only server
	} {
		rec := get(path)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
		if want == http.StatusOK && rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no CSP", path)
		}
	}
	// The read-only page has no CSRF token at all.
	if strings.Contains(get("/").Body.String(), `name="repolens-csrf"`) {
		t.Error("read-only server must not issue a CSRF token")
	}
}

func TestExportOfDemoData(t *testing.T) {
	st := demoStore(t)
	out := filepath.Join(t.TempDir(), "x.html")
	if err := Export(context.Background(), st.DB, out, ExportOptions{Lang: "cs"}); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)<script id="bundle" type="application/json">(.*?)</script>`).FindSubmatch(page)
	if m == nil {
		t.Fatal("no bundle in the export")
	}
	var b Bundle
	if err := json.Unmarshal(m[1], &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Portfolio) != 7 || len(b.Repos) != 7 || b.Agents == nil || b.People == nil || b.Status == nil {
		t.Fatalf("bundle incomplete: %d repos", len(b.Repos))
	}
	// Every listed session can be opened offline.
	for _, s := range b.Agents.Sessions {
		if b.Sessions[s.ID] == nil {
			t.Fatalf("session %s has no detail in the export", s.ID)
		}
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("export mode = %v, %v; want 0600 (it holds e-mails)", fi.Mode().Perm(), err)
	}
	if err := Export(context.Background(), st.DB, out, ExportOptions{Lang: "xx"}); err == nil {
		t.Error("unknown language accepted")
	}
}

func TestServeStopsWithContext(t *testing.T) {
	st := demoStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(st.DB).Serve(ctx, 0) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}
