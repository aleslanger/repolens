package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveKeepsOtherKeysAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	orig := `{"db": "~/x.db", "scan_depth": 3, "owners": ["github.com/octo"], "future_key": {"a": 1}}`
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	yes := true
	e := Editable{
		Roots:           []Root{{Path: dir, Depth: 2}},
		GitHubAccounts:  []string{"octo-org"},
		Owners:          []string{"github.com/octo"},
		IncludeForks:    true,
		IncludeUncloned: &yes,
		CIDays:          30,
		Identities:      []Identity{{Name: "Octo Cat", Aliases: []string{"octo"}}},
		Prices:          map[string]Price{"gpt-5": {Input: 1, Output: 2}},
	}
	if errs := Save(p, e); len(errs) != 0 {
		t.Fatalf("save: %v", errs)
	}
	var raw map[string]json.RawMessage
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	compact := func(m json.RawMessage) string {
		var buf bytes.Buffer
		if err := json.Compact(&buf, m); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if compact(raw["db"]) != `"~/x.db"` || compact(raw["scan_depth"]) != "3" || compact(raw["future_key"]) != `{"a":1}` {
		t.Errorf("unrelated keys changed: %s", b)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CIDays != 30 || !cfg.IncludeForks || len(cfg.Roots) != 1 || cfg.GitHubAccounts[0] != "octo-org" || cfg.Prices["gpt-5"].Output != 2 {
		t.Errorf("saved config = %+v", cfg)
	}
	bak, err := os.ReadFile(p + ".bak")
	if err != nil || string(bak) != orig {
		t.Errorf("backup = %q, %v", bak, err)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", st.Mode(), err)
	}
}

func TestSaveRejectsInvalidWithoutWriting(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"owners": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	errs := Save(p, Editable{Roots: []Root{{Path: "/definitely/not/here", Depth: 2}}, CIDays: 90})
	if len(errs) == 0 {
		t.Fatal("expected validation errors")
	}
	if b, _ := os.ReadFile(p); string(b) != `{"owners": []}` {
		t.Errorf("file changed: %s", b)
	}
	if _, err := os.Stat(p + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup written for a rejected save: %v", err)
	}
}

func TestSaveCreatesMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.json")
	if errs := Save(p, Editable{CIDays: 90}); len(errs) != 0 {
		t.Fatalf("save: %v", errs)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
}

func TestEditableRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"owners": ["github.com/Octo"], "roots": [{"path": "~/code"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := LoadEditable(p)
	if err != nil {
		t.Fatal(err)
	}
	// Values are shown as written (the ~ is kept), with defaults filled in for the form.
	if len(e.Roots) != 1 || e.Roots[0].Path != "~/code" || e.CIDays != defaultCIDays || len(e.GitHubAccounts) != 0 {
		t.Errorf("editable = %+v", e)
	}
}

func TestSaveClearsOmittedValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"include_uncloned": false, "roots": [{"path": "/x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if errs := Save(p, Editable{CIDays: 90}); len(errs) != 0 {
		t.Fatalf("save: %v", errs)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Uncloned() || len(cfg.Roots) != 0 {
		t.Errorf("omitted values survived: uncloned=%v roots=%v", cfg.Uncloned(), cfg.Roots)
	}
}

func TestSaveRejectsNegativeNumbers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	for _, e := range []Editable{{CIDays: -5}, {CIDays: 90, Roots: []Root{{Path: dir, Depth: -2}}}} {
		if errs := Save(p, e); len(errs) != 1 {
			t.Errorf("Save(%+v) errors = %v, want one", e, errs)
		}
	}
}

func TestSaveFollowsSymlinkAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "repolens.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if errs := Save(link, Editable{CIDays: 30}); len(errs) != 0 {
		t.Fatalf("save: %v", errs)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced: %v %v", fi.Mode(), err)
	}
	if st, err := os.Stat(real); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, %v; want 0640", st.Mode().Perm(), err)
	}
	if cfg, err := Load(link); err != nil || cfg.CIDays != 30 {
		t.Errorf("saved through the link: %+v, %v", cfg.CIDays, err)
	}
}
