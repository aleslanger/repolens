package report

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RepoDetailFor assembles everything the repository page shows; nil when the id is unknown.
func RepoDetailFor(ctx context.Context, db *sql.DB, now time.Time, id int64) (*RepoDetail, error) {
	return RepoDetailSince(ctx, db, now, id, 0)
}

// RepoDetailSince is RepoDetailFor limited to activity from since on (0 = all time).
func RepoDetailSince(ctx context.Context, db *sql.DB, now time.Time, id, since int64) (*RepoDetail, error) {
	rows, err := portfolio(ctx, db, now, &id, since)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return repoDetail(ctx, db, rows[0], since)
}

// RepoDetailFrom builds the all-time detail for an already loaded portfolio row.
func RepoDetailFrom(ctx context.Context, db *sql.DB, row PortfolioRow) (*RepoDetail, error) {
	return repoDetail(ctx, db, row, 0)
}

func repoDetail(ctx context.Context, db *sql.DB, row PortfolioRow, since int64) (*RepoDetail, error) {
	id := row.ID
	d := RepoDetail{Repo: row, Since: since}
	steps := []func(context.Context, *sql.DB, int64, *RepoDetail) error{
		loadWorktrees, loadDaily, loadIssues, loadIssueTimes, loadMilestones, loadAuthors,
		loadDirs, loadRecent, loadReleases, loadCI, loadLanguages, loadLabels, loadOpen,
	}
	for _, step := range steps {
		if err := step(ctx, db, id, &d); err != nil {
			return nil, err
		}
	}
	av, err := AgentsSince(ctx, db, &id, since)
	if err != nil {
		return nil, err
	}
	d.Agents = av
	return &d, nil
}

// queryEach runs q and calls scan for each row.
func queryEach(ctx context.Context, db *sql.DB, q string, args []any, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
	}
	return rows.Err()
}

func loadWorktrees(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Worktrees = []Worktree{}
	return queryEach(ctx, db, `SELECT path, IFNULL(branch,'') FROM worktree WHERE repo_id=? ORDER BY path`, []any{id},
		func(r *sql.Rows) error {
			var w Worktree
			if err := r.Scan(&w.Path, &w.Branch); err != nil {
				return err
			}
			d.Worktrees = append(d.Worktrees, w)
			return nil
		})
}

const dayExpr = `date(author_time,'unixepoch','localtime')`

func loadDaily(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.DailyTypes, d.DailyLines = []DayType{}, []DayLines{}
	err := queryEach(ctx, db, `SELECT `+dayExpr+`, ctype, COUNT(*) FROM commits WHERE repo_id=? AND author_time>=? GROUP BY 1,2 ORDER BY 1`, []any{id, d.Since},
		func(r *sql.Rows) error {
			var x DayType
			if err := r.Scan(&x.Day, &x.Type, &x.Count); err != nil {
				return err
			}
			d.DailyTypes = append(d.DailyTypes, x)
			return nil
		})
	if err != nil {
		return err
	}
	return queryEach(ctx, db, `SELECT `+dayExpr+`, SUM(additions), SUM(deletions) FROM commits WHERE repo_id=? AND is_merge=0 AND author_time>=? GROUP BY 1 ORDER BY 1`, []any{id, d.Since},
		func(r *sql.Rows) error {
			var x DayLines
			if err := r.Scan(&x.Day, &x.Additions, &x.Deletions); err != nil {
				return err
			}
			d.DailyLines = append(d.DailyLines, x)
			return nil
		})
}

