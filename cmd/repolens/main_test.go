package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/config"
)

func TestCollectOptions(t *testing.T) {
	no := false
	cfg := config.Config{ScanDepth: 4, Roots: []config.Root{{Path: "/code", Depth: 2}}, GitHubAccounts: []string{"octo"}}
	cases := []struct {
		name         string
		cfg          config.Config
		dir, account string
		github       bool
		roots, accts int
		wantErr      bool
	}{
		{"config roots and accounts", cfg, "", "", true, 1, 1, false},
		{"--dir replaces the configuration", cfg, "/x", "", true, 1, 0, false},
		{"--github alone is mode A", cfg, "", "someone", true, 0, 1, false},
		{"--no-github drops accounts", cfg, "", "", false, 1, 0, false},
		{"uncloned disabled", config.Config{Roots: cfg.Roots, GitHubAccounts: cfg.GitHubAccounts, IncludeUncloned: &no}, "", "", true, 1, 0, false},
		{"nothing configured", config.Config{}, "", "", true, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, err := collectOptions(c.cfg, c.dir, c.account, c.github, true)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if len(opts.Roots) != c.roots || len(opts.Accounts) != c.accts {
				t.Errorf("roots = %v accounts = %v", opts.Roots, opts.Accounts)
			}
		})
	}
	if _, err := collectOptions(cfg, "", "octo", false, true); err == nil {
		t.Error("--github with --no-github must be refused")
	}
	// --dir replaces a stale configured root, so validation sees only the root in use.
	stale := config.Config{ScanDepth: 4, Roots: []config.Root{{Path: "/definitely/gone", Depth: 2}}, CIDays: 90}
	dirOpts, _ := collectOptions(stale, t.TempDir(), "", true, true)
	if errs := effective(stale, dirOpts).Validate(); len(errs) != 0 {
		t.Errorf("effective config errors = %v", errs)
	}
	opts, _ := collectOptions(cfg, "/x", "", true, true)
	if opts.Roots[0].Depth != 4 {
		t.Errorf("--dir depth = %d, want scan_depth 4", opts.Roots[0].Depth)
	}
}

func TestRunCommands(t *testing.T) {
	dir := t.TempDir()
	// No owners: GitHub accounts would otherwise be derived from them, and a test must never
	// reach the network.
	t.Setenv("GITHUB_TOKEN", "")
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	demoDB := filepath.Join(dir, "demo.db")
	out := filepath.Join(dir, "out.html")
	anon := filepath.Join(dir, "anon.html")
	for _, args := range [][]string{
		{"demo", "--db", demoDB},
		{"export", "--config", cfgPath, "--db", demoDB, "--out", out},
		{"export", "--config", cfgPath, "--db", demoDB, "--out", anon, "--anonymize", "--lang", "en"},
		{"install-timer", "--config", cfgPath, "--print"},
		{"--help"},
	} {
		if err := run(args); err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	page, err := os.ReadFile(anon)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), `data-lang="en"`) || !strings.Contains(string(page), `"anonymized":true`) || strings.Contains(string(page), "beacon") {
		t.Error("anonymized English export is wrong")
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Errorf("export missing: %v", err)
	}
	for _, args := range [][]string{
		{"demo"},                 // needs --db
		{"demo", "--db", demoDB}, // database is not empty
		{"export", "--config", cfgPath, "--db", demoDB, "--out", out, "--lang", "de"},
		{"collect", "--config", cfgPath, "--db", filepath.Join(dir, "c.db"), "--github", "octo", "--no-github"},
		{"collect", "--config", cfgPath, "--db", filepath.Join(dir, "c.db")}, // no roots, no accounts
		{"install-timer", "--config", cfgPath, "--print", "--schedule", ""},
		{"nonsense"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run %v succeeded, want an error", args)
		}
	}
}

func TestRunCollectDir(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "code", "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "feat: x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "x.db")
	if err := run([]string{"collect", "--config", cfgPath, "--db", db, "--dir", filepath.Join(dir, "code"), "--no-github", "--no-agents", "--strict"}); err != nil {
		t.Fatal(err)
	}
	// A relative --dir is refused by validation before anything is scanned.
	if err := run([]string{"collect", "--config", cfgPath, "--db", db, "--dir", "code", "--no-github", "--no-agents"}); err == nil {
		t.Error("relative --dir accepted")
	}
}

// The dashboard's collection re-reads the configuration on every run, validates it and writes
// through its own database connection.
func TestCollectFromConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	root := filepath.Join(dir, "code")
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", filepath.Join(root, "app"), "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	cfgPath := filepath.Join(dir, "config.json")
	body := `{"roots": [{"path": "` + root + `"}], "claude_home": "` + dir + `/claude", "codex_home": "` + dir + `/codex"}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "x.db")
	var log strings.Builder
	if err := collectFromConfig(cfgPath, db)(t.Context(), &log); err != nil {
		t.Fatalf("collect: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "Skenuji "+root) {
		t.Errorf("log does not show the configured root:\n%s", log.String())
	}
	if err := os.WriteFile(cfgPath, []byte(`{"roots": [{"path": "relative"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collectFromConfig(cfgPath, db)(t.Context(), &log); err == nil {
		t.Error("an invalid configuration must stop the dashboard's collection")
	}
}
