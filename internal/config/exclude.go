package config

import (
	"path/filepath"
	"regexp"
	"strings"
)

// repoKeySpec is host/owner/name, the key repositories are stored under.
var repoKeySpec = regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]{2,}/[a-z0-9_.-]+/[a-z0-9_.-]+$`)

var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// normalizeRepoKey lower-cases a repository key and reads "owner/name" as a github.com one.
func normalizeRepoKey(k string) string {
	k = strings.ToLower(strings.Trim(strings.TrimSpace(k), "/"))
	if strings.Count(k, "/") == 1 {
		k = githubHost + "/" + k
	}
	return k
}

// isPathEntry tells excluded directories (absolute or ~/ paths) from repository keys.
func isPathEntry(e string) bool { return strings.HasPrefix(e, "/") || strings.HasPrefix(e, "~") }

// normalizeLists expands ~ in paths and normalizes repository keys in Exclude and SessionPaths.
func normalizeLists(cfg *Config, home string) {
	for i, e := range cfg.Exclude {
		if isPathEntry(e) {
			cfg.Exclude[i] = filepath.Clean(expand(strings.TrimSpace(e), home))
		} else {
			cfg.Exclude[i] = normalizeRepoKey(e)
		}
	}
	if len(cfg.SessionPaths) > 0 {
		out := make(map[string]string, len(cfg.SessionPaths))
		for p, k := range cfg.SessionPaths {
			out[filepath.Clean(expand(strings.TrimSpace(p), home))] = normalizeRepoKey(k)
		}
		cfg.SessionPaths = out
	}
}

// ExcludedPath reports whether a directory lies in an excluded path; it is not scanned.
func (c Config) ExcludedPath(path string) bool {
	path = filepath.Clean(path)
	for _, e := range c.Exclude {
		if isPathEntry(e) && (path == e || strings.HasPrefix(path, e+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// ExcludedRepo reports whether a repository key is excluded from GitHub; its local statistics
// stay, but repolens no longer asks the API for it.
func (c Config) ExcludedRepo(key string) bool {
	key = strings.ToLower(key)
	for _, e := range c.Exclude {
		if !isPathEntry(e) && e == key {
			return true
		}
	}
	return false
}

func (c Config) validateLists() []error {
	var errs []error
	for _, e := range c.Exclude {
		switch {
		case isPathEntry(e) && !filepath.IsAbs(e):
			errs = append(errs, problem("Výjimka {entry} musí být absolutní cesta.", "entry", e))
		case !isPathEntry(e) && !repoKeySpec.MatchString(e):
			errs = append(errs, problem("Výjimka „{entry}“ není cesta ani repozitář ve tvaru github.com/účet/repo.", "entry", e))
		}
	}
	for p, k := range c.SessionPaths {
		if !filepath.IsAbs(p) {
			errs = append(errs, problem("Cesta sessions {path} musí být absolutní.", "path", p))
		}
		if !repoKeySpec.MatchString(k) && !strings.HasPrefix(k, "local:/") {
			errs = append(errs, problem("„{repo}“ není klíč repozitáře.", "repo", k))
		}
	}
	if c.PricesDate != "" && !isoDate.MatchString(c.PricesDate) {
		errs = append(errs, problem("Datum ceníku musí mít tvar RRRR-MM-DD."))
	}
	return errs
}
