package report

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PeriodTotals are the headline numbers of one period.
type PeriodTotals struct {
	Commits     int     `json:"commits"`
	ActiveRepos int     `json:"activeRepos"`
	Sessions    int     `json:"sessions"`
	Tokens      int64   `json:"tokens"`
	Fresh       int64   `json:"fresh"`
	CostUSD     float64 `json:"costUsd"`
}

// PeriodSummary holds the totals of a period and of the equally long period before it, so the
// dashboard can show the change. Previous is nil for the all-time view.
type PeriodSummary struct {
	Since    int64         `json:"since"`
	Current  PeriodTotals  `json:"current"`
	Previous *PeriodTotals `json:"previous,omitempty"`
}

// Summary returns the totals from since to now and, when since is set, of the period before.
func Summary(ctx context.Context, db *sql.DB, now time.Time, since int64) (*PeriodSummary, error) {
	end := now.Unix() + 1
	cur, err := periodTotals(ctx, db, since, end)
	if err != nil {
		return nil, err
	}
	s := &PeriodSummary{Since: since, Current: cur}
	if since > 0 {
		prev, err := periodTotals(ctx, db, since-(end-since), since)
		if err != nil {
			return nil, err
		}
		s.Previous = &prev
	}
	return s, nil
}

func periodTotals(ctx context.Context, db *sql.DB, from, to int64) (PeriodTotals, error) {
	var t PeriodTotals
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM commits WHERE author_time>=? AND author_time<?),
		(SELECT COUNT(DISTINCT repo_id) FROM commits WHERE author_time>=? AND author_time<?),
		(SELECT COUNT(*) FROM agent_session WHERE parent_id IS NULL AND started_at>=? AND started_at<?),
		(SELECT IFNULL(SUM(m.input+m.output+m.cache_read+m.cache_write),0) FROM session_model m `+joinRoot+` WHERE s.started_at>=? AND s.started_at<?),
		(SELECT IFNULL(SUM(m.input+m.output),0) FROM session_model m `+joinRoot+` WHERE s.started_at>=? AND s.started_at<?),
		(SELECT IFNULL(SUM(m.cost_usd),0) FROM session_model m `+joinRoot+` WHERE s.started_at>=? AND s.started_at<?)`,
		from, to, from, to, from, to, from, to, from, to, from, to).
		Scan(&t.Commits, &t.ActiveRepos, &t.Sessions, &t.Tokens, &t.Fresh, &t.CostUSD)
	if err != nil {
		return t, fmt.Errorf("period totals: %w", err)
	}
	return t, nil
}

// Attention severities, in the order the dashboard lists them.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

const (
	// stalePRDays makes an open pull request "old".
	stalePRDays = 14
	// staleDataDays makes the last collection "old".
	staleDataDays = 7
	// attentionPerKind bounds repeated items (one per repository) of the same kind.
	attentionPerKind = 5
)

// AttentionItem is something worth acting on. Text is a Czech template with {placeholders}
// filled from Args (translated in the browser); RepoID names the repository when there is one.
type AttentionItem struct {
	Kind     string         `json:"kind"`
	Severity string         `json:"severity"`
	Text     string         `json:"text"`
	Args     map[string]any `json:"args,omitempty"`
	Link     string         `json:"link"`
	RepoID   int64          `json:"repoId,omitempty"`
}

// Attention gathers what needs a look: failing CI, old pull requests, collection problems,
// tokens without a price, unused configuration, unmatched sessions and stale data.
func Attention(ctx context.Context, db *sql.DB, now time.Time) ([]AttentionItem, error) {
	out := []AttentionItem{}
	steps := []func(context.Context, *sql.DB, time.Time) ([]AttentionItem, error){
		attentionCI, attentionPRs, attentionCollection, attentionAgents,
	}
	for _, step := range steps {
		items, err := step(ctx, db, now)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	rank := map[string]int{SeverityCritical: 0, SeverityWarning: 1, SeverityInfo: 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	return out, nil
}

func attentionCI(ctx context.Context, db *sql.DB, _ time.Time) ([]AttentionItem, error) {
	var out []AttentionItem
	err := queryEach(ctx, db, `SELECT r.id, r.name FROM repo r WHERE IFNULL(r.is_archived,0)=0 AND
		(SELECT conclusion FROM gh_run g WHERE g.repo_id=r.id AND (r.default_branch IS NULL OR g.branch=r.default_branch)
		 ORDER BY created_at DESC LIMIT 1) IN ('failure','timed_out','startup_failure')
		ORDER BY r.name LIMIT ?`, []any{attentionPerKind}, func(r *sql.Rows) error {
		var it AttentionItem
		var name string
		if err := r.Scan(&it.RepoID, &name); err != nil {
			return err
		}
		it.Kind, it.Severity = "ci", SeverityCritical
		it.Text = "Poslední běh CI v {repo} selhal."
		it.Args, it.Link = map[string]any{"repo": name}, fmt.Sprintf("#/repo/%d", it.RepoID)
		out = append(out, it)
		return nil
	})
	return out, err
}

func attentionPRs(ctx context.Context, db *sql.DB, now time.Time) ([]AttentionItem, error) {
	var out []AttentionItem
	old := now.AddDate(0, 0, -stalePRDays).Unix()
	err := queryEach(ctx, db, `SELECT r.id, r.name, COUNT(*) FROM gh_issue i JOIN repo r ON r.id=i.repo_id
		WHERE i.is_pr=1 AND i.state='OPEN' AND i.created_at<? GROUP BY r.id ORDER BY COUNT(*) DESC LIMIT ?`,
		[]any{old, attentionPerKind}, func(r *sql.Rows) error {
			var it AttentionItem
			var name string
			var n int
			if err := r.Scan(&it.RepoID, &name, &n); err != nil {
				return err
			}
			it.Kind, it.Severity = "stale_pr", SeverityWarning
			it.Text = "{repo}: {n} otevřených PR starších než {days} dní."
			it.Args = map[string]any{"repo": name, "n": n, "days": stalePRDays}
			it.Link = fmt.Sprintf("#/repo/%d?tab=github", it.RepoID)
			out = append(out, it)
			return nil
		})
	return out, err
}

func attentionCollection(ctx context.Context, db *sql.DB, now time.Time) ([]AttentionItem, error) {
	var last sql.NullInt64
	var errorsN, unrecognized int
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT MAX(finished_at) FROM collect_run),
		(SELECT COUNT(*) FROM collect_error WHERE run_id=(SELECT MAX(id) FROM collect_run WHERE finished_at IS NOT NULL)),
		(SELECT IFNULL(SUM(count),0) FROM parse_issue WHERE kind='unrecognized')`).Scan(&last, &errorsN, &unrecognized)
	if err != nil {
		return nil, fmt.Errorf("attention collection: %w", err)
	}
	var out []AttentionItem
	switch {
	case !last.Valid:
		out = append(out, AttentionItem{Kind: "no_data", Severity: SeverityWarning, Text: "Zatím neproběhl žádný sběr.", Link: "#/status"})
	case now.Unix()-last.Int64 > staleDataDays*secondsPerDay:
		out = append(out, AttentionItem{Kind: "stale_data", Severity: SeverityWarning, Text: "Stáří posledního sběru: {days} dní.",
			Args: map[string]any{"days": (now.Unix() - last.Int64) / secondsPerDay}, Link: "#/status"})
	}
	if errorsN > 0 {
		out = append(out, AttentionItem{Kind: "collect_errors", Severity: SeverityWarning, Text: "Chyby posledního sběru: {n} (opakující se lze vyloučit).",
			Args: map[string]any{"n": errorsN}, Link: "#/status"})
	}
	if unrecognized > 0 {
		out = append(out, AttentionItem{Kind: "unrecognized", Severity: SeverityInfo, Text: "Nerozpoznané záznamy v transcriptech: {n}.",
			Args: map[string]any{"n": unrecognized}, Link: "#/status"})
	}
	return out, nil
}

