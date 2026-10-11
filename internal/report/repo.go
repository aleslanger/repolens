// Package report turns the database into JSON-ready views shared by the server and the export.
package report

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	sparkWeeks      = 26
	recentCommits   = 50
	topDirs         = 15
	agentWindowDays = 90
	secondsPerDay   = 86400
	secondsPerWeek  = 7 * secondsPerDay
)

type PortfolioRow struct {
	ID          int64   `json:"id"`
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	Owner       string  `json:"owner"`
	LocalPath   string  `json:"localPath"`
	InLocal     bool    `json:"inLocal"`
	InGitHub    bool    `json:"inGithub"`
	Private     *bool   `json:"private"`
	Fork        bool    `json:"fork"`
	Archived    bool    `json:"archived"`
	Description string  `json:"description"`
	Language    string  `json:"language"`
	Stars       int     `json:"stars"`
	Commits     int     `json:"commits"`
	LastCommit  int64   `json:"lastCommit"`
	Spark       []int   `json:"spark"` // commits per week, oldest first
	OpenIssues  *int    `json:"openIssues"`
	OpenPRs     *int    `json:"openPrs"`
	CI          string  `json:"ci"`         // conclusion of the latest default-branch run
	AgentShare  float64 `json:"agentShare"` // share of commits with an agent in the last 90 days
	Sessions    int     `json:"sessions"`
	Tokens      int64   `json:"tokens"`
	// FreshTokens is input + output without cache reads and writes, which dominate the total.
	FreshTokens int64   `json:"freshTokens"`
	CostUSD     float64 `json:"costUsd"`
	Worktrees   int     `json:"worktrees"`
	// SyncedAt and ScannedAt tell how fresh the GitHub and the local data are (0 = never).
	SyncedAt  int64 `json:"syncedAt"`
	ScannedAt int64 `json:"scannedAt"`
}

// Portfolio lists every repository with its headline numbers.
func Portfolio(ctx context.Context, db *sql.DB, now time.Time) ([]PortfolioRow, error) {
	return portfolio(ctx, db, now, nil, 0)
}

// PortfolioSince counts commits, sessions, tokens and cost from since on (0 = all time). The
// last commit, the sparkline (26 weeks) and the agent share (90 days) keep their own windows.
func PortfolioSince(ctx context.Context, db *sql.DB, now time.Time, since int64) ([]PortfolioRow, error) {
	return portfolio(ctx, db, now, nil, since)
}

// portfolio loads all repositories, or only the one with id when id is set.
func portfolio(ctx context.Context, db *sql.DB, now time.Time, id *int64, since int64) ([]PortfolioRow, error) {
	shareSince := now.Unix() - agentWindowDays*secondsPerDay
	where, args := "", []any{since, shareSince, since}
	if id != nil {
		where, args = "WHERE r.id=?", append(args, *id)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT r.id, r.key, r.name, IFNULL(r.owner,''), IFNULL(r.local_path,''), r.in_local, r.in_github,
		       r.is_private, IFNULL(r.is_fork,0), IFNULL(r.is_archived,0), IFNULL(r.description,''), IFNULL(r.stars,0),
		       r.open_issues, r.open_prs,
		       (SELECT COUNT(*) FROM commits c WHERE c.repo_id=r.id AND c.author_time>=?),
		       (SELECT IFNULL(MAX(author_time),0) FROM commits c WHERE c.repo_id=r.id),
		       (SELECT IFNULL(AVG(agents<>'' OR in_session),0) FROM commits c WHERE c.repo_id=r.id AND c.author_time>=?),
		       (SELECT lang FROM gh_language l WHERE l.repo_id=r.id ORDER BY bytes DESC LIMIT 1),
		       (SELECT IFNULL(conclusion,status) FROM gh_run g WHERE g.repo_id=r.id AND (r.default_branch IS NULL OR g.branch=r.default_branch) ORDER BY created_at DESC LIMIT 1),
		       (SELECT COUNT(*) FROM agent_session s WHERE s.repo_id=r.id AND s.parent_id IS NULL AND s.started_at>=?),
		       (SELECT COUNT(*) FROM worktree w WHERE w.repo_id=r.id),
		       IFNULL(r.gh_synced_at,0), IFNULL(r.scanned_at,0)
		FROM repo r `+where+` ORDER BY r.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("portfolio query: %w", err)
	}
	defer rows.Close()
	out := []PortfolioRow{}
	byID := map[int64]int{}
	for rows.Next() {
		var p PortfolioRow
		var priv sql.NullBool
		var oi, op sql.NullInt64
		var lang, ci sql.NullString
		if err := rows.Scan(&p.ID, &p.Key, &p.Name, &p.Owner, &p.LocalPath, &p.InLocal, &p.InGitHub,
			&priv, &p.Fork, &p.Archived, &p.Description, &p.Stars, &oi, &op,
			&p.Commits, &p.LastCommit, &p.AgentShare, &lang, &ci, &p.Sessions, &p.Worktrees,
			&p.SyncedAt, &p.ScannedAt); err != nil {
			return nil, fmt.Errorf("scan portfolio: %w", err)
		}
		p.Private = nullBool(priv)
		p.OpenIssues, p.OpenPRs = nullInt(oi), nullInt(op)
		p.Language, p.CI = lang.String, ci.String
		p.Spark = make([]int, sparkWeeks)
		byID[p.ID] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("portfolio rows: %w", err)
	}
	if err := fillTokens(ctx, db, since, out, byID); err != nil {
		return nil, err
	}
	return out, fillSparks(ctx, db, now, out, byID)
}

