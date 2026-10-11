// Package timer writes a systemd user timer that runs `repolens collect` on a schedule.
package timer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	ServiceName = "repolens-collect.service"
	TimerName   = "repolens-collect.timer"
	unitMode    = 0o644
	dirMode     = 0o755
)

// Spec says what the service runs and when. Paths must be absolute; DB is optional.
type Spec struct {
	Exe, Config, DB string
	// Schedule is a systemd OnCalendar expression such as "daily" or "*-*-* 06:00:00".
	Schedule string
}

// Units are the contents of the two unit files.
type Units struct{ Service, Timer string }

// UserDir is where systemd looks for user units.
func UserDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config dir: %w", err)
	}
	return filepath.Join(dir, "systemd", "user"), nil
}

// quote makes a value one systemd word: double quotes around it, % and $ doubled (specifiers and
// variable expansion otherwise). Characters that could end the word or the line are refused.
func quote(what, v string) (string, error) {
	if strings.ContainsAny(v, "\"\\\n\r\x00") {
		return "", fmt.Errorf("%s obsahuje nepovolený znak: %q", what, v)
	}
	return `"` + strings.NewReplacer("%", "%%", "$", "$$").Replace(v) + `"`, nil
}

func absPath(what, v string) (string, error) {
	if !filepath.IsAbs(v) {
		return "", fmt.Errorf("%s musí být absolutní cesta: %q", what, v)
	}
	return quote(what, v)
}

// Render renders the service and the timer. The service runs collect --strict, so a failed run
// shows in `systemctl --user status`.
func Render(s Spec) (Units, error) {
	if s.Schedule == "" || strings.ContainsAny(s.Schedule, "\n\r\x00") {
		return Units{}, errors.New("plán (--schedule) musí být jednořádkový výraz OnCalendar, např. daily")
	}
	exe, err := absPath("program", s.Exe)
	if err != nil {
		return Units{}, err
	}
	cfg, err := absPath("konfigurace", s.Config)
	if err != nil {
		return Units{}, err
	}
	cmd := exe + " collect --strict --config " + cfg
	if s.DB != "" {
		db, err := absPath("databáze", s.DB)
		if err != nil {
			return Units{}, err
		}
		cmd += " --db " + db
	}
	service := "[Unit]\nDescription=repolens: collect repository and agent statistics\n\n" +
		"[Service]\nType=oneshot\nExecStart=" + cmd + "\nNice=10\nIOSchedulingClass=idle\n"
	timer := "[Unit]\nDescription=repolens: scheduled collection\n\n" +
		"[Timer]\nOnCalendar=" + s.Schedule + "\nPersistent=true\nRandomizedDelaySec=10min\nUnit=" + ServiceName + "\n\n" +
		"[Install]\nWantedBy=timers.target\n"
	return Units{Service: service, Timer: timer}, nil
}

// Write stores both unit files in dir.
func Write(dir string, u Units) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	for name, body := range map[string]string{ServiceName: u.Service, TimerName: u.Timer} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), unitMode); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// Remove deletes both unit files; missing files are not an error.
func Remove(dir string) error {
	for _, name := range []string{TimerName, ServiceName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	return nil
}

// CheckSchedule asks systemd whether schedule is a valid OnCalendar expression, so a typo fails
// before anything is written. Without systemd-analyze it returns nil and systemd reports it later.
func CheckSchedule(ctx context.Context, schedule string) error {
	path, err := exec.LookPath("systemd-analyze")
	if err != nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, path, "calendar", "--", schedule).CombinedOutput()
	if err != nil {
		return fmt.Errorf("plán %q systemd nezná: %s", schedule, strings.TrimSpace(string(out)))
	}
	return nil
}
