package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExcludeAndSessionPaths(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	body := `{"exclude": ["~/projects/tmp", "Octo/Gone", "github.com/octo/Old"],
		"session_paths": {"~/old/app": "octo/app"}, "prices_date": "2026-10-01"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if !cfg.ExcludedPath(filepath.Join(home, "projects/tmp/redis/data")) || cfg.ExcludedPath(filepath.Join(home, "projects/tmpx")) {
		t.Error("path exclusion wrong (prefix must end at a separator)")
	}
	if !cfg.ExcludedRepo("github.com/octo/gone") || !cfg.ExcludedRepo("GitHub.com/octo/old") || cfg.ExcludedRepo("github.com/octo/app") {
		t.Errorf("repo exclusion wrong: %v", cfg.Exclude)
	}
	if cfg.SessionPaths[filepath.Join(home, "old/app")] != "github.com/octo/app" {
		t.Errorf("session paths = %v", cfg.SessionPaths)
	}
	if errs := cfg.validateLists(); len(errs) != 0 {
		t.Errorf("valid lists rejected: %v", errs)
	}
	bad := Config{Exclude: []string{"not a key", "~relative"}, SessionPaths: map[string]string{"rel": "x"}, PricesDate: "1.10.2026"}
	bad.Exclude[1] = "relative/dir"
	if errs := bad.validateLists(); len(errs) != 5 {
		t.Errorf("errors = %v, want 5", errs)
	}
}
