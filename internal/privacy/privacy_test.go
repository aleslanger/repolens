package privacy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestForbidden(t *testing.T) {
	cases := map[string]bool{
		"/home/u/projects/id_rsa":                    true,
		"/home/u/projects/id_rsa.pub":                true,
		"/home/u/projects/app/id_ed25519":            true,
		"/home/u/projects/app/.env":                  true,
		"/home/u/projects/app/.env.local":            true,
		"/home/u/projects/app/cert.pem":              true,
		"/home/u/projects/app/server.key":            true,
		"/home/u/.ssh/config":                        true,
		"/home/u/.aws/credentials":                   true,
		"/home/u/.gnupg/pubring.kbx":                 true,
		"/home/u/projects/app/.ssh":                  true,
		"/home/u/projects/app/CLAUDE.md":             false,
		"/home/u/projects/app/.claude/settings.json": false,
		"/home/u/projects/app/environment.go":        false,
		"/home/u/projects/app/.envrc.example.md":     true,
		"/home/u/projects/app/docs/keys.md":          false,
		"/home/u/.codex/auth.json":                   true,
		"/home/u/.claude.json":                       true,
		"/home/u/.codex/config.toml":                 false,
		"/home/u/projects/app/ID_RSA":                true,
		"/home/u/projects/app/.ENV":                  true,
		"/home/u/.SSH/known_hosts":                   true,
		"/home/u/projects/app/.npmrc":                true,
		"/home/u/.kube/config":                       true,
		"/home/u/.claude/.credentials.json":          true,
	}
	for path, want := range cases {
		if got := Forbidden(path); got != want {
			t.Errorf("Forbidden(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestReadFileRefusesForbidden(t *testing.T) {
	if _, err := ReadFile("/nonexistent/.env", 1024); err == nil {
		t.Fatal("expected refusal for .env")
	}
}

func TestOpenRefusesSymlinkToForbiddenTarget(t *testing.T) {
	dir := t.TempDir()
	secretDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "id_rsa"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(filepath.Join(secretDir, "id_rsa"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(link, 1024); !errors.Is(err, ErrForbidden) {
		t.Fatalf("symlink to a private key: err = %v, want ErrForbidden", err)
	}
	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(ok, 1024); err != nil || string(b) != "{}" {
		t.Fatalf("regular file: %q, %v", b, err)
	}
	if _, err := ReadFile(dir, 1024); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("directory: err = %v, want ErrNotRegular", err)
	}
}