func attentionAgents(ctx context.Context, db *sql.DB, _ time.Time) ([]AttentionItem, error) {
	var unpricedTokens int64
	var unpricedModels, unmatched int
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT IFNULL(SUM(input+output+cache_read+cache_write),0) FROM session_model WHERE cost_usd IS NULL),
		(SELECT COUNT(DISTINCT model) FROM session_model WHERE cost_usd IS NULL),
		(SELECT COUNT(*) FROM agent_session WHERE repo_id IS NULL AND parent_id IS NULL)`).Scan(&unpricedTokens, &unpricedModels, &unmatched)
	if err != nil {
		return nil, fmt.Errorf("attention agents: %w", err)
	}
	var out []AttentionItem
	if unpricedModels > 0 {
		out = append(out, AttentionItem{Kind: "unpriced", Severity: SeverityInfo, Text: "Modely bez ceny: {n} ({tokens} tokenů); doplň ceník.",
			Args: map[string]any{"n": unpricedModels, "tokens": unpricedTokens}, Link: "#/settings"})
	}
	if unmatched > 0 {
		out = append(out, AttentionItem{Kind: "unmatched", Severity: SeverityInfo, Text: "Sessions mimo repozitáře: {n}.",
			Args: map[string]any{"n": unmatched}, Link: "#/agents?tab=sessions"})
	}
	av := &AgentView{Counters: map[string][]CounterRow{}, Unconfigured: map[string][]CounterRow{}}
	if err := agentConfig(ctx, db, nil, av); err != nil {
		return nil, err
	}
	unused := 0
	for _, c := range av.Config {
		if c.Usable && c.Present && c.Uses == 0 && c.Scope == "project" {
			unused++
		}
	}
	if unused > 0 {
		out = append(out, AttentionItem{Kind: "unused_config", Severity: SeverityInfo, Text: "Nikdy nespuštěné skills, agenti a commandy v repozitářích: {n}.",
			Args: map[string]any{"n": unused}, Link: "#/agents?tab=config"})
	}
	return out, nil
}

// SessionFilter selects sessions for the paged list; zero values mean "any".
type SessionFilter struct {
	Since  int64
	RepoID int64
	Tool   string
	Model  string
	Offset int
	Limit  int
}

// SessionPage is one page of sessions, newest first, with the number of all matches.
type SessionPage struct {
	Total int          `json:"total"`
	Rows  []SessionRow `json:"rows"`
}

// maxPageSize bounds a page of the session list.
const maxPageSize = 200

// Sessions lists top-level sessions matching f.
func Sessions(ctx context.Context, db *sql.DB, f SessionFilter) (*SessionPage, error) {
	conds, args := []string{sessionFilter, "s.started_at>=?"}, []any{f.Since}
	if f.RepoID != 0 {
		conds, args = append(conds, "s.repo_id=?"), append(args, f.RepoID)
	}
	if f.Tool != "" {
		conds, args = append(conds, "s.tool=?"), append(args, f.Tool)
	}
	if f.Model != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM session_model m JOIN agent_session c ON c.id=m.session_id WHERE (c.id=s.id OR c.parent_id=s.id) AND m.model=?)")
		args = append(args, f.Model)
	}
	where := strings.Join(conds, " AND ")
	p := &SessionPage{Rows: []SessionRow{}}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_session s WHERE `+where, args...).Scan(&p.Total); err != nil {
		return nil, fmt.Errorf("count sessions: %w", err)
	}
	limit := f.Limit
	if limit <= 0 || limit > maxPageSize {
		limit = maxPageSize
	}
	err := queryEach(ctx, db, `SELECT `+sessionColumns+` FROM agent_session s LEFT JOIN repo r ON r.id=s.repo_id
		WHERE `+where+` ORDER BY s.started_at DESC LIMIT ? OFFSET ?`, append(args, limit, max(f.Offset, 0)), func(r *sql.Rows) error {
		s, err := scanSession(r)
		if err != nil {
			return err
		}
		p.Rows = append(p.Rows, s)
		return nil
	})
	return p, err
}
