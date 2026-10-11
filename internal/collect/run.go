package collect

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"repolens/internal/config"
	"repolens/internal/github"
	"repolens/internal/store"
)

// Options select what one collection covers. Roots are scanned for local repositories;
// Accounts are GitHub users or organisations whose repositories are collected even without
// a clone (mode A when Roots is empty).
type Options struct {
	Roots    []config.Root
	Accounts []string
	GitHub   bool
	Agents   bool
	// Strict fails the run when anything was skipped as an error or not recognized, so a timer
	// or a script notices format changes instead of reading an incomplete database.
	Strict bool
}

// strictCheck turns errors and unrecognized records into a failure in strict mode.
func strictCheck(opts Options, sum *Summary) error {
	if !opts.Strict || (sum.Errors == 0 && sum.Unrecognized == 0) {
		return nil
	}
	return fmt.Errorf("--strict: %d chyb, %d nerozpoznaných záznamů", sum.Errors, sum.Unrecognized)
}

// mode names the run for collect_run: "dir", "github" or "all".
func (o Options) mode() (mode, target string) {
	var paths []string
	for _, r := range o.Roots {
		paths = append(paths, r.Path)
	}
	switch {
	case len(o.Accounts) == 0:
		return "dir", strings.Join(paths, ", ")
	case len(o.Roots) == 0:
		return "github", strings.Join(o.Accounts, ", ")
	default:
		return "all", strings.Join(append(paths, o.Accounts...), ", ")
	}
}

// Run executes one collection and records it in collect_run.
func Run(ctx context.Context, st *store.Store, cfg config.Config, opts Options, log io.Writer) error {
	unlock, err := Lock(cfg.DB)
	if err != nil {
		return err
	}
	defer unlock()
	// Costs are computed now, with this price list; remember which one for the dashboard.
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('prices_date',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, cfg.PricesDate); err != nil {
		return fmt.Errorf("record price list date: %w", err)
	}
	mode, target := opts.mode()
	var runID int64
	if err := st.DB.QueryRowContext(ctx, `INSERT INTO collect_run(mode,target,started_at) VALUES(?,?,?) RETURNING id`,
		mode, target, time.Now().Unix()).Scan(&runID); err != nil {
		return fmt.Errorf("start run: %w", err)
	}
	sum := &Summary{}
	err = runSteps(ctx, st, cfg, opts, log, sum)
	if _, uerr := st.DB.ExecContext(context.Background(), `UPDATE collect_run SET finished_at=?, repos=?, skipped=?, commits=?, sessions=?, unrecognized=?, errors=? WHERE id=?`,
		time.Now().Unix(), sum.Repos, sum.Skipped, sum.Commits, sum.Sessions, sum.Unrecognized, sum.Errors, runID); uerr != nil {
		return fmt.Errorf("finish run: %w", uerr)
	}
	for _, f := range sum.Failures {
		if _, ferr := st.DB.ExecContext(context.Background(), `INSERT INTO collect_error(run_id,target,message) VALUES(?,?,?)`,
			runID, f.Target, f.Message); ferr != nil {
			return fmt.Errorf("record error: %w", ferr)
		}
	}
	// The summary is printed even after Ctrl-C, and a failure to read it never hides err.
	totals, terr := dbTotals(context.WithoutCancel(ctx), st)
	if terr == nil {
		printSummary(log, sum, totals)
	}
	if err != nil {
		return err
	}
	if terr != nil {
		return terr
	}
	return strictCheck(opts, sum)
}

type totals struct{ repos, sessions, unrecognized, parseErrors int }

