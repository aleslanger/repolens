package timer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitsQuoteAndEscapePaths(t *testing.T) {
	u, err := Render(Spec{Exe: "/opt/repo lens/bin/repolens", Config: "/home/u/.config/repolens/100%$x.json", Schedule: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="/opt/repo lens/bin/repolens" collect --strict --config "/home/u/.config/repolens/100%%$$x.json"`
	if !strings.Contains(u.Service, want) {
		t.Errorf("service:\n%s\nwant line %s", u.Service, want)
	}
	for _, line := range []string{"OnCalendar=daily", "Persistent=true", "Unit=" + ServiceName} {
		if !strings.Contains(u.Timer, line) {
			t.Errorf("timer lacks %q:\n%s", line, u.Timer)
		}
	}
	if strings.Contains(u.Service, "--db") {
		t.Error("no --db unless set")
	}
}

func TestUnitsRejectUnsafeValues(t *testing.T) {
	bad := []Spec{
		{Exe: "relative/repolens", Config: "/c.json", Schedule: "daily"},
		{Exe: "/bin/repolens\nExecStartPost=/bin/sh", Config: "/c.json", Schedule: "daily"},
		{Exe: "/bin/repolens", Config: "/c\".json", Schedule: "daily"},
		{Exe: "/bin/repolens", Config: "/c.json", Schedule: "daily\nOnBootSec=1"},
		{Exe: "/bin/repolens", Config: "/c.json", DB: "/x\\y.db", Schedule: "daily"},
		{Exe: "/bin/repolens", Config: "/c.json", Schedule: ""},
	}
	for _, s := range bad {
		if _, err := Render(s); err == nil {
			t.Errorf("Render(%q) accepted", s)
		}
	}
}

func TestInstallAndRemove(t *testing.T) {
	dir := t.TempDir()
	u, err := Render(Spec{Exe: "/bin/repolens", Config: "/c.json", DB: "/d.db", Schedule: "*-*-* 06:00:00"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, u); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ServiceName))
	if err != nil || !strings.Contains(string(b), `--db "/d.db"`) {
		t.Fatalf("service file: %s, %v", b, err)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, TimerName)); !os.IsNotExist(err) {
		t.Errorf("timer file still there: %v", err)
	}
	if err := Remove(dir); err != nil {
		t.Errorf("removing twice: %v", err)
	}
}

func TestCheckSchedule(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not installed")
	}
	if err := CheckSchedule(t.Context(), "*-*-* 06:00:00"); err != nil {
		t.Errorf("valid schedule refused: %v", err)
	}
	if err := CheckSchedule(t.Context(), "every tuesday-ish"); err == nil {
		t.Error("invalid schedule accepted")
	}
}
