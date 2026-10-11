package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"repolens/internal/report"
)

// Bundle is everything the dashboard needs, embedded into the static export.
type Bundle struct {
	GeneratedAt int64                        `json:"generatedAt"`
	Lang        string                       `json:"lang,omitempty"`
	Portfolio   []report.PortfolioRow        `json:"portfolio"`
	Repos       map[int64]*report.RepoDetail `json:"repos"`
	Agents      *report.AgentView            `json:"agents"`
	People      *report.PeopleView           `json:"people"`
	Status      *report.Status               `json:"status"`
	// Sessions holds the detail of every session listed on the agents page.
	Sessions map[string]*report.SessionView `json:"sessions"`
	// Persons, Attention and Summary back the person pages, the attention panel and the tiles.
	Persons   map[int64]*report.PersonView `json:"persons"`
	Attention []report.AttentionItem       `json:"attention"`
	Summary   *report.PeriodSummary        `json:"summary"`
	// Anonymized marks an export whose names, e-mails and paths were replaced (export --anonymize).
	Anonymized bool `json:"anonymized,omitempty"`
}

// BuildBundle runs every report once.
func BuildBundle(ctx context.Context, db *sql.DB, now time.Time) (*Bundle, error) {
	pf, err := report.Portfolio(ctx, db, now)
	if err != nil {
		return nil, err
	}
	b := &Bundle{GeneratedAt: now.Unix(), Portfolio: pf, Repos: map[int64]*report.RepoDetail{}}
	for _, p := range pf {
		d, err := report.RepoDetailFrom(ctx, db, p)
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", p.Key, err)
		}
		b.Repos[p.ID] = d
	}
	if b.Agents, err = report.AgentsFor(ctx, db, nil); err != nil {
		return nil, err
	}
	if b.People, err = report.People(ctx, db); err != nil {
		return nil, err
	}
	if b.Status, err = report.StatusFor(ctx, db); err != nil {
		return nil, err
	}
	if b.Attention, err = report.Attention(ctx, db, now); err != nil {
		return nil, err
	}
	if b.Summary, err = report.Summary(ctx, db, now, 0); err != nil {
		return nil, err
	}
	b.Persons = map[int64]*report.PersonView{}
	for _, p := range b.People.People {
		if p.PersonID == 0 {
			continue
		}
		if b.Persons[p.PersonID], err = report.PersonDetail(ctx, db, p.PersonID, 0); err != nil {
			return nil, err
		}
	}
	b.Sessions = map[string]*report.SessionView{}
	for _, s := range b.Agents.Sessions {
		if b.Sessions[s.ID], err = report.SessionDetail(ctx, db, s.ID); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Langs are the UI languages; the first is the default.
var Langs = []string{"cs", "en"}

// ExportOptions select the export language and whether identifying values are replaced.
type ExportOptions struct {
	Lang      string
	Anonymize bool
}

// Export writes a single self-contained HTML file that opens in opts.Lang (the viewer can switch).
func Export(ctx context.Context, db *sql.DB, path string, opts ExportOptions) error {
	lang := opts.Lang
	if !slices.Contains(Langs, lang) {
		return fmt.Errorf("neznámý jazyk %q (možnosti: %s)", lang, strings.Join(Langs, ", "))
	}
	b, err := BuildBundle(ctx, db, time.Now())
	if err != nil {
		return err
	}
	b.Lang = lang
	if opts.Anonymize {
		anonymize(b)
	}
	page, err := renderPage(b)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, page, 0o600); err != nil {
		return fmt.Errorf("write export: %w", err)
	}
	return nil
}

const exportCSP = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'none'; base-uri 'none'; form-action 'none'">`

// scripts are inlined in this order; it must match index.html.
var scripts = []string{"static/i18n.js", "static/charts.js", "static/app.js", "static/repo.js", "static/agents.js", "static/people.js", "static/settings.js", "static/collection.js"}

// renderPage returns index.html; with a bundle, CSS/JS are inlined and data embedded.
func renderPage(b *Bundle) ([]byte, error) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	if b == nil {
		return page, nil
	}
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		return nil, fmt.Errorf("read css: %w", err)
	}
	var js bytes.Buffer
	for _, f := range scripts {
		src, err := staticFS.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		js.Write(src)
		js.WriteByte('\n')
	}
	// json.Marshal escapes <, > and & so the payload cannot close the script element.
	data, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encode bundle: %w", err)
	}
	lang := Langs[0]
	if slices.Contains(Langs, b.Lang) {
		lang = b.Lang
	}
	r := strings.NewReplacer(
		`<html lang="cs">`, `<html lang="`+lang+`" data-lang="`+lang+`">`,
		// The file is opened from disk without server headers; the meta CSP forbids any network access.
		`<meta charset="utf-8">`, `<meta charset="utf-8">`+"\n"+exportCSP,
		`<link rel="stylesheet" href="/static/app.css">`, "<style>\n"+string(css)+"\n</style>",
		`<script src="/static/i18n.js"></script>`, "",
		`<script src="/static/charts.js"></script>`, "",
		`<script src="/static/settings.js"></script>`, "",
		`<script src="/static/collection.js"></script>`, "",
		`<script src="/static/repo.js"></script>`, "",
		`<script src="/static/agents.js"></script>`, "",
		`<script src="/static/people.js"></script>`, "",
		`<script src="/static/app.js"></script>`,
		`<script id="bundle" type="application/json">`+string(data)+"</script>\n<script>\n"+
			strings.ReplaceAll(js.String(), "</script", `<\/script`)+"\n</script>",
	)
	return []byte(r.Replace(string(page))), nil
}
