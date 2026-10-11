package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"repolens/internal/config"
	"repolens/internal/github"
	"repolens/internal/report"
)

// Control lets the served dashboard change the configuration and start a collection. The export
// never has it. Collect runs one full collection as configured, writing progress to log.
type Control struct {
	ConfigPath string
	Collect    func(ctx context.Context, log io.Writer) error
	// GitHub reports the connection; nil uses the real token source and API.
	GitHub func(ctx context.Context) GitHubStatus
	// PriceList fetches the public price list through the GitHub API; nil uses the real one.
	PriceList func(ctx context.Context) (*github.PriceList, error)
}

// GitHubStatus is safe to send to the browser: it never contains the token.
type GitHubStatus struct {
	Source string         `json:"source,omitempty"`
	Viewer *github.Viewer `json:"viewer,omitempty"`
	Error  string         `json:"error,omitempty"`
}

const (
	csrfHeader      = "X-Repolens-CSRF"
	csrfBytes       = 32
	maxSettingsBody = 256 << 10
	githubTimeout   = 20 * time.Second
	sseHeartbeat    = 15 * time.Second
	// maxStreams bounds concurrent log streams; each holds a goroutine and a connection.
	maxStreams = 8
)

// WithControl enables the write endpoints. base bounds background collections (server lifetime).
func (s *Server) WithControl(base context.Context, c Control) *Server {
	token := make([]byte, csrfBytes)
	if _, err := rand.Read(token); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err)) // the platform RNG never fails on supported systems
	}
	s.ctl = &c
	s.csrf = hex.EncodeToString(token)
	s.runs = newRunner(base, c.Collect)
	s.streams = make(chan struct{}, maxStreams)
	if c.GitHub == nil {
		s.ctl.GitHub = realGitHubStatus
	}
	if c.PriceList == nil {
		s.ctl.PriceList = realPriceList
	}
	return s
}

func (s *Server) controlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings", s.json(func(r *http.Request) (any, error) {
		e, err := config.LoadEditable(s.ctl.ConfigPath)
		if err != nil {
			return nil, err
		}
		unpriced, err := report.UnpricedModels(r.Context(), s.db)
		return map[string]any{"path": s.ctl.ConfigPath, "values": e, "unpriced": unpriced}, err
	}))
	mux.Handle("PUT /api/settings", s.mutating(http.HandlerFunc(s.saveSettings)))
	mux.Handle("POST /api/exclude", s.mutating(http.HandlerFunc(s.addExclude)))
	mux.Handle("POST /api/assign", s.mutating(http.HandlerFunc(s.assignSessions)))
	mux.Handle("POST /api/prices/fetch", s.mutating(http.HandlerFunc(s.fetchPrices)))
	mux.HandleFunc("GET /api/github", s.json(func(r *http.Request) (any, error) {
		ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
		defer cancel()
		return s.ctl.GitHub(ctx), nil
	}))
	mux.HandleFunc("GET /api/collect", s.json(func(r *http.Request) (any, error) {
		return s.runs.state(), nil
	}))
	mux.Handle("POST /api/collect", s.mutating(http.HandlerFunc(s.startCollect)))
	mux.HandleFunc("GET /api/collect/log", s.streamLog)
}

