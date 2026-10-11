package collect

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/config"
)

func TestExcludedPathIsNotScannedAndFailuresAreRecorded(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"keep", "skip/inner"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "init", "-q", "-b", "main")
		commitFile(t, dir, "a.txt", "a\n", "feat: a")
	}
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	st := newStore(t)
	cfg := config.Config{DB: filepath.Join(t.TempDir(), "x.db"), ScanDepth: 3, ModuleDepth: 1, Exclude: []string{filepath.Join(root, "skip")}}
	opts := Options{Roots: []config.Root{{Path: root, Depth: 3}}}
	if err := Run(context.Background(), st, cfg, opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	var repos int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM repo`).Scan(&repos); err != nil || repos != 1 {
		t.Errorf("repos = %d, %v; want only keep/", repos, err)
	}
	var target string
	if err := st.DB.QueryRow(`SELECT target FROM collect_error`).Scan(&target); err != nil || target != locked {
		t.Errorf("recorded failure target = %q, %v; want %s", target, err, locked)
	}
	// Excluding the unreadable directory makes the next run clean.
	cfg.Exclude = append(cfg.Exclude, locked)
	if err := Run(context.Background(), st, cfg, Options{Roots: opts.Roots, Strict: true}, io.Discard); err != nil {
		t.Errorf("strict run after excluding: %v", err)
	}
}

func TestManualSessionPathsMatch(t *testing.T) {
	st := newStore(t)
	if _, err := st.DB.Exec(`INSERT INTO repo(id,key,name) VALUES(5,'github.com/octo/app','app')`); err != nil {
		t.Fatal(err)
	}
	m, err := loadMatcher(context.Background(), st.DB, map[string]string{"/old/app": "github.com/octo/app", "/gone": "github.com/octo/unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := m.match("/old/app/src", ""); id != int64(5) {
		t.Errorf("manual path match = %v, want 5", id)
	}
	if id, _ := m.match("/gone/x", ""); id != nil {
		t.Errorf("unknown repository matched: %v", id)
	}
}
