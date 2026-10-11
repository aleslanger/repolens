// Package web serves the dashboard on loopback and renders the single-file export.
package web

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"repolens/internal/report"
)

//go:embed static
var staticFS embed.FS

const (
	readHeaderTimeout = 5 * time.Second
	writeTimeout      = 60 * time.Second
	shutdownTimeout   = 5 * time.Second
)

type Server struct {
	db      *sql.DB
	now     func() time.Time
	ctl     *Control // nil: read-only dashboard
	csrf    string
	runs    *runner
	streams chan struct{} // semaphore for log streams
}

func New(db *sql.DB) *Server { return &Server{db: db, now: time.Now} }

// Handler builds the routes. Every response is local-only and carries a strict CSP.
func (s *Server) Handler(allowedHosts map[string]bool) http.Handler {
	mux := http.NewServeMux()
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Sprintf("embedded static dir missing: %v", err))
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/portfolio", s.json(func(r *http.Request) (any, error) {
		since, err := periodStart(r.URL.Query().Get("days"), s.now())
		if err != nil {
			return nil, err
		}
		return report.PortfolioSince(r.Context(), s.db, s.now(), since)
	}))
	mux.HandleFunc("GET /api/repo/{id}", s.json(func(r *http.Request) (any, error) {
		id, since, err := s.idAndPeriod(r)
		if err != nil {
			return nil, err
		}
		return found(report.RepoDetailSince(r.Context(), s.db, s.now(), id, since))
	}))
	mux.HandleFunc("GET /api/person/{id}", s.json(func(r *http.Request) (any, error) {
		id, since, err := s.idAndPeriod(r)
		if err != nil {
			return nil, err
		}
		return found(report.PersonDetail(r.Context(), s.db, id, since))
	}))
	mux.HandleFunc("GET /api/summary", s.json(func(r *http.Request) (any, error) {
		since, err := periodStart(r.URL.Query().Get("days"), s.now())
		if err != nil {
			return nil, err
		}
		return report.Summary(r.Context(), s.db, s.now(), since)
	}))
	mux.HandleFunc("GET /api/attention", s.json(func(r *http.Request) (any, error) {
		return report.Attention(r.Context(), s.db, s.now())
	}))
	mux.HandleFunc("GET /api/sessions", s.json(func(r *http.Request) (any, error) {
		f, err := sessionFilter(r, s.now())
		if err != nil {
			return nil, err
		}
		return report.Sessions(r.Context(), s.db, f)
	}))
	mux.HandleFunc("GET /api/agents", s.json(func(r *http.Request) (any, error) {
		since, err := periodStart(r.URL.Query().Get("days"), s.now())
		if err != nil {
			return nil, err
		}
		return report.AgentsSince(r.Context(), s.db, nil, since)
	}))
	mux.HandleFunc("GET /api/session/{id}", s.json(func(r *http.Request) (any, error) {
		id := r.PathValue("id")
		if !sessionID.MatchString(id) {
			return nil, errBadRequest
		}
		d, err := report.SessionDetail(r.Context(), s.db, id)
		if err == nil && d == nil {
			return nil, errNotFound
		}
		return d, err
	}))
	mux.HandleFunc("GET /api/people", s.json(func(r *http.Request) (any, error) {
		since, err := periodStart(r.URL.Query().Get("days"), s.now())
		if err != nil {
			return nil, err
		}
		return report.PeopleSince(r.Context(), s.db, since)
	}))
	mux.HandleFunc("GET /api/status", s.json(func(r *http.Request) (any, error) {
		return report.StatusFor(r.Context(), s.db)
	}))
	mux.HandleFunc("GET /api/lastrun", s.json(func(r *http.Request) (any, error) {
		return report.LastRun(r.Context(), s.db)
	}))
	if s.ctl != nil {
		s.controlRoutes(mux)
	}
	return guard(allowedHosts, mux)
}

