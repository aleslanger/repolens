// Command repolens collects development and agent-usage statistics and serves a local dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"repolens/internal/collect"
	"repolens/internal/config"
	"repolens/internal/demo"
	"repolens/internal/store"
	"repolens/internal/timer"
	"repolens/internal/web"
)

const (
	defaultPort   = 8765
	defaultExport = "repolens.html"
)

const usage = `repolens — přehledy vývoje a použití kódovacích agentů

Použití:
  repolens collect [--dir <cesta>] [--github <účet>] [--depth N] [--no-github] [--no-agents] [--strict]
      bez --dir a --github projde kořeny a GitHub účty z konfigurace
  repolens serve [--port N]
  repolens install-timer [--schedule daily] [--print]   pravidelný sběr přes systemd (uživatelský timer)
  repolens uninstall-timer
  repolens demo --db demo.db                            vymyšlená data na vyzkoušení (jen do nové databáze)
  repolens export [--out soubor.html] [--lang cs|en] [--anonymize]

Společné přepínače:
  --config <soubor>   konfigurace (výchozí ~/.config/repolens/config.json)
  --db <soubor>       SQLite databáze (přepíše hodnotu z konfigurace)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "chyba:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	dbPath := fs.String("db", "", "database file")
	dir := fs.String("dir", "", "directory to scan")
	depth := fs.Int("depth", 0, "scan depth")
	owner := fs.String("github", "", "GitHub owner")
	noGitHub := fs.Bool("no-github", false, "skip GitHub enrichment")
	noAgents := fs.Bool("no-agents", false, "skip agent data")
	strict := fs.Bool("strict", false, "fail on any error or unrecognized record")
	port := fs.Int("port", defaultPort, "port")
	out := fs.String("out", defaultExport, "export file")
	lang := fs.String("lang", web.Langs[0], "export language (cs, en)")
	schedule := fs.String("schedule", "daily", "systemd OnCalendar expression for install-timer")
	printOnly := fs.Bool("print", false, "install-timer: print the units instead of installing them")
	anon := fs.Bool("anonymize", false, "export: replace names, e-mails and paths with pseudonyms")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "install-timer":
		return installTimer(ctx, *cfgPath, *dbPath, *schedule, *printOnly)
	case "uninstall-timer":
		return uninstallTimer(ctx)
	case "demo":
		// Never the real database by default: demo data must not mix with collected data.
		if *dbPath == "" {
			return errors.New("demo potřebuje --db <nový soubor>, např. --db demo.db")
		}
		st, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := demo.Generate(ctx, st, time.Now()); err != nil {
			return err
		}
		fmt.Printf("demo data zapsána do %s; spusť: repolens serve --db %s\n", *dbPath, *dbPath)
		return nil
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *dbPath != "" {
		cfg.DB = *dbPath
	}
	if *depth > 0 {
		cfg.ScanDepth = *depth
		for i := range cfg.Roots {
			cfg.Roots[i].Depth = *depth
		}
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	switch args[0] {
	case "collect":
		opts, err := collectOptions(cfg, *dir, *owner, !*noGitHub, !*noAgents)
		if err != nil {
			return err
		}
		if errs := effective(cfg, opts).Validate(); len(errs) > 0 {
			return fmt.Errorf("neplatná konfigurace %s: %w", *cfgPath, errors.Join(errs...))
		}
		opts.Strict = *strict
		return collect.Run(ctx, st, cfg, opts, os.Stdout)
	case "serve":
		srv := web.New(st.DB).WithControl(ctx, web.Control{ConfigPath: *cfgPath, Collect: collectFromConfig(*cfgPath, *dbPath)})
		return srv.Serve(ctx, *port)
	case "export":
		if err := web.Export(ctx, st.DB, *out, web.ExportOptions{Lang: *lang, Anonymize: *anon}); err != nil {
			return err
		}
		fmt.Println("export zapsán:", *out)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("neznámý příkaz %q", args[0])
	}
}

// collectOptions applies --dir and --github over the configured roots and accounts: either flag
// replaces the configuration, so `--dir x` scans only x and `--github y` is GitHub-only (mode A).
func collectOptions(cfg config.Config, dir, account string, withGitHub, withAgents bool) (collect.Options, error) {
	if account != "" && !withGitHub {
		return collect.Options{}, errors.New("--github a --no-github se vylučují")
	}
	opts := collect.Options{GitHub: withGitHub, Agents: withAgents}
	switch {
	case dir != "" || account != "":
		if dir != "" {
			opts.Roots = []config.Root{{Path: dir, Depth: cfg.ScanDepth}}
		}
		if account != "" {
			opts.Accounts = []string{account}
		}
	default:
		opts.Roots = cfg.Roots
		if withGitHub && cfg.Uncloned() {
			opts.Accounts = cfg.GitHubAccounts
		}
	}
	if len(opts.Roots) == 0 && len(opts.Accounts) == 0 {
		return opts, errors.New("není co sbírat: nastav \"roots\" nebo \"github_accounts\" v konfiguraci, nebo použij --dir / --github")
	}
	return opts, nil
}

// collectFromConfig runs `repolens collect` for the dashboard: the configuration is re-read on
// every run (the Settings page may have changed it) and the collection uses its own database
// connection, so the dashboard keeps answering while it writes.
func collectFromConfig(cfgPath, dbOverride string) func(context.Context, io.Writer) error {
	return func(ctx context.Context, log io.Writer) error {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		if dbOverride != "" {
			cfg.DB = dbOverride
		}
		opts, err := collectOptions(cfg, "", "", true, true)
		if err != nil {
			return err
		}
		if errs := effective(cfg, opts).Validate(); len(errs) > 0 {
			return fmt.Errorf("neplatná konfigurace: %w", errors.Join(errs...))
		}
		st, err := store.Open(cfg.DB)
		if err != nil {
			return err
		}
		defer st.Close()
		return collect.Run(ctx, st, cfg, opts, io.MultiWriter(log, os.Stdout))
	}
}

// installTimer writes the systemd user units and enables the timer. The timer runs this binary
// with the same configuration (and database, when --db was given).
func installTimer(ctx context.Context, cfgPath, dbPath, schedule string, printOnly bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own binary: %w", err)
	}
	spec := timer.Spec{Exe: exe, Schedule: schedule}
	if spec.Config, err = filepath.Abs(cfgPath); err != nil {
		return err
	}
	if dbPath != "" {
		if spec.DB, err = filepath.Abs(dbPath); err != nil {
			return err
		}
	}
	units, err := timer.Render(spec)
	if err != nil {
		return err
	}
	if err := timer.CheckSchedule(ctx, schedule); err != nil {
		return err
	}
	if printOnly {
		fmt.Printf("# %s\n%s\n# %s\n%s", timer.ServiceName, units.Service, timer.TimerName, units.Timer)
		return nil
	}
	dir, err := timer.UserDir()
	if err != nil {
		return err
	}
	if err := timer.Write(dir, units); err != nil {
		return err
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := systemctl(ctx, "enable", "--now", timer.TimerName); err != nil {
		return err
	}
	fmt.Printf("timer zapnut (%s), jednotky v %s\nstav: systemctl --user list-timers %s\n", schedule, dir, timer.TimerName)
	return nil
}

func uninstallTimer(ctx context.Context) error {
	dir, err := timer.UserDir()
	if err != nil {
		return err
	}
	if err := systemctl(ctx, "disable", "--now", timer.TimerName); err != nil {
		fmt.Fprintln(os.Stderr, "upozornění:", err)
	}
	if err := timer.Remove(dir); err != nil {
		return err
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	fmt.Println("timer odstraněn")
	return nil
}

func systemctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// effective is the configuration a run actually uses: --dir and --github replace the configured
// roots and accounts, so a stale configured root does not block `collect --dir`.
func effective(cfg config.Config, opts collect.Options) config.Config {
	cfg.Roots, cfg.GitHubAccounts = opts.Roots, opts.Accounts
	return cfg
}
