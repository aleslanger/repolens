// Package config loads repolens settings from a JSON file. Secrets never live here.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"repolens/internal/privacy"
	"repolens/internal/scan"
)

// Identity groups the names, e-mails and GitHub logins of one person.
type Identity struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
}

// Price is USD per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Root is a directory scanned for git repositories.
type Root struct {
	Path  string `json:"path"`
	Depth int    `json:"depth,omitempty"`
}

type Config struct {
	// Roots are scanned by `repolens collect` without arguments.
	Roots []Root `json:"roots"`
	// GitHubAccounts are users or organisations whose repositories are collected, including
	// ones without a local clone (see IncludeUncloned). Empty means the github.com owners.
	GitHubAccounts  []string `json:"github_accounts"`
	IncludeUncloned *bool    `json:"include_uncloned,omitempty"`
	DB              string   `json:"db"`
	ScanDepth       int      `json:"scan_depth"`
	ModuleDepth     int      `json:"module_depth"`
	// Owners lists "host/owner" prefixes whose repositories count as own.
	// Empty means "the authenticated GitHub user" when a token is available.
	Owners          []string         `json:"owners"`
	IncludeForks    bool             `json:"include_forks"`
	IncludeArchived bool             `json:"include_archived"`
	Identities      []Identity       `json:"identities"`
	Prices          map[string]Price `json:"prices"`
	CIDays          int              `json:"ci_days"`
	// Exclude lists directories that are not scanned and repository keys that are not asked
	// from GitHub (their local statistics stay).
	Exclude []string `json:"exclude"`
	// SessionPaths maps a session working directory (prefix) to a repository key, for sessions
	// whose directory belongs to no repository (moved or deleted clones).
	SessionPaths map[string]string `json:"session_paths"`
	// PricesDate is the date the price list is valid for (YYYY-MM-DD), shown with cost estimates.
	PricesDate string `json:"prices_date"`
	// FetchPrices fills prices missing from Prices with the public LiteLLM list, read through the
	// GitHub API. Off by default; Prices always win.
	FetchPrices bool `json:"fetch_prices"`
	// ListPrices is the fetched list in use (set by the collector, never read from the file).
	ListPrices map[string]Price `json:"-"`
	ClaudeHome string           `json:"claude_home"`
	CodexHome  string           `json:"codex_home"`
}

const (
	maxConfigSize      = 1 << 20
	defaultScanDepth   = 4
	defaultModuleDepth = 2
	defaultCIDays      = 90
)

// DefaultPath is ~/.config/repolens/config.json.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "repolens.json"
	}
	return filepath.Join(dir, "repolens", "config.json")
}

// Load reads path; a missing file yields defaults.
func Load(path string) (Config, error) {
	cfg := Config{}
	b, err := privacy.ReadFile(path, maxConfigSize)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return cfg, fmt.Errorf("read config: %w", err)
	default:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	return withDefaults(cfg)
}

func withDefaults(cfg Config) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg, fmt.Errorf("home dir: %w", err)
	}
	if cfg.DB == "" {
		cfg.DB = filepath.Join(home, ".local", "share", "repolens", "repolens.db")
	}
	if cfg.ScanDepth <= 0 {
		cfg.ScanDepth = defaultScanDepth
	}
	if cfg.ModuleDepth <= 0 {
		cfg.ModuleDepth = defaultModuleDepth
	}
	if cfg.CIDays == 0 { // a negative value is left for Validate to report
		cfg.CIDays = defaultCIDays
	}
	if cfg.ClaudeHome == "" {
		cfg.ClaudeHome = filepath.Join(home, ".claude")
	}
	if cfg.CodexHome == "" {
		cfg.CodexHome = filepath.Join(home, ".codex")
	}
	cfg.DB = expand(cfg.DB, home)
	cfg.ClaudeHome = expand(cfg.ClaudeHome, home)
	cfg.CodexHome = expand(cfg.CodexHome, home)
	for i, o := range cfg.Owners {
		cfg.Owners[i] = strings.ToLower(strings.Trim(o, "/"))
	}
	for i := range cfg.Roots {
		cfg.Roots[i].Path = expand(cfg.Roots[i].Path, home)
		if cfg.Roots[i].Depth == 0 { // a negative depth is left for Validate to report
			cfg.Roots[i].Depth = cfg.ScanDepth
		}
	}
	if len(cfg.GitHubAccounts) == 0 {
		for _, o := range cfg.Owners {
			if login, ok := strings.CutPrefix(o, githubHost+"/"); ok {
				cfg.GitHubAccounts = append(cfg.GitHubAccounts, login)
			}
		}
	}
	for i, a := range cfg.GitHubAccounts {
		cfg.GitHubAccounts[i] = strings.ToLower(strings.TrimSpace(a))
	}
	normalizeLists(&cfg, home)
	return cfg, nil
}

