package github

import (
	"net/http"
	"testing"
)

func TestFetchPriceList(t *testing.T) {
	calls := map[string]int{}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/BerriAI/litellm/contents/model_prices_and_context_window.json":
			calls["file"]++
			if r.Header.Get("Accept") != acceptRaw {
				t.Errorf("accept = %q, want the raw media type", r.Header.Get("Accept"))
			}
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Write([]byte(`{
				"sample_spec": {"input_cost_per_token": "doc"},
				"gpt-5.5": {"input_cost_per_token": 5e-06, "output_cost_per_token": 3e-05, "cache_read_input_token_cost": 5e-07},
				"claude-opus-5": {"input_cost_per_token": 5e-06, "output_cost_per_token": 2.5e-05, "cache_read_input_token_cost": 5e-07, "cache_creation_input_token_cost": 6.25e-06},
				"azure/gpt-5.5": {"input_cost_per_token": 9e-06, "output_cost_per_token": 9e-05},
				"broken": {"input_cost_per_token": 5, "output_cost_per_token": 1},
				"embedding-only": {"input_cost_per_token": 1e-07}
			}`))
		case "/repos/BerriAI/litellm/commits":
			if r.URL.Query().Get("path") != "model_prices_and_context_window.json" {
				t.Errorf("commits query = %s", r.URL.RawQuery)
			}
			w.Write([]byte(`[{"commit":{"committer":{"date":"2026-10-11T06:58:16Z"}}}]`))
		default:
			http.NotFound(w, r)
		}
	})
	list, err := c.FetchPriceList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if list.Date != "2026-10-11" || len(list.Prices) != 2 {
		t.Fatalf("list = %+v, want two priced models dated 2026-10-11", list)
	}
	gpt := list.Prices["gpt-5.5"]
	if gpt.Input != 5 || gpt.Output != 30 || gpt.CacheRead != 0.5 || gpt.CacheWrite != 0 {
		t.Errorf("gpt-5.5 = %+v, want 5 / 30 / 0.5 / 0 USD per million", gpt)
	}
	if _, ok := list.Prices["azure/gpt-5.5"]; ok {
		t.Error("provider-prefixed prices must be skipped")
	}
	if list.Prices["claude-opus-5"].CacheWrite != 6.25 {
		t.Errorf("claude-opus-5 = %+v", list.Prices["claude-opus-5"])
	}
	// The second fetch is answered from the cache with a 304.
	if _, err := c.FetchPriceList(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls["file"] != 2 {
		t.Errorf("file requests = %d, want 2 (the second one conditional)", calls["file"])
	}
	if _, hits := c.Stats(); hits != 1 {
		t.Errorf("cache hits = %d, want 1", hits)
	}
}

func TestFetchPriceListRejectsEmptyList(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"sample_spec": {}}`)) })
	if _, err := c.FetchPriceList(t.Context()); err == nil {
		t.Error("a list without prices must be an error")
	}
}
