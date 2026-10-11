package collect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"repolens/internal/config"
	"repolens/internal/github"
	"repolens/internal/gitlog"
	"repolens/internal/store"
)

const (
	sourceGitHub = "github-api"
	githubHost   = "github.com"
	// syncOverlap re-reads a little before the last sync to absorb clock skew.
	syncOverlap = 10 * time.Minute
	// runRefresh re-reads recent CI runs that may still have been running at the last sync.
	runRefresh = 72 * time.Hour
	// historyOverlap re-reads API history: GitHub filters history by commit date, so a branch
	// merged later can bring commits dated before the last sync.
	historyOverlap = 14 * 24 * time.Hour
)

// dbCache stores REST responses in http_cache. The token is never part of the key or body.
type dbCache struct{ db *sql.DB }

func (c dbCache) Get(ctx context.Context, url string) (github.Cached, bool) {
	var e github.Cached
	err := c.db.QueryRowContext(ctx, `SELECT IFNULL(etag,''), IFNULL(link,''), body FROM http_cache WHERE url=?`, url).Scan(&e.ETag, &e.Link, &e.Body)
	if err != nil {
		return e, false // cache miss (or unreadable row): fetch fresh
	}
	return e, true
}

func (c dbCache) Put(ctx context.Context, url string, e github.Cached) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO http_cache(url,etag,link,body,fetched_at) VALUES(?,?,?,?,?)
		ON CONFLICT(url) DO UPDATE SET etag=excluded.etag, link=excluded.link, body=excluded.body, fetched_at=excluded.fetched_at`,
		url, e.ETag, e.Link, e.Body, time.Now().Unix())
	return err
}

// githubAPI overrides the API root in tests. It is deliberately not configurable from outside:
// the token must never be sent anywhere but api.github.com.
var githubAPI string

func newGitHubClient(ctx context.Context, st *store.Store, log io.Writer) (*github.Client, error) {
	token, err := github.Token(ctx)
	if err != nil {
		return nil, err
	}
	c := github.New(token, dbCache{st.DB}, log)
	if githubAPI != "" {
		c.WithBase(githubAPI)
	}
	return c, nil
}

// ghRepo pairs a repository row with its GitHub coordinates and last sync time.
type ghRepo struct {
	ID       int64
	Ref      github.Ref
	InLocal  bool
	LastSync time.Time // zero when never synced
}

// syncWorkers bounds concurrent repositories; GitHub discourages heavy parallelism.
const syncWorkers = 4

// syncResult is what one repository sync produced; it is merged into Summary by one goroutine.
type syncResult struct {
	repos, commits int
	err            error
	line           string
}

func lastSync(ctx context.Context, st *store.Store, id int64) (time.Time, error) {
	var last sql.NullInt64
	if err := st.DB.QueryRowContext(ctx, `SELECT gh_synced_at FROM repo WHERE id=?`, id).Scan(&last); err != nil {
		return time.Time{}, fmt.Errorf("read sync time: %w", err)
	}
	if !last.Valid {
		return time.Time{}, nil
	}
	return time.Unix(last.Int64, 0).Add(-syncOverlap), nil
}

// EnrichLocal adds GitHub data to local repositories that have a github.com remote.
func EnrichLocal(ctx context.Context, st *store.Store, cfg config.Config, c *github.Client, repos []LocalRepo, log io.Writer, sum *Summary) {
	syncAll(ctx, st, cfg, c, localTargets(ctx, st, cfg, repos, sum), log, sum)
}

// localTargets returns the local repositories with a github.com remote that are not excluded.
func localTargets(ctx context.Context, st *store.Store, cfg config.Config, repos []LocalRepo, sum *Summary) []ghRepo {
	var targets []ghRepo
	for _, r := range repos {
		if !r.HasRemote || r.Remote.Host != githubHost || cfg.ExcludedRepo(r.Remote.Key()) {
			continue
		}
		since, err := lastSync(ctx, st, r.ID)
		if err != nil {
			sum.fail(err)
			continue
		}
		targets = append(targets, ghRepo{ID: r.ID, Ref: github.Ref{Owner: r.Remote.Owner, Name: r.Remote.Name}, InLocal: true, LastSync: since})
	}
	return targets
}

// Owner collects all repositories of a GitHub user or organisation (mode A).
func Owner(ctx context.Context, st *store.Store, cfg config.Config, c *github.Client, login string, log io.Writer, sum *Summary) error {
	targets, err := accountTargets(ctx, st, cfg, c, login, log, sum)
	if err != nil {
		return err
	}
	syncAll(ctx, st, cfg, c, targets, log, sum)
	return nil
}

// accountTargets lists the repositories of login, stores the new ones and returns them all.
func accountTargets(ctx context.Context, st *store.Store, cfg config.Config, c *github.Client, login string, log io.Writer, sum *Summary) ([]ghRepo, error) {
	fmt.Fprintf(log, "GitHub: seznam repozitářů %s…\n", login)
	metas, err := c.ListOwnerRepos(ctx, login)
	if err != nil {
		return nil, err
	}
	var targets []ghRepo
	for i := range metas {
		m := &metas[i]
		if (m.IsFork && !cfg.IncludeForks) || (m.IsArchived && !cfg.IncludeArchived) {
			sum.Skipped++
			continue
		}
		ref := m.Ref()
		key := githubHost + "/" + strings.ToLower(ref.Owner) + "/" + strings.ToLower(ref.Name)
		if cfg.ExcludedRepo(key) {
			sum.Skipped++
			continue
		}
		var id int64
		var inLocal bool
		err := st.DB.QueryRowContext(ctx, `INSERT INTO repo(key,name,host,owner,in_github) VALUES(?,?,?,?,1)
			ON CONFLICT(key) DO UPDATE SET in_github=1 RETURNING id, in_local`,
			key, strings.ToLower(ref.Name), githubHost, strings.ToLower(ref.Owner)).Scan(&id, &inLocal)
		if err != nil {
			sum.fail(fmt.Errorf("upsert %s: %w", key, err))
			continue
		}
		if inLocal {
			if inLocal, err = stillCloned(ctx, st, id); err != nil {
				sum.fail(err)
				continue
			}
		}
		since, err := lastSync(ctx, st, id)
		if err != nil {
			sum.fail(err)
			continue
		}
		targets = append(targets, ghRepo{ID: id, Ref: ref, InLocal: inLocal, LastSync: since})
	}
	return targets, nil
}

// stillCloned checks that a repository marked as cloned still has its clone on disk. A clone that
// was deleted or moved is unmarked, so its history comes from the API again instead of freezing.
func stillCloned(ctx context.Context, st *store.Store, id int64) (bool, error) {
	var path sql.NullString
	if err := st.DB.QueryRowContext(ctx, `SELECT local_path FROM repo WHERE id=?`, id).Scan(&path); err != nil {
		return false, fmt.Errorf("read clone path: %w", err)
	}
	if path.Valid && path.String != "" {
		if _, err := os.Stat(filepath.Join(path.String, ".git")); err == nil {
			return true, nil
		}
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE repo SET in_local=0 WHERE id=?`, id); err != nil {
		return false, fmt.Errorf("unmark clone: %w", err)
	}
	return false, nil
}

