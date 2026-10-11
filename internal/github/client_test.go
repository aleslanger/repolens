package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"repolens/internal/gitlog"
)

const testToken = "ghp_TESTTOKEN123"

type memCache map[string]Cached

func (m memCache) Get(_ context.Context, u string) (Cached, bool) {
	v, ok := m[u]
	return v, ok
}

func (m memCache) Put(_ context.Context, u string, c Cached) error {
	m[u] = c
	return nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newTestClient points a client at handler and records sleeps instead of sleeping.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(testToken, memCache{}, io.Discard)
	c.base = srv.URL
	slept := &[]time.Duration{}
	c.sleep = func(_ context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		return nil
	}
	return c, slept
}

// graphqlVars decodes the request and fails on queries with unbalanced braces or parentheses,
// which GitHub would reject as a syntax error.
func graphqlVars(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if err := checkBalanced(body.Query); err != nil {
		t.Errorf("malformed query: %v\n%s", err, body.Query)
	}
	return body.Variables
}

func checkBalanced(q string) error {
	var stack []rune
	pairs := map[rune]rune{'}': '{', ')': '('}
	for i, ch := range q {
		switch ch {
		case '{', '(':
			stack = append(stack, ch)
		case '}', ')':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[ch] {
				return fmt.Errorf("unexpected %q at %d", ch, i)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("%d unclosed", len(stack))
	}
	return nil
}

const emptyIssuesPage = `{"data":{"repository":{"%s":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`

func TestFetchIssuesFromRealFixture(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("missing bearer token")
		}
		if graphqlVars(t, r)["after"] != nil {
			fmt.Fprintf(w, emptyIssuesPage, "issues")
			return
		}
		w.Write(fixture(t, "issues_page1.json"))
	})
	issues, err := c.FetchIssues(context.Background(), Ref{"o", "r"}, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if calls, _ := c.Stats(); len(issues) != 30 || calls != 2 {
		t.Fatalf("issues=%d calls=%d, want 30 and 2 (second page empty)", len(issues), calls)
	}
	open, closed := 0, 0
	for _, i := range issues {
		if i.IsPR {
			t.Fatal("issue flagged as PR")
		}
		switch i.State {
		case "OPEN":
			open++
		case "CLOSED":
			closed++
			if i.ClosedAt == nil {
				t.Errorf("closed issue #%d without closedAt", i.Number)
			}
		}
	}
	if open+closed != 30 || open == 0 || closed == 0 {
		t.Errorf("open=%d closed=%d", open, closed)
	}

	// Incremental: stop at the first issue updated before `since`.
	since := issues[9].UpdatedAt.Add(time.Nanosecond)
	recent, err := c.FetchIssues(context.Background(), Ref{"o", "r"}, false, since)
	if err != nil {
		t.Fatal(err)
	}
	if calls, _ := c.Stats(); len(recent) != 9 || calls != 3 {
		t.Errorf("incremental issues=%d total calls=%d, want 9 and 3 (one more call)", len(recent), calls)
	}
}

func TestFetchPullRequestsFromRealFixture(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if graphqlVars(t, r)["after"] != nil {
			fmt.Fprintf(w, emptyIssuesPage, "pullRequests")
			return
		}
		w.Write(fixture(t, "prs_page1.json"))
	})
	prs, err := c.FetchIssues(context.Background(), Ref{"o", "r"}, true, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	merged := 0
	for _, p := range prs {
		if !p.IsPR {
			t.Fatal("PR not flagged")
		}
		if p.MergedAt != nil && p.State == "MERGED" {
			merged++
		}
		if p.AuthorLogin() == "" {
			t.Errorf("PR #%d without author", p.Number)
		}
	}
	if len(prs) != 30 || merged != 30 {
		t.Errorf("prs=%d merged=%d, want 30/30", len(prs), merged)
	}
}

