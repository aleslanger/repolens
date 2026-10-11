package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/remote"
	"repolens/internal/scan"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanDepth != defaultScanDepth || cfg.ModuleDepth != defaultModuleDepth || cfg.DB == "" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadRejectsInvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error")
	}
}

func TestIsOwn(t *testing.T) {
	cfg := Config{Owners: []string{"github.com/octo"}}
	own := scan.Repo{HasRemote: true, Remote: remote.Remote{Host: "github.com", Owner: "octo", Name: "x"}}
	foreign := scan.Repo{HasRemote: true, Remote: remote.Remote{Host: "github.com", Owner: "coinbase", Name: "x"}}
	other := scan.Repo{HasRemote: true, Remote: remote.Remote{Host: "bitbucket.org", Owner: "octo", Name: "x"}}
	local := scan.Repo{}
	if !cfg.IsOwn(own) || cfg.IsOwn(foreign) || cfg.IsOwn(other) || !cfg.IsOwn(local) {
		t.Error("ownership filter wrong")
	}
}

func TestRootsAndAccountsDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	body := `{"owners": ["github.com/Octo"], "scan_depth": 3, "roots": [{"path": "~/code"}, {"path": "/srv/git", "depth": 2}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if len(cfg.Roots) != 2 || cfg.Roots[0].Path != filepath.Join(home, "code") || cfg.Roots[0].Depth != 3 || cfg.Roots[1].Depth != 2 {
		t.Errorf("roots = %+v", cfg.Roots)
	}
	// GitHub accounts default to the github.com owners; uncloned repositories are included.
	if len(cfg.GitHubAccounts) != 1 || cfg.GitHubAccounts[0] != "octo" || !cfg.Uncloned() {
		t.Errorf("accounts = %v uncloned = %v", cfg.GitHubAccounts, cfg.Uncloned())
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	good := Config{Roots: []Root{{Path: dir, Depth: 4}}, GitHubAccounts: []string{"octo-org"}, Owners: []string{"github.com/octo"}, CIDays: 90}
	if errs := good.Validate(); len(errs) != 0 {
		t.Fatalf("valid config rejected: %v", errs)
	}
	bad := Config{
		Roots:          []Root{{Path: filepath.Join(dir, "missing"), Depth: 4}, {Path: file, Depth: 4}, {Path: dir, Depth: 99}, {Path: dir, Depth: -1}},
		GitHubAccounts: []string{"../evil", "ok"},
		Owners:         []string{"no-slash"},
		CIDays:         -1,
		Prices:         map[string]Price{"m": {Input: -1}},
	}
	if errs := bad.Validate(); len(errs) != 8 {
		t.Errorf("errors = %d (%v), want 8", len(errs), errs)
	}
}

func TestValidateRefusesUnsafeRoots(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, ".ssh")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "innocent")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	// "." exists, so only the absolute-path rule can refuse it.
	for _, root := range []string{".", "/", secret, link} {
		c := Config{Roots: []Root{{Path: root, Depth: 2}}, CIDays: 90}
		if errs := c.Validate(); len(errs) != 1 {
			t.Errorf("root %q: errors %v, want exactly one", root, errs)
		}
	}
}

func TestValidateRefusesHugePrices(t *testing.T) {
	for _, v := range []float64{1e308, math.Inf(1), math.NaN(), -1} {
		c := Config{CIDays: 90, Prices: map[string]Price{"m": {Output: v}}}
		if errs := c.Validate(); len(errs) != 1 {
			t.Errorf("price %v: errors %v", v, errs)
		}
	}
}
