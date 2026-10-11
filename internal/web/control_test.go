package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"repolens/internal/github"
	"repolens/internal/store"
)

const testHost = "127.0.0.1:8765"

type controlEnv struct {
	srv      *Server
	h        http.Handler
	cfgPath  string
	release  chan struct{}
	priceErr error
}

func newControlEnv(t *testing.T) *controlEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	env := &controlEnv{cfgPath: filepath.Join(t.TempDir(), "config.json"), release: make(chan struct{})}
	if err := os.WriteFile(env.cfgPath, []byte(`{"owners": ["github.com/octo"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env.srv = New(st.DB).WithControl(t.Context(), Control{
		ConfigPath: env.cfgPath,
		Collect: func(ctx context.Context, log io.Writer) error {
			fmt.Fprintln(log, "Skenuji /code…")
			fmt.Fprint(log, "  app +3 ")
			<-env.release
			fmt.Fprintln(log, "commitů")
			return errors.New("2 chyby")
		},
		GitHub: func(context.Context) GitHubStatus { return GitHubStatus{Source: "GITHUB_TOKEN"} },
		PriceList: func(context.Context) (*github.PriceList, error) {
			if env.priceErr != nil {
				return nil, env.priceErr
			}
			return &github.PriceList{Date: "2026-10-11", Prices: map[string]github.ModelPrice{"gpt-5.5": {Input: 5, Output: 30}, "unused-model": {Input: 1}}}, nil
		},
	})
	env.h = env.srv.Handler(map[string]bool{testHost: true})
	return env
}

func (e *controlEnv) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = testHost
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *controlEnv) trusted(extra map[string]string) map[string]string {
	h := map[string]string{"Origin": "http://" + testHost, "Sec-Fetch-Site": "same-origin", csrfHeader: e.srv.csrf, "Content-Type": "application/json"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func TestIndexCarriesCSRFTokenOnlyWhenServed(t *testing.T) {
	env := newControlEnv(t)
	body := env.do(http.MethodGet, "/", "", nil).Body.String()
	m := regexp.MustCompile(`<meta name="repolens-csrf" content="([0-9a-f]{64})">`).FindStringSubmatch(body)
	if m == nil || m[1] != env.srv.csrf {
		t.Fatalf("served index lacks the CSRF token")
	}
	page, err := renderPage(&Bundle{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), `<meta name="repolens-csrf"`) {
		t.Error("export must not carry a CSRF token")
	}
}

func TestWriteEndpointsRejectForeignRequests(t *testing.T) {
	env := newControlEnv(t)
	valid := `{"owners": ["github.com/octo"], "ci_days": 30}`
	cases := map[string]map[string]string{
		"no token":          env.trusted(map[string]string{csrfHeader: ""}),
		"wrong token":       env.trusted(map[string]string{csrfHeader: strings.Repeat("0", 64)}),
		"foreign origin":    env.trusted(map[string]string{"Origin": "http://evil.example"}),
		"cross-site fetch":  env.trusted(map[string]string{"Sec-Fetch-Site": "cross-site"}),
		"same-site fetch":   env.trusted(map[string]string{"Sec-Fetch-Site": "same-site"}),
		"null origin":       env.trusted(map[string]string{"Origin": "null"}),
		"https origin same": env.trusted(map[string]string{"Origin": "https://" + testHost}),
	}
	for name, hdr := range cases {
		if rec := env.do(http.MethodPut, "/api/settings", valid, hdr); rec.Code != http.StatusForbidden {
			t.Errorf("%s: PUT status %d, want 403", name, rec.Code)
		}
		if rec := env.do(http.MethodPost, "/api/collect", "", hdr); rec.Code != http.StatusForbidden {
			t.Errorf("%s: POST status %d, want 403", name, rec.Code)
		}
	}
	if rec := env.do(http.MethodPut, "/api/settings", valid, env.trusted(map[string]string{"Content-Type": "text/plain"})); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: status %d, want 415", rec.Code)
	}
	if b, _ := os.ReadFile(env.cfgPath); string(b) != `{"owners": ["github.com/octo"]}` {
		t.Errorf("config changed by a rejected request: %s", b)
	}
}

func TestSaveSettings(t *testing.T) {
	env := newControlEnv(t)
	rec := env.do(http.MethodPut, "/api/settings", `{"owners": ["github.com/octo"], "ci_days": 30}`, env.trusted(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(env.cfgPath); !strings.Contains(string(b), `"ci_days": 30`) {
		t.Errorf("config not written: %s", b)
	}
	rec = env.do(http.MethodPut, "/api/settings", `{"ci_days": -5, "github_accounts": ["../x"]}`, env.trusted(nil))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"text":"Historie CI musí být 1–{max} dní."`) {
		t.Errorf("invalid save: %d %s", rec.Code, rec.Body)
	}
	if rec := env.do(http.MethodPut, "/api/settings", `{"db": "/tmp/elsewhere.db"}`, env.trusted(nil)); rec.Code != http.StatusBadRequest {
		t.Errorf("non-editable key: status %d, want 400", rec.Code)
	}
	if rec := env.do(http.MethodPut, "/api/settings", `{"identities": [`+strings.Repeat(`{"name":"x"},`, 30000)+`{}]}`, env.trusted(nil)); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body: status %d, want 400", rec.Code)
	}
	if rec := env.do(http.MethodGet, "/api/settings", "", nil); !strings.Contains(rec.Body.String(), `"ci_days":30`) {
		t.Errorf("settings read back: %s", rec.Body)
	}
}