func loadIssues(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	weeks := map[string]*WeekIssues{}
	get := func(w string) *WeekIssues {
		if weeks[w] == nil {
			weeks[w] = &WeekIssues{Week: w}
		}
		return weeks[w]
	}
	// Monday on or before the timestamp's local date.
	const weekExpr = `date(%s,'unixepoch','localtime','-6 days','weekday 1')`
	q := `SELECT ` + fmt.Sprintf(weekExpr, "created_at") + `, is_pr, 'open', COUNT(*) FROM gh_issue WHERE repo_id=? AND created_at>=? GROUP BY 1,2
		UNION ALL SELECT ` + fmt.Sprintf(weekExpr, "closed_at") + `, is_pr, 'closed', COUNT(*) FROM gh_issue WHERE repo_id=? AND closed_at IS NOT NULL AND merged_at IS NULL AND closed_at>=? GROUP BY 1,2
		UNION ALL SELECT ` + fmt.Sprintf(weekExpr, "merged_at") + `, is_pr, 'merged', COUNT(*) FROM gh_issue WHERE repo_id=? AND merged_at IS NOT NULL AND merged_at>=? GROUP BY 1,2`
	err := queryEach(ctx, db, q, []any{id, d.Since, id, d.Since, id, d.Since}, func(r *sql.Rows) error {
		var week, what string
		var isPR bool
		var n int
		if err := r.Scan(&week, &isPR, &what, &n); err != nil {
			return err
		}
		w := get(week)
		switch {
		case !isPR && what == "open":
			w.IssuesOpened += n
		case !isPR:
			w.IssuesClosed += n
		case what == "open":
			w.PRsOpened += n
		case what == "merged":
			w.PRsMerged += n
		}
		return nil
	})
	if err != nil {
		return err
	}
	d.Issues = make([]WeekIssues, 0, len(weeks))
	for _, w := range weeks {
		d.Issues = append(d.Issues, *w)
	}
	sort.Slice(d.Issues, func(i, j int) bool { return d.Issues[i].Week < d.Issues[j].Week })
	return nil
}

func loadIssueTimes(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	var issueH, prH []float64
	err := queryEach(ctx, db, `SELECT is_pr, state, created_at, closed_at, merged_at FROM gh_issue WHERE repo_id=?`, []any{id},
		func(r *sql.Rows) error {
			var isPR bool
			var state string
			var created int64
			var closed, merged sql.NullInt64
			if err := r.Scan(&isPR, &state, &created, &closed, &merged); err != nil {
				return err
			}
			switch {
			case state == "OPEN" && isPR:
				d.IssueTimes.OpenPRs++
			case state == "OPEN":
				d.IssueTimes.OpenIssues++
			case isPR && merged.Valid && merged.Int64 >= d.Since:
				prH = append(prH, float64(merged.Int64-created)/3600)
			case !isPR && closed.Valid && closed.Int64 >= d.Since:
				issueH = append(issueH, float64(closed.Int64-created)/3600)
			}
			return nil
		})
	d.IssueTimes.MedianIssueCloseHours = median(issueH)
	d.IssueTimes.MedianPRMergeHours = median(prH)
	return err
}

