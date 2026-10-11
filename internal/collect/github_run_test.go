package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"repolens/internal/config"
)

// fakeGitHub answers the queries repolens sends, for account "octo": svc (GitHub only),
// app (also cloned locally) and a fork that must be skipped. It counts requests by kind.
type fakeGitHub struct {
	mu    sync.Mutex
	calls map[string]int
}

const fakeTime = "2026-09-01T10:00:00Z"

func metaJSON(name, desc string, fork bool) string {
	return fmt.Sprintf(`{"nameWithOwner":"octo/%s","description":%q,"isPrivate":false,"isFork":%v,"isArchived":false,
		"stargazerCount":7,"forkCount":1,"pushedAt":%q,"defaultBranchRef":{"name":"main"},"issues":{"totalCount":1},"pullRequests":{"totalCount":0},
		"languages":{"edges":[{"size":1200,"node":{"name":"Go"}}]},
		"milestones":{"nodes":[{"number":1,"title":"v1","state":"OPEN","dueOn":null,"open":{"totalCount":1},"closed":{"totalCount":2}}]},
		"labels":{"nodes":[{"name":"bug","color":"d73a4a"}]},
		"releases":{"nodes":[{"tagName":"v0.1.0","name":"first","publishedAt":%q,"isPrerelease":false}]},
		"recentIssues":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":3,"title":"Crash on start","state":"OPEN","createdAt":%q,"updatedAt":%q,"author":{"login":"mona"},"labels":{"nodes":[{"name":"bug"}]}}]},
		"recentPRs":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":4,"title":"Fix crash","state":"MERGED","createdAt":%q,"closedAt":%q,"mergedAt":%q,"updatedAt":%q,"author":{"login":"octo"},"labels":{"nodes":[]}}]}}`,
		name, desc, fork, fakeTime, fakeTime, fakeTime, fakeTime, fakeTime, fakeTime, fakeTime, fakeTime)
}

