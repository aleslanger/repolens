package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"repolens/internal/report"
)

func TestRenderPageInlinesEverythingAndEscapesData(t *testing.T) {
	b := &Bundle{Portfolio: []report.PortfolioRow{{ID: 1, Name: "</script><script>alert(1)</script>"}}}
	page, err := renderPage(b)
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	for _, ref := range []string{`src="/static/`, `href="/static/`} {
		if strings.Contains(s, ref) {
			t.Errorf("export still references %s", ref)
		}
	}
	if strings.Contains(s, "</script><script>alert(1)") {
		t.Error("bundle data can break out of its script element")
	}
	if !strings.Contains(s, `id="bundle"`) || !strings.Contains(s, "window.Charts") || !strings.Contains(s, "RL.pages.repo") {
		t.Error("bundle or scripts missing from export")
	}
	if !strings.Contains(s, "connect-src 'none'") {
		t.Error("export lacks a CSP that blocks network access")
	}
	if strings.Index(s, `id="bundle"`) > strings.Index(s, "window.Charts") {
		t.Error("bundle must precede the scripts that read it")
	}
}

func TestGuardRejectsForeignHost(t *testing.T) {
	h := guard(map[string]bool{"127.0.0.1:1": true}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for host, want := range map[string]int{"127.0.0.1:1": http.StatusOK, "evil.example:1": http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %s: status %d, want %d", host, rec.Code, want)
		}
		if want == http.StatusOK && !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self';") {
			t.Error("CSP missing or allows inline scripts")
		}
	}
}
