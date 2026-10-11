package collect

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"repolens/internal/config"
	"repolens/internal/github"
)

func TestLookupPricePrefersUserPricesAndExactListNames(t *testing.T) {
	cfg := config.Config{
		Prices:     map[string]config.Price{"gpt-5": {Input: 1}},
		ListPrices: map[string]config.Price{"gpt-5.5": {Input: 5}, "claude-opus-5": {Input: 4}, "claude": {Input: 99}},
	}
	for model, want := range map[string]float64{"gpt-5.5": 1, "claude-opus-5": 4} {
		if p, ok := lookupPrice(cfg, model); !ok || p.Input != want {
			t.Errorf("%s: %+v %v, want input %v", model, p, ok, want)
		}
	}
	if _, ok := lookupPrice(cfg, "claude-sonnet-9"); ok {
		t.Error("the fetched list must match exact names only, not prefixes")
	}
}

func TestRepriceFollowsPricesButKeepsReportedCosts(t *testing.T) {
	st := newStore(t)
	if _, err := st.DB.Exec(`INSERT INTO agent_session(id,tool,cwd,started_at,ended_at,source_file) VALUES('codex:a','codex','/x',1,2,'f'),('claude:b','claude','/x',1,2,'f');
		INSERT INTO session_model(session_id,model,requests,input,output,cache_read,cache_write,reasoning,cost_usd,cost_source) VALUES
		('codex:a','gpt-5.5',1,1000000,100000,2000000,0,0,NULL,''),
		('claude:b','claude-opus-5',1,1000000,0,0,0,0,7.0,'reported')`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ListPrices: map[string]config.Price{"gpt-5.5": {Input: 5, Output: 30, CacheRead: 0.5}, "claude-opus-5": {Input: 1}}}
	if err := Reprice(context.Background(), st, cfg); err != nil {
		t.Fatal(err)
	}
	cost := func(id string) (c *float64, src string) {
		if err := st.DB.QueryRow(`SELECT cost_usd, cost_source FROM session_model WHERE session_id=?`, id).Scan(&c, &src); err != nil {
			t.Fatal(err)
		}
		return c, src
	}
	// 1 M input × 5 + 0.1 M output × 30 + 2 M cache reads × 0.5 = 9 USD.
	if c, src := cost("codex:a"); c == nil || math.Abs(*c-9) > 1e-9 || src != "estimate" {
		t.Errorf("estimated cost = %v %q, want 9 estimate", c, src)
	}
	if c, src := cost("claude:b"); c == nil || *c != 7 || src != "reported" {
		t.Errorf("reported cost changed: %v %q", c, src)
	}
	// Without a price the estimate disappears instead of staying stale.
	if err := Reprice(context.Background(), st, config.Config{}); err != nil {
		t.Fatal(err)
	}
	if c, src := cost("codex:a"); c != nil || src != "" {
		t.Errorf("cost without a price = %v %q, want none", c, src)
	}
}

func TestPriceListIsFetchedOnlyWhenEnabledAndKeptOnFailure(t *testing.T) {
	fail := false
	files := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case fail:
			http.Error(w, "down", http.StatusNotFound)
		case r.URL.Path == "/repos/BerriAI/litellm/commits":
			w.Write([]byte(`[{"commit":{"committer":{"date":"2026-10-11T06:58:16Z"}}}]`))
		default:
			files++
			w.Write([]byte(`{"gpt-5.5": {"input_cost_per_token": 5e-06, "output_cost_per_token": 3e-05}}`))
		}
	}))
	defer srv.Close()
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "" })
	t.Setenv("GITHUB_TOKEN", "test-token")
	st := newStore(t)
	base := config.Config{DB: filepath.Join(t.TempDir(), "x.db")}
	opts := Options{Roots: []config.Root{{Path: t.TempDir(), Depth: 1}}, GitHub: true}
	if err := Run(context.Background(), st, base, opts, io.Discard); err != nil || files != 0 {
		t.Fatalf("disabled: err %v, file requests %d; want none", err, files)
	}
	on := base
	on.FetchPrices = true
	if err := Run(context.Background(), st, on, opts, io.Discard); err != nil || files != 1 {
		t.Fatalf("enabled: err %v, file requests %d", err, files)
	}
	var date string
	if err := st.DB.QueryRow(`SELECT value FROM meta WHERE key='price_list_date'`).Scan(&date); err != nil || date != "2026-10-11" {
		t.Errorf("list date = %q, %v", date, err)
	}
	fail = true
	sum := &Summary{}
	list, err := priceList(context.Background(), st, github.New("t", nil, io.Discard).WithBase(srv.URL), io.Discard, sum)
	if err != nil || sum.Errors != 1 || list["gpt-5.5"].Output != 30 {
		t.Errorf("failed fetch: list %v, errors %d, %v; want the stored list and one error", list, sum.Errors, err)
	}
}