func dbTotals(ctx context.Context, st *store.Store) (totals, error) {
	var t totals
	err := st.DB.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM repo),
		(SELECT COUNT(*) FROM agent_session WHERE parent_id IS NULL),
		(SELECT IFNULL(SUM(count),0) FROM parse_issue WHERE kind='unrecognized'),
		(SELECT IFNULL(SUM(count),0) FROM parse_issue WHERE kind='error')`).Scan(&t.repos, &t.sessions, &t.unrecognized, &t.parseErrors)
	if err != nil {
		return t, fmt.Errorf("db totals: %w", err)
	}
	return t, nil
}

func runSteps(ctx context.Context, st *store.Store, cfg config.Config, opts Options, log io.Writer, sum *Summary) error {
	gh, err := githubFor(ctx, st, opts, log, sum)
	if err != nil {
		return err
	}
	var repos []LocalRepo
	if len(opts.Roots) > 0 {
		if len(cfg.Owners) == 0 {
			fmt.Fprintln(log, "Pozor: v konfiguraci chybí \"owners\", beru všechna repa jako vlastní.")
		}
		if repos, err = Dirs(ctx, st, cfg, opts.Roots, log, sum); err != nil {
			return err
		}
	}
	if gh != nil {
		if err := syncGitHub(ctx, st, cfg, gh, repos, opts.Accounts, log, sum); err != nil {
			return err
		}
	}
	if cfg.FetchPrices {
		if cfg.ListPrices, err = priceList(ctx, st, gh, log, sum); err != nil {
			return err
		}
	} else if _, err := st.DB.ExecContext(ctx, `DELETE FROM meta WHERE key='price_list_date'`); err != nil {
		return fmt.Errorf("clear price list date: %w", err) // the list is not in use any more
	}
	if opts.Agents && len(opts.Roots) > 0 {
		if err := Agents(ctx, st, cfg, repos, log, sum); err != nil {
			return err
		}
	}
	// Estimated costs follow the current price lists, also for sessions read earlier.
	if err := Reprice(ctx, st, cfg); err != nil {
		return err
	}
	// Commit rows may have been reloaded (in_session reset), so attribution is always recomputed.
	if err := MarkCommits(ctx, st); err != nil {
		return err
	}
	if err := People(ctx, st, cfg); err != nil {
		return err
	}
	if gh != nil {
		calls, hits := gh.Stats()
		fmt.Fprintf(log, "GitHub: %d volání API, %d odpovědí z cache (304)\n", calls, hits)
	}
	return nil
}

// githubFor returns a client when the run needs GitHub. Without a token a GitHub-only run fails,
// while a local run continues without GitHub data.
func githubFor(ctx context.Context, st *store.Store, opts Options, log io.Writer, sum *Summary) (*github.Client, error) {
	if !opts.GitHub {
		return nil, nil
	}
	c, err := newGitHubClient(ctx, st, log)
	switch {
	case err != nil && len(opts.Roots) == 0:
		return nil, err
	case err != nil && len(opts.Accounts) > 0:
		// Accounts were asked for: missing them is an error of the run (and fails --strict).
		sum.fail(fmt.Errorf("GitHub přeskočen: %w", err))
		return nil, nil
	case err != nil:
		fmt.Fprintf(log, "GitHub přeskočen: %v\n", err)
		return nil, nil
	}
	return c, nil
}

// syncGitHub syncs the cloned GitHub repositories and those of the accounts, each once.
// An account that cannot be listed is an error of the run, not a reason to stop it, unless
// there is nothing else to collect.
func syncGitHub(ctx context.Context, st *store.Store, cfg config.Config, gh *github.Client, repos []LocalRepo, accounts []string, log io.Writer, sum *Summary) error {
	lists := [][]ghRepo{localTargets(ctx, st, cfg, repos, sum)}
	for _, login := range accounts {
		targets, err := accountTargets(ctx, st, cfg, gh, login, log, sum)
		if err != nil && len(repos) == 0 && len(accounts) == 1 {
			return err
		}
		if err != nil {
			sum.fail(fmt.Errorf("GitHub účet %s: %w", login, err))
			continue
		}
		lists = append(lists, targets)
	}
	syncAll(ctx, st, cfg, gh, mergeTargets(lists...), log, sum)
	return nil
}

func printSummary(w io.Writer, s *Summary, t totals) {
	fmt.Fprintln(w, "\nSouhrn:                 tento běh   celkem v DB")
	fmt.Fprintf(w, "  repozitáře:           %9d   %11d\n", s.Repos, t.repos)
	fmt.Fprintf(w, "  přeskočeno (cizí):    %9d\n", s.Skipped)
	fmt.Fprintf(w, "  nové commity:         %9d\n", s.Commits)
	fmt.Fprintf(w, "  sessions agentů:      %9d   %11d\n", s.Sessions, t.sessions)
	fmt.Fprintf(w, "  nerozpoznané záznamy: %9d   %11d\n", s.Unrecognized, t.unrecognized)
	fmt.Fprintf(w, "  chyby:                %9d   %11d (z toho parsování)\n", s.Errors, t.parseErrors)
	for _, e := range s.Failures {
		fmt.Fprintf(w, "    - %s\n", e.Message)
	}
}