// fillTokens adds tokens and cost per repository in one grouped query (per-row subqueries over
// the thread-to-parent join are slow on large histories). Threads count for their parent.
func fillTokens(ctx context.Context, db *sql.DB, since int64, out []PortfolioRow, byID map[int64]int) error {
	return queryEach(ctx, db, `SELECT s.repo_id, SUM(m.input+m.output+m.cache_read+m.cache_write), SUM(m.input+m.output), IFNULL(SUM(m.cost_usd),0)
		FROM session_model m `+joinRoot+` WHERE s.repo_id IS NOT NULL AND s.started_at>=? GROUP BY s.repo_id`, []any{since},
		func(r *sql.Rows) error {
			var id, tokens, fresh int64
			var cost float64
			if err := r.Scan(&id, &tokens, &fresh, &cost); err != nil {
				return err
			}
			if i, ok := byID[id]; ok {
				out[i].Tokens, out[i].FreshTokens, out[i].CostUSD = tokens, fresh, cost
			}
			return nil
		})
}

func fillSparks(ctx context.Context, db *sql.DB, now time.Time, out []PortfolioRow, byID map[int64]int) error {
	start := now.Unix() - sparkWeeks*secondsPerWeek
	rows, err := db.QueryContext(ctx, `SELECT repo_id, (author_time-?)/?, COUNT(*) FROM commits
		WHERE author_time>=? GROUP BY 1,2`, start, secondsPerWeek, start)
	if err != nil {
		return fmt.Errorf("spark query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var week, n int
		if err := rows.Scan(&id, &week, &n); err != nil {
			return fmt.Errorf("scan spark: %w", err)
		}
		if i, ok := byID[id]; ok && week >= 0 && week < sparkWeeks {
			out[i].Spark[week] = n
		}
	}
	return rows.Err()
}

func nullBool(b sql.NullBool) *bool {
	if !b.Valid {
		return nil
	}
	return &b.Bool
}

func nullInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

type DayType struct {
	Day   string `json:"day"`
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type DayLines struct {
	Day       string `json:"day"`
	Additions int    `json:"add"`
	Deletions int    `json:"del"`
}

type WeekIssues struct {
	Week         string `json:"week"`
	IssuesOpened int    `json:"issuesOpened"`
	IssuesClosed int    `json:"issuesClosed"`
	PRsOpened    int    `json:"prsOpened"`
	PRsMerged    int    `json:"prsMerged"`
}

type Author struct {
	PersonID  int64    `json:"personId"`
	Name      string   `json:"name"`
	Emails    []string `json:"emails"`
	Commits   int      `json:"commits"`
	Additions int      `json:"add"`
	Deletions int      `json:"del"`
	First     int64    `json:"first"`
	Last      int64    `json:"last"`
	Bot       bool     `json:"bot"`
}

type DirStat struct {
	Dir       string `json:"dir"`
	Commits   int    `json:"commits"`
	Additions int    `json:"add"`
	Deletions int    `json:"del"`
}

type CommitRow struct {
	Hash      string `json:"hash"`
	Time      int64  `json:"time"`
	Author    string `json:"author"`
	Subject   string `json:"subject"`
	Type      string `json:"type"`
	Add       int    `json:"add"`
	Del       int    `json:"del"`
	Agents    string `json:"agents"`
	InSession bool   `json:"inSession"`
	Source    string `json:"source"`
}

type Milestone struct {
	Title  string `json:"title"`
	State  string `json:"state"`
	DueOn  *int64 `json:"dueOn"`
	Open   int    `json:"open"`
	Closed int    `json:"closed"`
}

type Release struct {
	Tag        string `json:"tag"`
	Name       string `json:"name"`
	Published  int64  `json:"published"`
	Prerelease bool   `json:"prerelease"`
}

type CIStats struct {
	Workflow    string   `json:"workflow"`
	Runs        int      `json:"runs"`
	SuccessRate *float64 `json:"successRate"` // nil when no run succeeded or failed (all cancelled/running)
	AvgSeconds  float64  `json:"avgSeconds"`
	Last        string   `json:"last"`
}

type Lang struct {
	Lang  string `json:"lang"`
	Bytes int64  `json:"bytes"`
}

type IssueTimes struct {
	MedianIssueCloseHours *float64 `json:"medianIssueCloseHours"`
	MedianPRMergeHours    *float64 `json:"medianPrMergeHours"`
	OpenIssues            int      `json:"openIssues"`
	OpenPRs               int      `json:"openPrs"`
}

type RepoDetail struct {
	Repo       PortfolioRow `json:"repo"`
	Worktrees  []Worktree   `json:"worktrees"`
	DailyTypes []DayType    `json:"dailyTypes"`
	DailyLines []DayLines   `json:"dailyLines"`
	Issues     []WeekIssues `json:"issues"`
	IssueTimes IssueTimes   `json:"issueTimes"`
	Milestones []Milestone  `json:"milestones"`
	Authors    []Author     `json:"authors"`
	Dirs       []DirStat    `json:"dirs"`
	Recent     []CommitRow  `json:"recent"`
	Releases   []Release    `json:"releases"`
	CI         []CIStats    `json:"ci"`
	Languages  []Lang       `json:"languages"`
	Labels     []string     `json:"labels"`
	Agents     *AgentView   `json:"agents"`
	Open       []OpenItem   `json:"open"`
	// Since is the start of the period (0 = all time): it limits commits, lines, authors,
	// directories, issue activity, CI runs and agent sessions. Open items and releases are current.
	Since int64 `json:"since"`
}

// OpenItem is an open issue or pull request, oldest first so the stale ones stand out.
type OpenItem struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	IsPR    bool   `json:"isPr"`
	Created int64  `json:"created"`
	Author  string `json:"author"`
	Labels  string `json:"labels"`
}

type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
}
