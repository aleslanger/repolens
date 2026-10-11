package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"repolens/internal/privacy"
)

// Editable is the part of the configuration the Settings page may change. Values keep the form
// the user wrote (e.g. "~/code"); other keys of the file are preserved on save.
type Editable struct {
	Roots           []Root            `json:"roots"`
	GitHubAccounts  []string          `json:"github_accounts"`
	IncludeUncloned *bool             `json:"include_uncloned,omitempty"`
	Owners          []string          `json:"owners"`
	IncludeForks    bool              `json:"include_forks"`
	IncludeArchived bool              `json:"include_archived"`
	Identities      []Identity        `json:"identities"`
	Prices          map[string]Price  `json:"prices"`
	CIDays          int               `json:"ci_days"`
	Exclude         []string          `json:"exclude"`
	SessionPaths    map[string]string `json:"session_paths"`
	PricesDate      string            `json:"prices_date"`
	FetchPrices     bool              `json:"fetch_prices"`
}

// editableKeys are the JSON keys Save owns; everything else in the file is left untouched.
var editableKeys = []string{"roots", "github_accounts", "include_uncloned", "owners", "include_forks",
	"include_archived", "identities", "prices", "ci_days", "exclude", "session_paths", "prices_date", "fetch_prices"}

const (
	fileMode   = 0o600
	dirMode    = 0o700
	backupExt  = ".bak"
	jsonIndent = "  "
)

// LoadEditable returns the editable values as written in path, with form defaults filled in.
func LoadEditable(path string) (Editable, error) {
	raw, err := readRaw(path)
	if err != nil {
		return Editable{}, err
	}
	var e Editable
	if err := unmarshalKeys(raw, &e); err != nil {
		return e, fmt.Errorf("parse config %s: %w", path, err)
	}
	if e.CIDays <= 0 {
		e.CIDays = defaultCIDays
	}
	return e, nil
}

// Save validates e merged with the rest of the file, then replaces the file atomically and keeps
// the previous version next to it as config.json.bak. Nothing is written when validation fails.
func Save(path string, e Editable) []error {
	saveMu.Lock()
	defer saveMu.Unlock()
	raw, err := readRaw(path)
	if err != nil {
		return []error{err}
	}
	for _, k := range editableKeys {
		delete(raw, k)
	}
	fields, err := json.Marshal(e)
	if err != nil {
		return []error{fmt.Errorf("encode settings: %w", err)}
	}
	if err := json.Unmarshal(fields, &raw); err != nil {
		return []error{fmt.Errorf("merge settings: %w", err)}
	}
	var cfg Config
	if err := unmarshalKeys(raw, &cfg); err != nil {
		return []error{fmt.Errorf("settings: %w", err)}
	}
	if cfg, err = withDefaults(cfg); err != nil {
		return []error{err}
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		return errs
	}
	body, err := json.MarshalIndent(raw, "", jsonIndent)
	if err != nil {
		return []error{fmt.Errorf("encode config: %w", err)}
	}
	if len(body) >= maxConfigSize {
		return []error{problem("Nastavení je příliš velké (víc než {max} bajtů).", "max", maxConfigSize)}
	}
	if err := replaceFile(path, append(body, '\n')); err != nil {
		return []error{err}
	}
	return nil
}

func readRaw(path string) (map[string]json.RawMessage, error) {
	raw := map[string]json.RawMessage{}
	b, err := privacy.ReadFile(path, maxConfigSize)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return raw, nil
	case err != nil:
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return raw, nil
}

func unmarshalKeys(raw map[string]json.RawMessage, out any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// saveMu serialises saves: each one reads, merges and replaces the whole file.
var saveMu sync.Mutex

// replaceFile writes body to a temporary file in the same directory and renames it over path,
// so a crash leaves either the old or the new file. The old file is copied to path+".bak".
// A symlinked config (dotfiles) is followed so the link survives, and the file keeps its mode.
func replaceFile(path string, body []byte) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	mode := os.FileMode(fileMode)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+backupExt, old, fileMode); err != nil {
			return fmt.Errorf("backup config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config for backup: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
