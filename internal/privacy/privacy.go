// Package privacy guards every file read repolens performs against secret-bearing paths.
package privacy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrForbidden is returned when a path matches the secret denylist.
var ErrForbidden = errors.New("path is on the privacy denylist")

// All comparisons are lower case, so .SSH or ID_RSA on case-insensitive filesystems match too.
var forbiddenDirs = map[string]bool{".ssh": true, ".aws": true, ".gnupg": true, ".kube": true, ".docker": true, ".password-store": true}

var forbiddenExts = map[string]bool{".pem": true, ".key": true, ".p12": true, ".pfx": true, ".keystore": true, ".jks": true}

var forbiddenPrefixes = []string{".env", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "credentials"}

// forbiddenNames are exact file names that hold agent credentials.
var forbiddenNames = map[string]bool{"auth.json": true, ".claude.json": true, ".credentials.json": true, ".netrc": true, ".git-credentials": true,
	".npmrc": true, ".pypirc": true, ".pgpass": true, "hosts.yml": true}

// Forbidden reports whether a path (or any of its directories) must never be opened.
func Forbidden(path string) bool {
	clean := strings.ToLower(filepath.Clean(path))
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if forbiddenDirs[part] {
			return true
		}
	}
	base := filepath.Base(clean)
	if forbiddenNames[base] {
		return true
	}
	for _, p := range forbiddenPrefixes {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return forbiddenExts[filepath.Ext(base)]
}

// ReadFile reads at most limit bytes of a file after checking the denylist.
func ReadFile(path string, limit int64) ([]byte, error) {
	f, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return b, nil
}

// ErrNotRegular is returned for devices, sockets and other non-regular files.
var ErrNotRegular = errors.New("not a regular file")

// Open opens a regular file for reading after checking the denylist against both the given
// path and its symlink-resolved target, so a link such as .claude/settings.json -> ~/.ssh/id_rsa
// is refused.
func Open(path string) (*os.File, error) {
	if Forbidden(path) {
		return nil, fmt.Errorf("%s: %w", path, ErrForbidden)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	if Forbidden(real) {
		return nil, fmt.Errorf("%s: %w", path, ErrForbidden)
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	return f, nil
}
