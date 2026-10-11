package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func finishedRunner(t *testing.T, collect func(context.Context, io.Writer) error) *runner {
	t.Helper()
	r := newRunner(t.Context(), collect)
	if !r.start() {
		t.Fatal("start refused")
	}
	r.wait()
	return r
}

// A client that connects after old lines were dropped continues from the absolute index and
// never receives a line twice.
func TestSinceUsesAbsoluteIndexAfterDrop(t *testing.T) {
	r := finishedRunner(t, func(_ context.Context, w io.Writer) error {
		for i := range maxLogLines + 10 {
			fmt.Fprintf(w, "line %d\n", i)
		}
		return nil
	})
	lines, end, _, _ := r.since(0)
	if len(lines) != maxLogLines || lines[0] != "line 10" || end != maxLogLines+10 {
		t.Fatalf("first read: %d lines from %q, end %d", len(lines), lines[0], end)
	}
	if again, _, _, _ := r.since(end); len(again) != 0 {
		t.Errorf("re-read after end returned %d lines", len(again))
	}
}

func TestPanickingCollectorBecomesAnError(t *testing.T) {
	r := finishedRunner(t, func(context.Context, io.Writer) error { panic("boom") })
	if s := r.state(); s.Running || !strings.Contains(s.Error, "boom") {
		t.Errorf("state = %+v", s)
	}
}

// Output without a newline is flushed as a line once it is long, while the run is still going.
func TestUnterminatedOutputIsBounded(t *testing.T) {
	written, release := make(chan struct{}), make(chan struct{})
	r := newRunner(t.Context(), func(_ context.Context, w io.Writer) error {
		_, err := w.Write([]byte(strings.Repeat("x", 3*maxLineBytes)))
		close(written)
		<-release
		return err
	})
	r.start()
	<-written
	lines, _, _, _ := r.since(0)
	r.mu.Lock()
	partial := len(r.partial)
	r.mu.Unlock()
	close(release)
	r.wait()
	if len(lines) != 1 || len(lines[0]) != 3*maxLineBytes || partial != 0 {
		t.Errorf("during the run: lines = %d, partial = %d", len(lines), partial)
	}
}

func TestCrossSiteAPIRequestsAreRefused(t *testing.T) {
	env := newControlEnv(t)
	for site, want := range map[string]int{"cross-site": http.StatusForbidden, "same-origin": http.StatusOK, "": http.StatusOK} {
		req := httptest.NewRequest(http.MethodGet, "/api/github", nil)
		req.Host = testHost
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		rec := httptest.NewRecorder()
		env.h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Sec-Fetch-Site %q: status %d, want %d", site, rec.Code, want)
		}
	}
}