// idAndPeriod reads a positive {id} and ?days= from the request.
func (s *Server) idAndPeriod(r *http.Request) (int64, int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, errBadRequest
	}
	since, err := periodStart(r.URL.Query().Get("days"), s.now())
	return id, since, err
}

// found turns a nil result of a lookup into 404.
func found[T any](v *T, err error) (any, error) {
	if err == nil && v == nil {
		return nil, errNotFound
	}
	return v, err
}

// toolNames are the agents whose sessions are stored; modelName bounds a model filter value.
var (
	toolNames = map[string]bool{"": true, "claude": true, "codex": true}
	modelName = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{0,100}$`)
)

// sessionFilter validates the query of the session list at the boundary.
func sessionFilter(r *http.Request, now time.Time) (report.SessionFilter, error) {
	q := r.URL.Query()
	since, err := periodStart(q.Get("days"), now)
	if err != nil {
		return report.SessionFilter{}, err
	}
	f := report.SessionFilter{Since: since, Tool: q.Get("tool"), Model: q.Get("model")}
	if !toolNames[f.Tool] || !modelName.MatchString(f.Model) {
		return f, errBadRequest
	}
	for name, dst := range map[string]*int{"offset": &f.Offset, "limit": &f.Limit} {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return f, errBadRequest
			}
			*dst = n
		}
	}
	if v := q.Get("repo"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return f, errBadRequest
		}
		f.RepoID = id
	}
	return f, nil
}

// periods are the selectable lengths of the period switch, in days; "" means everything.
var periods = map[string]int{"30": 30, "90": 90, "365": 365}

// sessionID is the shape of stored session ids: <tool>:<id from the transcript>.
var sessionID = regexp.MustCompile(`^(claude|codex):[A-Za-z0-9._-]{1,128}$`)

// periodStart turns ?days= into a unix start time; unknown values are rejected.
func periodStart(days string, now time.Time) (int64, error) {
	if days == "" || days == "all" {
		return 0, nil
	}
	n, ok := periods[days]
	if !ok {
		return 0, errBadRequest
	}
	return now.AddDate(0, 0, -n).Unix(), nil
}

var (
	errBadRequest = errors.New("bad request")
	errNotFound   = errors.New("not found")
)

func (s *Server) json(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		switch {
		case errors.Is(err, errBadRequest):
			http.Error(w, "neplatný požadavek", http.StatusBadRequest)
			return
		case errors.Is(err, errNotFound):
			http.Error(w, "nenalezeno", http.StatusNotFound)
			return
		case err != nil:
			log.Printf("api %s: %v", r.URL.Path, err)
			http.Error(w, "interní chyba", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("encode %s: %v", r.URL.Path, err)
		}
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	page, err := renderPage(nil)
	if err == nil && s.ctl != nil {
		// Only the served page carries the token; a cross-origin page cannot read it.
		page = bytes.Replace(page, []byte(`<meta charset="utf-8">`),
			[]byte(`<meta charset="utf-8">`+"\n"+`<meta name="repolens-csrf" content="`+s.csrf+`">`), 1)
	}
	if err != nil {
		log.Printf("render index: %v", err)
		http.Error(w, "interní chyba", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(page); err != nil {
		log.Printf("write index: %v", err)
	}
}

// guard rejects foreign Host headers (DNS rebinding) and sets security headers.
func guard(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "nepovolený host", http.StatusForbidden)
			return
		}
		// Other sites may not even trigger API calls (an <img> to /api/github would spend quota).
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "nepovolený původ požadavku", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Serve listens on 127.0.0.1:port until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, port int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	hosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", actual): true,
		fmt.Sprintf("localhost:%d", actual): true,
	}
	// Requests share ctx, so open log streams end when the server stops.
	srv := &http.Server{Handler: s.Handler(hosts), ReadHeaderTimeout: readHeaderTimeout, WriteTimeout: writeTimeout,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	fmt.Printf("repolens běží na http://127.0.0.1:%d\n", actual)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		// A collection started from the dashboard is cancelled by ctx; let it record its end.
		if s.runs != nil {
			s.runs.wait()
		}
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	}
}
