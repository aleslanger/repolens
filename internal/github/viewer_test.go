package github

import (
	"net/http"
	"strings"
	"testing"
)

func TestViewerReadsLoginScopesAndLimits(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("auth header = %q", got)
		}
		switch r.URL.Path {
		case "/user":
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			w.Write([]byte(`{"login":"octo"}`))
		case "/rate_limit":
			w.Write([]byte(`{"resources":{"core":{"limit":5000,"remaining":4990,"reset":1800000000},"graphql":{"limit":5000,"remaining":4000,"reset":1800000060}}}`))
		default:
			http.NotFound(w, r)
		}
	})
	v, err := c.Viewer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v.Login != "octo" || strings.Join(v.Scopes, "|") != "repo|read:org" || v.Core.Remaining != 4990 || v.GraphQL.Limit != 5000 || v.GraphQL.Reset.Unix() != 1800000060 {
		t.Errorf("viewer = %+v", v)
	}
}

func TestViewerWithoutScopesHeader(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Write([]byte(`{"login":"octo"}`))
			return
		}
		w.Write([]byte(`{"resources":{}}`))
	})
	v, err := c.Viewer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v.Scopes != nil {
		t.Errorf("scopes = %v, want nil for a fine-grained token", v.Scopes)
	}
}

func TestTokenSourcePrefersEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", " "+testToken+"\n")
	tok, src, err := TokenSource(t.Context())
	if err != nil || tok != testToken || src != SourceEnv {
		t.Errorf("TokenSource: token matches = %v, source %q, err %v", tok == testToken, src, err)
	}
}