// mutating admits a state-changing request only from the dashboard itself: same origin (when the
// browser says so) and the per-process CSRF token that only the served index page carries.
func (s *Server) mutating(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "nepovolený původ požadavku", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			http.Error(w, "nepovolený původ požadavku", http.StatusForbidden)
			return
		}
		got := r.Header.Get(csrfHeader)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.csrf)) != 1 {
			http.Error(w, "chybí nebo nesedí CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// decodeJSON reads a size-limited JSON body with no unknown keys; it answers the error itself.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "očekávám application/json", http.StatusUnsupportedMediaType)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "neplatný JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var e config.Editable
	if !decodeJSON(w, r, &e) {
		return
	}
	s.writeSaved(w, r, config.Save(s.ctl.ConfigPath, e))
}

// addExclude appends one directory or repository key to the exclusions ("ignore" at an error).
func (s *Server) addExclude(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Entry string `json:"entry"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	s.editSettings(w, r, func(e *config.Editable) {
		if !slices.Contains(e.Exclude, body.Entry) {
			e.Exclude = append(e.Exclude, body.Entry)
		}
	})
}

// assignSessions maps sessions of a directory that belongs to no repository to one. The mapping
// is saved (so later collections keep it) and applied to the stored sessions right away.
func (s *Server) assignSessions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD    string `json:"cwd"`
		RepoID int64  `json:"repoId"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	cwd := filepath.Clean(body.CWD)
	if !filepath.IsAbs(cwd) || cwd == string(filepath.Separator) {
		http.Error(w, "cesta musí být absolutní", http.StatusBadRequest)
		return
	}
	var key string
	if err := s.db.QueryRowContext(r.Context(), `SELECT key FROM repo WHERE id=?`, body.RepoID).Scan(&key); err != nil {
		http.Error(w, "repozitář nenalezen", http.StatusNotFound)
		return
	}
	errs := s.updateSettings(func(e *config.Editable) {
		if e.SessionPaths == nil {
			e.SessionPaths = map[string]string{}
		}
		e.SessionPaths[cwd] = key
	})
	if len(errs) > 0 {
		s.writeSaved(w, r, errs)
		return
	}
	res, err := s.db.ExecContext(r.Context(), `UPDATE agent_session SET repo_id=? WHERE repo_id IS NULL
		AND (cwd=? OR substr(cwd, 1, length(?)+1)=?||'/')`, body.RepoID, cwd, cwd, cwd)
	if err != nil {
		log.Printf("assign sessions: %v", err)
		http.Error(w, "interní chyba", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected() // SQLite always reports it
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeJSON(w, r, map[string]any{"saved": true, "sessions": n})
}

// editSettings changes the saved settings with fn and answers like a settings save.
func (s *Server) editSettings(w http.ResponseWriter, r *http.Request, fn func(*config.Editable)) {
	s.writeSaved(w, r, s.updateSettings(fn))
}

func (s *Server) updateSettings(fn func(*config.Editable)) []error {
	e, err := config.LoadEditable(s.ctl.ConfigPath)
	if err != nil {
		return []error{err}
	}
	fn(&e)
	return config.Save(s.ctl.ConfigPath, e)
}

// writeSaved answers a save: 200, or 422 with translatable problems.
func (s *Server) writeSaved(w http.ResponseWriter, r *http.Request, errs []error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if len(errs) > 0 {
		msgs := make([]*config.Problem, len(errs))
		for i, err := range errs {
			var p *config.Problem
			if !errors.As(err, &p) {
				p = &config.Problem{Text: err.Error()}
			}
			msgs[i] = p
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(w, r, map[string]any{"errors": msgs})
		return
	}
	writeJSON(w, r, map[string]any{"saved": true})
}

func (s *Server) startCollect(w http.ResponseWriter, r *http.Request) {
	if !s.runs.start() {
		http.Error(w, "sběr už běží", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, r, s.runs.state())
}

// streamLog sends the run's log as server-sent events: every line so far, then new ones, then a
// "done" event. The write deadline is extended per event because a run outlasts writeTimeout.
func (s *Server) streamLog(w http.ResponseWriter, r *http.Request) {
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		http.Error(w, "příliš mnoho otevřených logů", http.StatusServiceUnavailable)
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	next := 0
	for {
		lines, end, done, changed := s.runs.since(next)
		if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Printf("sse deadline: %v", err)
			return
		}
		for _, l := range lines {
			if _, err := fmt.Fprintf(w, "event: line\ndata: %s\n\n", l); err != nil {
				return
			}
		}
		next = end
		if done != nil {
			b, err := json.Marshal(done)
			if err != nil {
				log.Printf("sse done: %v", err)
				return
			}
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", b)
			rc.Flush()
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-heartbeat.C:
			// A comment line keeps idle connections and proxies alive and notices gone clients.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode %s: %v", r.URL.Path, err)
	}
}

// fetchPrices returns the public price list for the models seen in sessions, so the Settings
// page can pre-fill the form; nothing is saved until the user saves.
func (s *Server) fetchPrices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()
	list, err := s.ctl.PriceList(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	used := map[string]github.ModelPrice{}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT model FROM session_model`)
	if err != nil {
		log.Printf("price models: %v", err)
		http.Error(w, "interní chyba", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			log.Printf("price models: %v", err)
			http.Error(w, "interní chyba", http.StatusInternalServerError)
			return
		}
		if p, ok := list.Prices[m]; ok {
			used[m] = p
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeJSON(w, r, map[string]any{"date": list.Date, "source": github.PriceListSource, "prices": used})
}

func realPriceList(ctx context.Context) (*github.PriceList, error) {
	token, _, err := github.TokenSource(ctx)
	if err != nil {
		return nil, err
	}
	return github.New(token, nil, io.Discard).FetchPriceList(ctx)
}

func realGitHubStatus(ctx context.Context) GitHubStatus {
	token, source, err := github.TokenSource(ctx)
	if err != nil {
		return GitHubStatus{Error: err.Error()}
	}
	v, err := github.New(token, nil, io.Discard).Viewer(ctx)
	if err != nil {
		return GitHubStatus{Source: source, Error: err.Error()}
	}
	return GitHubStatus{Source: source, Viewer: &v}
}