func (f *fakeGitHub) count(kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[kind]++
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/repos/octo/") && strings.HasSuffix(r.URL.Path, "/actions/runs") {
		f.count("runs")
		// Recent runs are re-read on every sync (they may have been in progress), so they must
		// be replaced, not duplicated.
		recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"total_count":2,"workflow_runs":[
			{"id":11,"name":"CI","event":"push","head_branch":"main","status":"completed","conclusion":"success","created_at":%q,"run_started_at":%q,"updated_at":"2026-09-01T10:02:00Z"},
			{"id":12,"name":"CI","event":"push","head_branch":"main","status":"completed","conclusion":"failure","created_at":%q,"run_started_at":%q,"updated_at":"2026-09-01T10:01:00Z"}]}`,
			recent, recent, fakeTime, fakeTime)
		return
	}
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if r.URL.Path != "/graphql" || json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "unexpected request "+r.URL.Path, http.StatusNotFound)
		return
	}
	switch {
	case strings.Contains(body.Query, "repositoryOwner"):
		f.count("list")
		fmt.Fprintf(w, `{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[%s,%s,%s]}}}}`,
			metaJSON("svc", "Service", false), metaJSON("app", "App", false), metaJSON("forked", "Fork", true))
	case strings.Contains(body.Query, "r0: repository"):
		f.count("meta")
		var parts []string
		for i := 0; ; i++ {
			name, ok := body.Variables[fmt.Sprintf("n%d", i)].(string)
			if !ok {
				break
			}
			parts = append(parts, fmt.Sprintf(`"r%d":%s`, i, metaJSON(name, "Desc of "+name, false)))
		}
		fmt.Fprintf(w, `{"data":{%s}}`, strings.Join(parts, ","))
	case strings.Contains(body.Query, "history("):
		f.count("history:" + body.Variables["name"].(string))
		fmt.Fprintf(w, `{"data":{"repository":{"defaultBranchRef":{"target":{"history":{"pageInfo":{"hasNextPage":false},"nodes":[
			{"oid":"a1","messageHeadline":"feat: add api","message":"feat: add api\n\nCo-Authored-By: Claude <noreply@anthropic.com>","authoredDate":%q,"additions":10,"deletions":2,"changedFilesIfAvailable":3,"parents":{"totalCount":1},"author":{"name":"Octo Cat","email":"octo@example.com"}},
			{"oid":"a2","messageHeadline":"fix: typo","message":"fix: typo","authoredDate":%q,"additions":1,"deletions":1,"changedFilesIfAvailable":1,"parents":{"totalCount":1},"author":{"name":"Mona","email":"mona@example.com"}}]}}}}}}`,
			fakeTime, "2026-09-02T10:00:00Z")
	default:
		http.Error(w, "unexpected query", http.StatusBadRequest)
	}
}

func TestRunCollectsClonesAndAccountRepositories(t *testing.T) {
	fake := &fakeGitHub{calls: map[string]int{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "" })
	t.Setenv("GITHUB_TOKEN", "test-token")

	root := t.TempDir()
	app := filepath.Join(root, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, app, "init", "-q", "-b", "main")
	commitFile(t, app, "main.go", "package main\n", "feat: local commit")
	git(t, app, "remote", "add", "origin", "https://github.com/octo/app.git")

	st := newStore(t)
	cfg := config.Config{DB: filepath.Join(t.TempDir(), "x.db"), ScanDepth: 2, ModuleDepth: 1, CIDays: 90,
		Owners: []string{"github.com/octo"}, ClaudeHome: t.TempDir(), CodexHome: t.TempDir()}
	opts := Options{Roots: []config.Root{{Path: root, Depth: 2}}, Accounts: []string{"octo"}, GitHub: true, Agents: true}
	var log strings.Builder
	if err := Run(context.Background(), st, cfg, opts, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}

	q := func(query string, args ...any) (n int) {
		t.Helper()
		if err := st.DB.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := q(`SELECT COUNT(*) FROM repo WHERE in_github=1`); n != 2 {
		t.Errorf("GitHub repositories = %d, want 2 (the fork is skipped)", n)
	}
	if n := q(`SELECT COUNT(*) FROM repo WHERE key='github.com/octo/app' AND in_local=1 AND stars=7`); n != 1 {
		t.Error("the cloned repository must get GitHub metadata and stay local")
	}
	// The clone's commits come from git; only the GitHub-only repository reads API history.
	if fake.calls["history:app"] != 0 || fake.calls["history:svc"] != 1 {
		t.Errorf("history calls = %v", fake.calls)
	}
	if n := q(`SELECT COUNT(*) FROM commits c JOIN repo r ON r.id=c.repo_id WHERE r.name='svc' AND c.source='github-api'`); n != 2 {
		t.Errorf("API commits = %d, want 2", n)
	}
	if n := q(`SELECT COUNT(*) FROM commits c JOIN repo r ON r.id=c.repo_id WHERE r.name='svc' AND c.agents<>''`); n != 1 {
		t.Errorf("trailer attribution from API messages = %d, want 1", n)
	}
	for table, want := range map[string]int{"gh_issue": 4, "gh_run": 4, "gh_milestone": 2, "gh_release": 2, "gh_label": 2, "gh_language": 2} {
		if n := q(`SELECT COUNT(*) FROM ` + table); n != want {
			t.Errorf("%s rows = %d, want %d", table, n, want)
		}
	}
	if n := q(`SELECT COUNT(*) FROM gh_run WHERE conclusion='failure'`); n != 2 {
		t.Errorf("failed runs = %d, want 2", n)
	}
	if n := q(`SELECT COUNT(*) FROM collect_run WHERE mode='all' AND finished_at IS NOT NULL AND repos>=2`); n != 1 {
		t.Errorf("collect_run not recorded:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "Souhrn:") {
		t.Errorf("summary missing from the log:\n%s", log.String())
	}

	// A second run is incremental: nothing is duplicated and the token never shows in the log.
	if err := Run(context.Background(), st, cfg, opts, &log); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := q(`SELECT COUNT(*) FROM gh_run`); n != 4 {
		t.Errorf("runs after a second sync = %d, want 4", n)
	}
	if n := q(`SELECT COUNT(*) FROM collect_run WHERE errors > 0`); n != 0 {
		t.Errorf("%d runs reported errors:\n%s", n, log.String())
	}
	if fake.calls["runs"] != 4 {
		t.Errorf("runs requests = %d, want 4 (both repositories, both syncs)", fake.calls["runs"])
	}
	if strings.Contains(log.String(), "test-token") {
		t.Error("the token leaked into the log")
	}
}

func TestOwnerModeWithoutTokenFails(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	st := newStore(t)
	cfg := config.Config{DB: filepath.Join(t.TempDir(), "x.db")}
	err := Run(context.Background(), st, cfg, Options{Accounts: []string{"octo"}, GitHub: true}, io.Discard)
	if err == nil {
		t.Fatal("a GitHub-only run without a token must fail")
	}
}

func TestExcludedRepositoryIsNotAskedFromGitHub(t *testing.T) {
	fake := &fakeGitHub{calls: map[string]int{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "" })
	t.Setenv("GITHUB_TOKEN", "test-token")
	st := newStore(t)
	cfg := config.Config{DB: filepath.Join(t.TempDir(), "x.db"), CIDays: 90, Exclude: []string{"github.com/octo/svc"}}
	if err := Run(context.Background(), st, cfg, Options{Accounts: []string{"octo"}, GitHub: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM repo WHERE name='svc'`).Scan(&n); err != nil || n != 0 || fake.calls["history:svc"] != 0 {
		t.Errorf("excluded repository stored (%d) or synced (%v)", n, fake.calls)
	}
}