func TestReadOnlyServerHasNoWriteEndpoints(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := New(st.DB).Handler(map[string]bool{testHost: true})
	for _, p := range []string{"/api/settings", "/api/collect"} {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		req.Host = testHost
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d", p, rec.Code)
		}
	}
}

func TestCollectRunsOnceAndStreamsLog(t *testing.T) {
	env := newControlEnv(t)
	if rec := env.do(http.MethodPost, "/api/collect", "", env.trusted(nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	if rec := env.do(http.MethodPost, "/api/collect", "", env.trusted(nil)); rec.Code != http.StatusConflict {
		t.Errorf("second start: %d, want 409", rec.Code)
	}
	ts := httptest.NewServer(env.h)
	defer ts.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/collect/log", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var got []string
	released := false
	deadline := time.AfterFunc(5*time.Second, func() { resp.Body.Close() })
	defer deadline.Stop()
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			got = append(got, strings.TrimPrefix(line, "data: "))
		}
		if !released && len(got) == 1 {
			close(env.release) // the first line arrived while the run is still going
			released = true
		}
		if strings.HasPrefix(line, "event: done") {
			sc.Scan()
			got = append(got, strings.TrimPrefix(sc.Text(), "data: "))
			break
		}
	}
	want := []string{"Skenuji /code…", "  app +3 commitů"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || !strings.Contains(got[2], `"error":"2 chyby"`) {
		t.Fatalf("stream = %q", got)
	}
	if rec := env.do(http.MethodGet, "/api/collect", "", nil); !strings.Contains(rec.Body.String(), `"running":false`) {
		t.Errorf("state after run: %s", rec.Body)
	}
}

func TestAgentsPeriodAndSessionValidation(t *testing.T) {
	env := newControlEnv(t)
	for path, want := range map[string]int{
		"/api/agents":                     http.StatusOK,
		"/api/agents?days=90":             http.StatusOK,
		"/api/agents?days=all":            http.StatusOK,
		"/api/agents?days=7":              http.StatusBadRequest,
		"/api/agents?days=-1":             http.StatusBadRequest,
		"/api/session/claude:missing":     http.StatusNotFound,
		"/api/session/claude:..%2F..%2Fx": http.StatusBadRequest,
		"/api/session/shell:abc":          http.StatusBadRequest,
	} {
		if rec := env.do(http.MethodGet, path, "", nil); rec.Code != want {
			t.Errorf("%s: status %d, want %d", path, rec.Code, want)
		}
	}
}

func TestPeriodStart(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if s, err := periodStart("30", now); err != nil || s != now.AddDate(0, 0, -30).Unix() {
		t.Errorf("30 days = %d, %v", s, err)
	}
	if s, err := periodStart("", now); err != nil || s != 0 {
		t.Errorf("all = %d, %v", s, err)
	}
}

func TestExcludeAndAssignEndpoints(t *testing.T) {
	env := newControlEnv(t)
	if rec := env.do(http.MethodPost, "/api/exclude", `{"entry": "github.com/octo/gone"}`, env.trusted(nil)); rec.Code != http.StatusOK {
		t.Fatalf("exclude: %d %s", rec.Code, rec.Body)
	}
	env.do(http.MethodPost, "/api/exclude", `{"entry": "github.com/octo/gone"}`, env.trusted(nil))
	if b, _ := os.ReadFile(env.cfgPath); strings.Count(string(b), "github.com/octo/gone") != 1 || !strings.Contains(string(b), `"owners"`) {
		t.Errorf("config after exclude: %s", b)
	}
	if rec := env.do(http.MethodPost, "/api/exclude", `{"entry": "not a key"}`, env.trusted(nil)); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid exclusion: %d", rec.Code)
	}
	if rec := env.do(http.MethodPost, "/api/exclude", `{"entry": "/x"}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("exclude without CSRF: %d", rec.Code)
	}

	db := env.srv.db
	if _, err := db.Exec(`INSERT INTO repo(id,key,name) VALUES(3,'github.com/octo/app','app');
		INSERT INTO agent_session(id,tool,cwd,started_at,ended_at,source_file) VALUES
		('claude:a','claude','/old/app',1,2,'f'), ('claude:b','claude','/old/app/sub',1,2,'f'), ('claude:c','claude','/old/apple',1,2,'f')`); err != nil {
		t.Fatal(err)
	}
	rec := env.do(http.MethodPost, "/api/assign", `{"cwd": "/old/app", "repoId": 3}`, env.trusted(nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sessions":2`) {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body)
	}
	var unmatched int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_session WHERE repo_id IS NULL`).Scan(&unmatched); err != nil || unmatched != 1 {
		t.Errorf("unmatched after assign = %d, %v; /old/apple must stay", unmatched, err)
	}
	if b, _ := os.ReadFile(env.cfgPath); !strings.Contains(string(b), `"/old/app": "github.com/octo/app"`) {
		t.Errorf("mapping not saved: %s", b)
	}
	for body, want := range map[string]int{`{"cwd": "relative", "repoId": 3}`: 400, `{"cwd": "/", "repoId": 3}`: 400, `{"cwd": "/x", "repoId": 99}`: 404} {
		if rec := env.do(http.MethodPost, "/api/assign", body, env.trusted(nil)); rec.Code != want {
			t.Errorf("assign %s: %d, want %d", body, rec.Code, want)
		}
	}
}

func TestSessionListValidation(t *testing.T) {
	env := newControlEnv(t)
	for path, want := range map[string]int{
		"/api/sessions": http.StatusOK,
		"/api/sessions?tool=codex&repo=1&offset=10&limit=5&model=gpt-5.5&days=30": http.StatusOK,
		"/api/sessions?tool=bash":                         http.StatusBadRequest,
		"/api/sessions?offset=-1":                         http.StatusBadRequest,
		"/api/sessions?repo=x":                            http.StatusBadRequest,
		"/api/sessions?model=" + strings.Repeat("m", 101): http.StatusBadRequest,
		"/api/summary?days=90":                            http.StatusOK,
		"/api/attention":                                  http.StatusOK,
		"/api/person/1":                                   http.StatusNotFound,
		"/api/person/0":                                   http.StatusBadRequest,
		"/api/portfolio?days=7":                           http.StatusBadRequest,
		"/api/people?days=365":                            http.StatusOK,
	} {
		if rec := env.do(http.MethodGet, path, "", nil); rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}

func TestFetchPricesReturnsModelsInUse(t *testing.T) {
	env := newControlEnv(t)
	if _, err := env.srv.db.Exec(`INSERT INTO agent_session(id,tool,cwd,started_at,ended_at,source_file) VALUES('codex:a','codex','/x',1,2,'f');
		INSERT INTO session_model(session_id,model,requests,input,output,cache_read,cache_write,reasoning) VALUES('codex:a','gpt-5.5',1,1,1,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if rec := env.do(http.MethodPost, "/api/prices/fetch", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("without CSRF: %d", rec.Code)
	}
	rec := env.do(http.MethodPost, "/api/prices/fetch", "", env.trusted(nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"gpt-5.5":{"input":5,"output":30`) || strings.Contains(body, "unused-model") || !strings.Contains(body, `"date":"2026-10-11"`) {
		t.Errorf("fetch: %d %s", rec.Code, body)
	}
	env.priceErr = errors.New("rate limited")
	if rec := env.do(http.MethodPost, "/api/prices/fetch", "", env.trusted(nil)); rec.Code != http.StatusBadGateway {
		t.Errorf("failed fetch: %d", rec.Code)
	}
}