func TestFetchMetaBatchesWithAliases(t *testing.T) {
	var ms, rel struct {
		Data struct {
			Repository json.RawMessage `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(fixture(t, "meta_milestones.json"), &ms); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture(t, "meta_releases.json"), &rel); err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		vars := graphqlVars(t, r)
		if vars["o0"] != "a" || vars["n1"] != "y" || vars["s0"] != nil || vars["s1"] != "1970-01-01T00:00:01Z" {
			t.Errorf("variables = %v", vars)
		}
		fmt.Fprintf(w, `{"data":{"r0":%s,"r1":%s}}`, ms.Data.Repository, rel.Data.Repository)
	})
	metas, _, err := c.FetchMeta(context.Background(), []MetaRequest{{Ref: Ref{"a", "x"}}, {Ref: Ref{"b", "y"}, Since: time.Unix(1, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	if calls, _ := c.Stats(); calls != 1 || len(metas) != 2 {
		t.Fatalf("calls=%d metas=%d, want one batched call for two repos", calls, len(metas))
	}
	m := metas[0]
	if len(m.Milestones.Nodes) == 0 || m.Milestones.Nodes[0].Open.TotalCount+m.Milestones.Nodes[0].Closed.TotalCount == 0 {
		t.Errorf("milestones not parsed: %+v", m.Milestones)
	}
	if !m.IsPrivate || m.DefaultBranch() == "" || len(m.Languages.Edges) == 0 {
		t.Errorf("meta fields not parsed: private=%v branch=%q langs=%d", m.IsPrivate, m.DefaultBranch(), len(m.Languages.Edges))
	}
	if len(metas[1].Releases.Nodes) != 2 || metas[1].Releases.Nodes[0].TagName == "" {
		t.Errorf("releases = %+v", metas[1].Releases.Nodes)
	}
}

func TestFetchMetaFailsLoudlyOnNonNotFoundErrors(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		graphqlVars(t, r)
		fmt.Fprint(w, `{"errors":[{"message":"Expected NAME, actual: (none)"}]}`)
	})
	// A query-level error affects every repository: none may be returned as fetched, and each
	// carries the reason instead of silently disappearing.
	metas, skipped, err := c.FetchMeta(context.Background(), []MetaRequest{{Ref: Ref{"a", "x"}}, {Ref: Ref{"a", "y"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range metas {
		if metas[i] != nil || !strings.Contains(skipped[i], "Expected NAME") {
			t.Errorf("repo %d: meta=%v reason=%q", i, metas[i], skipped[i])
		}
	}
}

func TestFetchMetaIsolatesMissingRepository(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		vars := graphqlVars(t, r)
		if _, batch := vars["o1"]; batch || vars["n0"] == "gone" {
			fmt.Fprint(w, `{"data":{"r0":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"r0":{"nameWithOwner":"a/ok"}}}`)
	})
	metas, _, err := c.FetchMeta(context.Background(), []MetaRequest{{Ref: Ref{"a", "ok"}}, {Ref: Ref{"a", "gone"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || metas[0] == nil || metas[0].NameWithOwner != "a/ok" || metas[1] != nil {
		t.Errorf("metas = %+v", metas)
	}
}

func TestFetchRunsPaginatesAndUsesETag(t *testing.T) {
	var base string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/o/r/actions/runs") {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"total_count":8,"workflow_runs":[]}`)
			return
		}
		if r.Header.Get("If-None-Match") == `W/"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `W/"v1"`)
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/actions/runs?per_page=100&page=2>; rel="next"`, base))
		w.Write(fixture(t, "runs_page1.json"))
	})
	base = c.base
	runs, err := c.FetchRuns(context.Background(), Ref{"o", "r"}, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if calls, _ := c.Stats(); len(runs) != 8 || calls != 2 {
		t.Fatalf("runs=%d calls=%d, want 8 and 2", len(runs), calls)
	}
	if runs[0].Conclusion == nil || *runs[0].Conclusion != "failure" || runs[0].Duration() != 3*time.Second {
		t.Errorf("first run = %+v duration %s", runs[0], runs[0].Duration())
	}
	again, err := c.FetchRuns(context.Background(), Ref{"o", "r"}, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, hits := c.Stats(); hits != 1 || len(again) != 8 {
		t.Errorf("cache hits=%d runs=%d, want 1 and 8", hits, len(again))
	}
	// Runs older than `since` stop the pagination.
	newer, err := c.FetchRuns(context.Background(), Ref{"o", "r"}, runs[2].CreatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(newer) != 2 {
		t.Errorf("since filter kept %d runs, want 2", len(newer))
	}
}

func TestRateLimitWaitsUntilReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	calls := 0
	c, slept := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("X-Ratelimit-Remaining", "0")
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(now.Unix()+30, 10))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"workflow_runs":[]}`)
	})
	c.now = func() time.Time { return now }
	if _, err := c.FetchRuns(context.Background(), Ref{"o", "r"}, now); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 31*time.Second {
		t.Errorf("slept %v, want [31s]", *slept)
	}
}

func TestRetriesHTMLErrorPage(t *testing.T) {
	calls := 0
	c, slept := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "<html>502</html>")
			return
		}
		if calls == 2 {
			fmt.Fprint(w, "<html>unicorn</html>") // 200 with HTML body, as seen from real GitHub
			return
		}
		fmt.Fprint(w, `{"data":{"viewer":{"login":"x"}}}`)
	})
	var out struct {
		Viewer struct{ Login string } `json:"viewer"`
	}
	if err := c.GraphQL(context.Background(), "{viewer{login}}", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Viewer.Login != "x" || len(*slept) != 2 {
		t.Errorf("login=%q sleeps=%d, want x and 2", out.Viewer.Login, len(*slept))
	}
}

func TestErrorsNeverContainToken(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	})
	_, err := c.FetchRuns(context.Background(), Ref{"o", "r"}, time.Now())
	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchHistoryFromRealFixture(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if graphqlVars(t, r)["after"] != nil {
			fmt.Fprint(w, `{"data":{"repository":{"defaultBranchRef":{"target":{"history":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}}`)
			return
		}
		w.Write(fixture(t, "history_page1.json"))
	})
	var commits []HistoryCommit
	err := c.FetchHistory(context.Background(), Ref{"o", "r"}, time.Time{}, func(h HistoryCommit) error {
		commits = append(commits, h)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	merges, withAgent, adds := 0, 0, 0
	for _, h := range commits {
		if h.IsMerge() {
			merges++
		}
		if len(gitlog.DetectAgents(h.Commit)) > 0 {
			withAgent++
		}
		adds += h.Additions
	}
	if len(commits) != 15 || merges != 6 || withAgent != 2 {
		t.Errorf("commits=%d merges=%d agent=%d, want 15/6/2", len(commits), merges, withAgent)
	}
	if commits[0].Additions != 140 || adds == 0 {
		t.Errorf("first additions=%d", commits[0].Additions)
	}
}

func TestTrailersFromMessage(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{"feat: x\n\nbody\n\nCo-Authored-By: Claude <noreply@anthropic.com>", 1},
		{"feat: x\n\nCo-authored-by: A <a@x>\nSigned-off-by: B <b@x>", 2},
		{"feat: x\n\nplain paragraph that: looks odd\nsecond line", 0},
		{"feat: x\n\nNote: this paragraph is prose\nand it continues here", 0},
		{"feat: only subject", 0},
	}
	for _, tc := range cases {
		if got := trailersFromMessage(tc.msg); len(got) != tc.want {
			t.Errorf("trailers(%q) = %v, want %d", tc.msg, got, tc.want)
		}
	}
}

func TestIssuePageSinceAndComplete(t *testing.T) {
	var d struct {
		Data struct {
			Repository struct {
				Issues IssuePage `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(fixture(t, "issues_page1.json"), &d); err != nil {
		t.Fatal(err)
	}
	p := d.Data.Repository.Issues
	if p.Complete(time.Time{}) {
		t.Error("a full page with a next page is not complete without a bound")
	}
	cut := p.Nodes[4].UpdatedAt.Add(time.Nanosecond)
	if !p.Complete(cut) {
		t.Error("page reaching past `since` is complete")
	}
	got := p.Since(cut, true)
	if len(got) != 4 || !got[0].IsPR {
		t.Errorf("Since kept %d items (want 4), IsPR=%v", len(got), len(got) > 0 && got[0].IsPR)
	}
}

func TestNextLinkStaysOnBase(t *testing.T) {
	base := "https://api.github.com"
	if got := nextLink(`<https://api.github.com/repos/o/r/actions/runs?page=2>; rel="next"`, base); got != "/repos/o/r/actions/runs?page=2" {
		t.Errorf("same host: %q", got)
	}
	if got := nextLink(`<https://evil.example/x>; rel="next"`, base); got != "" {
		t.Errorf("foreign host followed: %q", got)
	}
}

func TestRedirectToOtherHostIsRefused(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
	})
	if _, err := c.FetchRuns(context.Background(), Ref{"o", "r"}, time.Now()); err == nil || !strings.Contains(err.Error(), "odmítnuto") {
		t.Errorf("err = %v, want redirect refused", err)
	}
}