const githubHost = "github.com"

// Uncloned reports whether repositories of the GitHub accounts without a local clone are
// collected too (default: yes).
func (c Config) Uncloned() bool { return c.IncludeUncloned == nil || *c.IncludeUncloned }

const (
	maxScanDepth = 10
	maxCIDays    = 3650
)

var (
	githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	ownerSpec   = regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]{2,}/[A-Za-z0-9_.-]+$`)
)

// Problem is a validation error whose message the dashboard can translate: Text is the Czech
// template with {placeholders}, Args fills them.
type Problem struct {
	Text string         `json:"text"`
	Args map[string]any `json:"args,omitempty"`
}

func (p *Problem) Error() string {
	out := p.Text
	for k, v := range p.Args {
		out = strings.ReplaceAll(out, "{"+k+"}", fmt.Sprint(v))
	}
	return out
}

func problem(text string, kv ...any) error {
	args := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		args[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return &Problem{Text: text, Args: args}
}

// Validate checks values entered by the user (Settings page); every problem is reported.
func (c Config) Validate() []error {
	var errs []error
	for _, r := range c.Roots {
		if p := rootProblem(r.Path); p != nil {
			errs = append(errs, p)
			continue
		}
		st, err := os.Stat(r.Path)
		switch {
		case err != nil:
			errs = append(errs, problem("Adresář {path} neexistuje nebo není čitelný.", "path", r.Path))
		case !st.IsDir():
			errs = append(errs, problem("{path} není adresář.", "path", r.Path))
		}
		if r.Depth < 1 || r.Depth > maxScanDepth {
			errs = append(errs, problem("Hloubka {depth} u {path} musí být 1–{max}.", "depth", r.Depth, "path", r.Path, "max", maxScanDepth))
		}
	}
	for _, a := range c.GitHubAccounts {
		if !githubLogin.MatchString(a) {
			errs = append(errs, problem("„{account}“ není platný GitHub účet.", "account", a))
		}
	}
	for _, o := range c.Owners {
		if !ownerSpec.MatchString(o) {
			errs = append(errs, problem("Vlastník „{owner}“ musí mít tvar host/účet, např. github.com/octo.", "owner", o))
		}
	}
	if c.CIDays < 1 || c.CIDays > maxCIDays {
		errs = append(errs, problem("Historie CI musí být 1–{max} dní.", "max", maxCIDays))
	}
	for model, p := range c.Prices {
		if !validPrice(p.Input) || !validPrice(p.Output) || !validPrice(p.CacheRead) || !validPrice(p.CacheWrite) {
			errs = append(errs, problem("Ceny modelu {model} musí být 0–{max} USD za milion tokenů.", "model", model, "max", maxPrice))
		}
	}
	return append(errs, c.validateLists()...)
}

// maxPrice bounds USD per million tokens; larger values would overflow cost sums to +Inf.
const maxPrice = 1e6

func validPrice(v float64) bool { return v >= 0 && v <= maxPrice && !math.IsNaN(v) }

// rootProblem refuses roots that are relative, the filesystem root, or (also through a symlink)
// a path the privacy denylist forbids: the scanner never checks the root itself.
func rootProblem(path string) error {
	if !filepath.IsAbs(path) {
		return problem("Adresář {path} musí být absolutní cesta (nebo začínat ~/).", "path", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path // a missing root is reported by the existence check
	}
	for _, p := range []string{filepath.Clean(path), resolved} {
		if p == string(filepath.Separator) {
			return problem("Kořen celého disku nelze procházet; zvol konkrétní adresář.")
		}
		if privacy.Forbidden(p) {
			return problem("Adresář {path} je chráněný (klíče, přihlašovací údaje) a nečte se.", "path", path)
		}
	}
	return nil
}

func expand(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// IsOwn decides whether a scanned repository belongs to the configured owners.
// Repositories without a remote are local work and always count as own.
func (c Config) IsOwn(r scan.Repo) bool {
	if !r.HasRemote {
		return true
	}
	if len(c.Owners) == 0 {
		return true
	}
	prefix := r.Remote.Host + "/" + r.Remote.Owner
	for _, o := range c.Owners {
		if o == prefix {
			return true
		}
	}
	return false
}