func median(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	sort.Float64s(v)
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		m = (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	return &m
}

func loadMilestones(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Milestones = []Milestone{}
	return queryEach(ctx, db, `SELECT title, state, due_on, open_issues, closed_issues FROM gh_milestone WHERE repo_id=? ORDER BY state DESC, due_on`, []any{id},
		func(r *sql.Rows) error {
			var m Milestone
			var due sql.NullInt64
			if err := r.Scan(&m.Title, &m.State, &due, &m.Open, &m.Closed); err != nil {
				return err
			}
			if due.Valid {
				m.DueOn = &due.Int64
			}
			d.Milestones = append(d.Milestones, m)
			return nil
		})
}

// authorSQL groups commits by merged identity; unmapped identities fall back to their e-mail.
const authorSQL = `
	SELECT IFNULL(p.id, 0), IFNULL(p.name, c.author_name), IFNULL(p.is_bot, 0),
	       GROUP_CONCAT(DISTINCT c.author_email), COUNT(*), SUM(c.additions), SUM(c.deletions),
	       MIN(c.author_time), MAX(c.author_time)
	FROM commits c
	LEFT JOIN identity i ON i.name=c.author_name AND i.email=c.author_email
	LEFT JOIN person p ON p.id=i.person_id
	%s
	GROUP BY IFNULL(CAST(p.id AS TEXT), 'e:'||c.author_email)
	ORDER BY COUNT(*) DESC`

func scanAuthor(r *sql.Rows) (Author, error) {
	var a Author
	var emails string
	if err := r.Scan(&a.PersonID, &a.Name, &a.Bot, &emails, &a.Commits, &a.Additions, &a.Deletions, &a.First, &a.Last); err != nil {
		return a, err
	}
	a.Emails = strings.Split(emails, ",")
	return a, nil
}

func loadAuthors(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Authors = []Author{}
	return queryEach(ctx, db, fmt.Sprintf(authorSQL, "WHERE c.repo_id=? AND c.author_time>=?"), []any{id, d.Since}, func(r *sql.Rows) error {
		a, err := scanAuthor(r)
		if err != nil {
			return err
		}
		d.Authors = append(d.Authors, a)
		return nil
	})
}

func loadDirs(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Dirs = []DirStat{}
	return queryEach(ctx, db, `SELECT x.dir, COUNT(*), SUM(x.additions), SUM(x.deletions) FROM commit_dir x
		JOIN commits c ON c.repo_id=x.repo_id AND c.hash=x.hash WHERE x.repo_id=? AND c.author_time>=?
		GROUP BY x.dir ORDER BY COUNT(*) DESC, SUM(x.additions+x.deletions) DESC LIMIT ?`, []any{id, d.Since, topDirs},
		func(r *sql.Rows) error {
			var x DirStat
			if err := r.Scan(&x.Dir, &x.Commits, &x.Additions, &x.Deletions); err != nil {
				return err
			}
			d.Dirs = append(d.Dirs, x)
			return nil
		})
}

func loadRecent(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Recent = []CommitRow{}
	return queryEach(ctx, db, `SELECT hash, author_time, author_name, subject, ctype, additions, deletions, agents, in_session, source
		FROM commits WHERE repo_id=? ORDER BY author_time DESC LIMIT ?`, []any{id, recentCommits},
		func(r *sql.Rows) error {
			var c CommitRow
			if err := r.Scan(&c.Hash, &c.Time, &c.Author, &c.Subject, &c.Type, &c.Add, &c.Del, &c.Agents, &c.InSession, &c.Source); err != nil {
				return err
			}
			d.Recent = append(d.Recent, c)
			return nil
		})
}

func loadReleases(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Releases = []Release{}
	return queryEach(ctx, db, `SELECT tag, IFNULL(name,''), IFNULL(published_at,0), prerelease FROM gh_release WHERE repo_id=? ORDER BY published_at DESC`, []any{id},
		func(r *sql.Rows) error {
			var x Release
			if err := r.Scan(&x.Tag, &x.Name, &x.Published, &x.Prerelease); err != nil {
				return err
			}
			d.Releases = append(d.Releases, x)
			return nil
		})
}

func loadCI(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.CI = []CIStats{}
	return queryEach(ctx, db, `SELECT workflow, COUNT(*),
		    AVG(CASE WHEN conclusion='success' THEN 1.0 WHEN conclusion IN ('failure','timed_out','startup_failure') THEN 0.0 END),
		    IFNULL(AVG(duration_s),0),
		    (SELECT IFNULL(conclusion,status) FROM gh_run g2 WHERE g2.repo_id=g.repo_id AND g2.workflow=g.workflow ORDER BY created_at DESC LIMIT 1)
		FROM gh_run g WHERE repo_id=? AND created_at>=? GROUP BY workflow ORDER BY COUNT(*) DESC`, []any{id, d.Since},
		func(r *sql.Rows) error {
			var x CIStats
			var rate sql.NullFloat64
			if err := r.Scan(&x.Workflow, &x.Runs, &rate, &x.AvgSeconds, &x.Last); err != nil {
				return err
			}
			if rate.Valid {
				x.SuccessRate = &rate.Float64
			}
			d.CI = append(d.CI, x)
			return nil
		})
}

func loadLanguages(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Languages = []Lang{}
	return queryEach(ctx, db, `SELECT lang, bytes FROM gh_language WHERE repo_id=? ORDER BY bytes DESC`, []any{id},
		func(r *sql.Rows) error {
			var x Lang
			if err := r.Scan(&x.Lang, &x.Bytes); err != nil {
				return err
			}
			d.Languages = append(d.Languages, x)
			return nil
		})
}

func loadLabels(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Labels = []string{}
	return queryEach(ctx, db, `SELECT name FROM gh_label WHERE repo_id=? ORDER BY name`, []any{id},
		func(r *sql.Rows) error {
			var s string
			if err := r.Scan(&s); err != nil {
				return err
			}
			d.Labels = append(d.Labels, s)
			return nil
		})
}

// openItemsMax bounds the open issues and pull requests listed on the repository page.
const openItemsMax = 50

func loadOpen(ctx context.Context, db *sql.DB, id int64, d *RepoDetail) error {
	d.Open = []OpenItem{}
	return queryEach(ctx, db, `SELECT number, title, is_pr, created_at, IFNULL(author,''), labels FROM gh_issue
		WHERE repo_id=? AND state='OPEN' ORDER BY created_at LIMIT ?`, []any{id, openItemsMax},
		func(r *sql.Rows) error {
			var x OpenItem
			if err := r.Scan(&x.Number, &x.Title, &x.IsPR, &x.Created, &x.Author, &x.Labels); err != nil {
				return err
			}
			d.Open = append(d.Open, x)
			return nil
		})
}