// mergeTargets keeps the first target of every repository, so a repository that is both cloned
// and listed under an account is synced once (as local, which keeps local commits authoritative).
func mergeTargets(lists ...[]ghRepo) []ghRepo {
	seen := map[int64]bool{}
	var out []ghRepo
	for _, list := range lists {
		for _, t := range list {
			if !seen[t.ID] {
				seen[t.ID] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// syncAll fetches batched metadata, then syncs repositories with a small worker pool.
func syncAll(ctx context.Context, st *store.Store, cfg config.Config, c *github.Client, targets []ghRepo, log io.Writer, sum *Summary) {
	reqs := make([]github.MetaRequest, len(targets))
	for i, t := range targets {
		reqs[i] = github.MetaRequest{Ref: t.Ref, Since: t.LastSync}
	}
	fmt.Fprintf(log, "GitHub: metadata pro %d repozitářů…\n", len(reqs))
	metas, skipped, err := c.FetchMeta(ctx, reqs)
	if err != nil {
		sum.fail(fmt.Errorf("GitHub metadata: %w", err))
		return
	}
	for i, m := range metas {
		if m == nil {
			ref := targets[i].Ref
			sum.failAt(githubHost+"/"+strings.ToLower(ref.Owner+"/"+ref.Name),
				fmt.Errorf("%s/%s: na GitHubu nedostupný: %s", ref.Owner, ref.Name, skipped[i]))
		}
	}
	jobs := make(chan int)
	results := make(chan syncResult)
	var wg sync.WaitGroup
	for range syncWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results <- syncRepo(ctx, st, cfg, c, targets[i], metas[i])
			}
		}()
	}
	go func() {
		for i, m := range metas {
			if m != nil {
				jobs <- i
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	for r := range results {
		if r.err != nil {
			sum.fail(r.err)
			continue
		}
		sum.Repos += r.repos
		sum.Commits += r.commits
		if r.line != "" {
			fmt.Fprintln(log, r.line)
		}
	}
}

// issuesSince uses the page bundled with the metadata, paginating only when it is not enough.
func issuesSince(ctx context.Context, c *github.Client, r ghRepo, page *github.IssuePage, prs bool) ([]github.Issue, error) {
	if page != nil && page.Complete(r.LastSync) {
		return page.Since(r.LastSync, prs), nil
	}
	return c.FetchIssues(ctx, r.Ref, prs, r.LastSync)
}

func syncRepo(ctx context.Context, st *store.Store, cfg config.Config, c *github.Client, r ghRepo, m *github.RepoMeta) syncResult {
	name := r.Ref.Owner + "/" + r.Ref.Name
	started := time.Now()
	issues, err := issuesSince(ctx, c, r, m.RecentIssues, false)
	if err != nil {
		return syncResult{err: fmt.Errorf("%s issues: %w", name, err)}
	}
	prs, err := issuesSince(ctx, c, r, m.RecentPRs, true)
	if err != nil {
		return syncResult{err: fmt.Errorf("%s PR: %w", name, err)}
	}
	// Runs are re-read for a few days back so queued or running ones get their final state.
	runsSince := started.AddDate(0, 0, -cfg.CIDays)
	if refresh := r.LastSync.Add(-runRefresh); r.LastSync.After(runsSince) && refresh.After(runsSince) {
		runsSince = refresh
	}
	runs, err := c.FetchRuns(ctx, r.Ref, runsSince)
	if err != nil {
		// Repositories with Actions disabled answer 404; that is not a failure of the run.
		var apiErr *github.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			return syncResult{err: fmt.Errorf("%s actions: %w", name, err)}
		}
	}
	res := syncResult{}
	if !r.InLocal {
		if res.commits, err = syncHistory(ctx, st, c, r); err != nil {
			return syncResult{err: fmt.Errorf("%s historie: %w", name, err)}
		}
		res.repos = 1
	}
	err = st.Tx(ctx, func(tx *sql.Tx) error {
		if err := writeMeta(tx, r.ID, m); err != nil {
			return err
		}
		if err := writeIssues(tx, r.ID, append(issues, prs...)); err != nil {
			return err
		}
		if err := writeRuns(tx, r.ID, runs); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE repo SET gh_synced_at=? WHERE id=?`, started.Unix(), r.ID)
		return err
	})
	if err != nil {
		return syncResult{err: fmt.Errorf("%s uložení: %w", name, err)}
	}
	if len(issues)+len(prs)+len(runs)+res.commits > 0 {
		res.line = fmt.Sprintf("  %-50s %d issues, %d PR, %d CI běhů, %d commitů", name, len(issues), len(prs), len(runs), res.commits)
	}
	return res
}

func writeMeta(tx *sql.Tx, id int64, m *github.RepoMeta) error {
	var pushed any
	if m.PushedAt != nil {
		pushed = m.PushedAt.Unix()
	}
	if _, err := tx.Exec(`UPDATE repo SET in_github=1, description=?, default_branch=?, is_private=?, is_fork=?, is_archived=?,
		stars=?, forks=?, open_issues=?, open_prs=?, pushed_at=? WHERE id=?`,
		m.Description, nullIfEmpty(m.DefaultBranch()), m.IsPrivate, m.IsFork, m.IsArchived,
		m.StargazerCount, m.ForkCount, m.Issues.TotalCount, m.PullRequests.TotalCount, pushed, id); err != nil {
		return fmt.Errorf("update repo meta: %w", err)
	}
	for _, t := range []string{"gh_language", "gh_milestone", "gh_label", "gh_release"} {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE repo_id=?`, id); err != nil {
			return fmt.Errorf("clear %s: %w", t, err)
		}
	}
	for _, e := range m.Languages.Edges {
		if _, err := tx.Exec(`INSERT INTO gh_language(repo_id,lang,bytes) VALUES(?,?,?)`, id, e.Node.Name, e.Size); err != nil {
			return fmt.Errorf("insert language: %w", err)
		}
	}
	for _, ms := range m.Milestones.Nodes {
		var due any
		if ms.DueOn != nil {
			due = ms.DueOn.Unix()
		}
		if _, err := tx.Exec(`INSERT INTO gh_milestone(repo_id,number,title,state,due_on,open_issues,closed_issues) VALUES(?,?,?,?,?,?,?)`,
			id, ms.Number, ms.Title, ms.State, due, ms.Open.TotalCount, ms.Closed.TotalCount); err != nil {
			return fmt.Errorf("insert milestone: %w", err)
		}
	}
	for _, l := range m.Labels.Nodes {
		if _, err := tx.Exec(`INSERT INTO gh_label(repo_id,name,color) VALUES(?,?,?)`, id, l.Name, l.Color); err != nil {
			return fmt.Errorf("insert label: %w", err)
		}
	}
	for _, rel := range m.Releases.Nodes {
		var pub any
		if rel.PublishedAt != nil {
			pub = rel.PublishedAt.Unix()
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO gh_release(repo_id,tag,name,published_at,prerelease) VALUES(?,?,?,?,?)`,
			id, rel.TagName, rel.Name, pub, rel.IsPrerelease); err != nil {
			return fmt.Errorf("insert release: %w", err)
		}
	}
	return nil
}

func unixOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func writeIssues(tx *sql.Tx, id int64, items []github.Issue) error {
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO gh_issue(repo_id,number,is_pr,state,title,author,created_at,closed_at,merged_at,milestone,labels,source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare issue insert: %w", err)
	}
	defer stmt.Close()
	for _, it := range items {
		var ms any
		if it.Milestone != nil {
			ms = it.Milestone.Title
		}
		if _, err := stmt.Exec(id, it.Number, it.IsPR, it.State, it.Title, it.AuthorLogin(), it.CreatedAt.Unix(),
			unixOrNil(it.ClosedAt), unixOrNil(it.MergedAt), ms, strings.Join(it.LabelNames(), ","), sourceGitHub); err != nil {
			return fmt.Errorf("insert issue %d: %w", it.Number, err)
		}
	}
	return nil
}

func writeRuns(tx *sql.Tx, id int64, runs []github.Run) error {
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO gh_run(repo_id,id,workflow,event,branch,status,conclusion,created_at,duration_s) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare run insert: %w", err)
	}
	defer stmt.Close()
	for _, r := range runs {
		var dur any
		if d := r.Duration(); d > 0 {
			dur = int64(d.Seconds())
		}
		if _, err := stmt.Exec(id, r.ID, r.Name, r.Event, r.HeadBranch, r.Status, r.Conclusion, r.CreatedAt.Unix(), dur); err != nil {
			return fmt.Errorf("insert run %d: %w", r.ID, err)
		}
	}
	return nil
}

// historySince is where API history reading starts: the last sync minus an overlap. It never
// depends on commit dates, which a single future-dated commit would push out of reach.
func historySince(lastSync time.Time) time.Time {
	if lastSync.IsZero() {
		return time.Time{}
	}
	return lastSync.Add(-historyOverlap)
}

// syncHistory reads default-branch commits from the API for repositories without a local clone.
// The network fetch happens before the write transaction so the single DB connection is not held.
func syncHistory(ctx context.Context, st *store.Store, c *github.Client, r ghRepo) (int, error) {
	since := historySince(r.LastSync)
	var fetched []github.HistoryCommit
	if err := c.FetchHistory(ctx, r.Ref, since, func(h github.HistoryCommit) error {
		fetched = append(fetched, h)
		return nil
	}); err != nil {
		return 0, err
	}
	inserted := 0
	err := st.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO commits(repo_id,hash,author_name,author_email,author_time,subject,ctype,is_merge,additions,deletions,files,agents,source)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return fmt.Errorf("prepare commit insert: %w", err)
		}
		defer stmt.Close()
		for _, h := range fetched {
			res, err := stmt.Exec(r.ID, h.Hash, h.AuthorName, strings.ToLower(h.AuthorEmail), h.AuthorTime.Unix(), h.Subject,
				gitlog.ClassifyType(h.Subject, h.IsMerge()), boolInt(h.IsMerge()), h.Additions, h.Deletions, h.Files,
				strings.Join(gitlog.DetectAgents(h.Commit), ","), sourceGitHub)
			if err != nil {
				return fmt.Errorf("insert commit %s: %w", h.Hash, err)
			}
			if n, err := res.RowsAffected(); err == nil {
				inserted += int(n)
			}
		}
		return nil
	})
	return inserted, err
}