func TestIsFirstPage(t *testing.T) {
	cases := map[string]bool{"/repos/o/r/actions/runs?per_page=100": true, "/repos/o/r/actions/runs?per_page=100&page=2": false, "/x": true}
	for p, want := range cases {
		if got := isFirstPage(p); got != want {
			t.Errorf("isFirstPage(%q) = %v", p, got)
		}
	}
}

// Audit P1: an SSO-protected repository (FORBIDDEN) must not abort metadata for the others.
func TestFetchMetaIsolatesAnyPerRepoError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		vars := graphqlVars(t, r)
		if _, batch := vars["o1"]; batch || vars["n0"] == "sso" {
			fmt.Fprint(w, `{"data":{"r0":null},"errors":[{"type":"FORBIDDEN","message":"Resource protected by organization SAML enforcement"}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"r0":{"nameWithOwner":"a/ok"}}}`)
	})
	metas, skipped, err := c.FetchMeta(context.Background(), []MetaRequest{{Ref: Ref{"a", "ok"}}, {Ref: Ref{"a", "sso"}}})
	if err != nil {
		t.Fatal(err)
	}
	if metas[0] == nil || metas[1] != nil || !strings.Contains(skipped[1], "SAML") || skipped[0] != "" {
		t.Errorf("metas=%v skipped=%q", metas, skipped)
	}
}

func TestRedirectToPlainHTTPIsRefused(t *testing.T) {
	c := New("t", nil, io.Discard)
	req, _ := http.NewRequest(http.MethodGet, "http://api.github.com/x", nil)
	if err := c.sameHostOnly(req, nil); err == nil {
		t.Error("redirect to http:// accepted; the bearer token would travel in clear text")
	}
	req, _ = http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	if err := c.sameHostOnly(req, make([]*http.Request, maxRedirects)); err == nil {
		t.Error("redirect loop not bounded")
	}
}
