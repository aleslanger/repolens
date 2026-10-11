// Package collect orchestrates collectors and writes their results to the store.
package collect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"repolens/internal/config"
	"repolens/internal/gitlog"
	"repolens/internal/scan"
	"repolens/internal/store"
)

const sourceGit = "git-local"

// Summary is what a collection run reports at the end.
type Summary struct {
	Repos, Skipped, Commits, Sessions, Unrecognized, Errors int
	Failures                                                []Failure
}

// Failure is one error of a run. Target is the directory or repository key it concerns (empty
// when there is none), so the dashboard can offer to exclude it.
type Failure struct {
	Target  string `json:"target"`
	Message string `json:"message"`
}

const maxErrorSamples = 20

func (s *Summary) fail(err error) { s.failAt("", err) }

func (s *Summary) failAt(target string, err error) {
	s.Errors++
	if len(s.Failures) < maxErrorSamples {
		s.Failures = append(s.Failures, Failure{Target: target, Message: err.Error()})
	}
}

// LocalRepo is a scanned repository that passed the ownership filter, with its DB id.
type LocalRepo struct {
	scan.Repo
	ID int64
}

// Dir scans root, stores own repositories and their git history.
func Dir(ctx context.Context, st *store.Store, cfg config.Config, root string, log io.Writer, sum *Summary) ([]LocalRepo, error) {
	return Dirs(ctx, st, cfg, []config.Root{{Path: root, Depth: cfg.ScanDepth}}, log, sum)
}

// Dirs scans every root. Clones of the same remote and paths reachable from overlapping roots
// are counted once across all roots. A root that does not exist fails the run before anything
// is scanned.
func Dirs(ctx context.Context, st *store.Store, cfg config.Config, roots []config.Root, log io.Writer, sum *Summary) ([]LocalRepo, error) {
	for _, r := range roots {
		if err := checkRoot(r.Path); err != nil {
			return nil, err
		}
	}
	d := &dirScan{st: st, cfg: cfg, log: log, sum: sum, firstClone: map[string]LocalRepo{}, seen: map[string]bool{}}
	for _, r := range roots {
		fmt.Fprintf(log, "Skenuji %s (hloubka %d)…\n", r.Path, r.Depth)
		if err := d.root(ctx, r); err != nil {
			return nil, err
		}
	}
	return d.out, nil
}

// dirScan is the state shared by the roots of one collection.
type dirScan struct {
	st         *store.Store
	cfg        config.Config
	log        io.Writer
	sum        *Summary
	firstClone map[string]LocalRepo
	seen       map[string]bool
	out        []LocalRepo
}

func (d *dirScan) root(ctx context.Context, root config.Root) error {
	repos, scanStats, err := scan.Find(ctx, root.Path, root.Depth, d.cfg.ExcludedPath)
	if err != nil {
		return err
	}
	for _, e := range scanStats.Errors {
		d.sum.failAt(e.Path, errors.New(e.Err))
	}
	for _, r := range repos {
		if d.seen[r.Path] {
			continue
		}
		d.seen[r.Path] = true
		if !d.cfg.IsOwn(r) {
			d.sum.Skipped++
			continue
		}
		d.add(ctx, r)
	}
	return nil
}

func (d *dirScan) add(ctx context.Context, r scan.Repo) {
	key, _ := repoKey(r)
	if first, dup := d.firstClone[key]; dup {
		// Another clone of the same remote: map its path to the same repository (so its
		// sessions match) instead of letting the two clones overwrite each other.
		if err := addClonePath(ctx, d.st, first.ID, r.Path); err != nil {
			d.sum.fail(err)
		}
		fmt.Fprintf(d.log, "  %-50s další klon %s, započítán pod %s\n", shortPath(r.Path), key, shortPath(first.Path))
		return
	}
	id, err := upsertLocalRepo(ctx, d.st, r)
	if err != nil {
		d.sum.fail(err)
		return
	}
	n, err := syncCommits(ctx, d.st, d.cfg, id, r.Path, d.sum)
	if err != nil {
		d.sum.failAt(r.Path, fmt.Errorf("%s: %w", r.Path, err))
		return
	}
	d.sum.Repos++
	d.sum.Commits += n
	if n > 0 {
		fmt.Fprintf(d.log, "  %-50s +%d commitů\n", shortPath(r.Path), n)
	}
	lr := LocalRepo{Repo: r, ID: id}
	d.firstClone[key] = lr
	d.out = append(d.out, lr)
}

// checkRoot validates a scan root at the boundary: it must exist and be a directory.
func checkRoot(root string) error {
	st, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("adresář %s: %w", root, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s není adresář", root)
	}
	return nil
}

// cloneBranch marks worktree rows that are really additional clones of the same remote.
const cloneBranch = "(další klon)"

func addClonePath(ctx context.Context, st *store.Store, repoID int64, path string) error {
	_, err := st.DB.ExecContext(ctx, `INSERT INTO worktree(repo_id,path,branch) VALUES(?,?,?)
		ON CONFLICT(path) DO UPDATE SET repo_id=excluded.repo_id, branch=excluded.branch`, repoID, path, cloneBranch)
	if err != nil {
		return fmt.Errorf("map clone %s: %w", path, err)
	}
	return nil
}

func shortPath(p string) string {
	const maxLen = 50
	if len(p) <= maxLen {
		return p
	}
	return "…" + p[len(p)-maxLen+1:]
}

func repoKey(r scan.Repo) (key, name string) {
	if r.HasRemote {
		return r.Remote.Key(), r.Remote.Name
	}
	return "local:" + r.Path, filepath.Base(r.Path)
}

func upsertLocalRepo(ctx context.Context, st *store.Store, r scan.Repo) (int64, error) {
	key, name := repoKey(r)
	var id int64
	err := st.Tx(ctx, func(tx *sql.Tx) error {
		if err := migrateKey(tx, r.Path, key, name); err != nil {
			return err
		}
		err := tx.QueryRow(`INSERT INTO repo(key,name,host,owner,local_path,in_local) VALUES(?,?,?,?,?,1)
			ON CONFLICT(key) DO UPDATE SET local_path=excluded.local_path, in_local=1,
				host=COALESCE(excluded.host, repo.host), owner=COALESCE(excluded.owner, repo.owner)
			RETURNING id`, key, name, nullIfEmpty(r.Remote.Host), nullIfEmpty(r.Remote.Owner), r.Path).Scan(&id)
		if err == nil {
			_, err = tx.Exec(`UPDATE repo SET scanned_at=? WHERE id=?`, time.Now().Unix(), id)
		}
		if err != nil {
			return fmt.Errorf("upsert repo %s: %w", key, err)
		}
		if _, err := tx.Exec(`DELETE FROM worktree WHERE repo_id=? AND branch IS NOT ?`, id, cloneBranch); err != nil {
			return fmt.Errorf("clear worktrees: %w", err)
		}
		for _, w := range r.Worktrees {
			if _, err := tx.Exec(`INSERT INTO worktree(repo_id,path,branch) VALUES(?,?,?)
				ON CONFLICT(path) DO UPDATE SET repo_id=excluded.repo_id, branch=excluded.branch`, id, w.Path, w.Branch); err != nil {
				return fmt.Errorf("insert worktree: %w", err)
			}
		}
		return nil
	})
	return id, err
}

// migrateKey handles a repository whose identity changed (an origin was added or renamed):
// the row found by local path takes the new key, or gives way to an existing row with that key.
func migrateKey(tx *sql.Tx, path, key, name string) error {
	var oldID int64
	var oldKey string
	err := tx.QueryRow(`SELECT id, key FROM repo WHERE local_path=? AND key<>?`, path, key).Scan(&oldID, &oldKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find repo by path: %w", err)
	}
	var taken int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM repo WHERE key=?`, key).Scan(&taken); err != nil {
		return fmt.Errorf("check key: %w", err)
	}
	if taken == 0 {
		if _, err := tx.Exec(`UPDATE repo SET key=?, name=? WHERE id=?`, key, name, oldID); err != nil {
			return fmt.Errorf("rename repo %s: %w", oldKey, err)
		}
		return nil
	}
	// The new key already exists (e.g. collected from GitHub): keep it, drop the stale local row.
	if _, err := tx.Exec(`DELETE FROM repo WHERE id=?`, oldID); err != nil {
		return fmt.Errorf("drop stale repo %s: %w", oldKey, err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// syncCommits loads commits reachable from the repository's stable tips (see tips) that are new
// since the stored tips, or everything when history was rewritten or a shallow clone became
// complete. Checking out another branch does not change the numbers.
func syncCommits(ctx context.Context, st *store.Store, cfg config.Config, repoID int64, dir string, sum *Summary) (int, error) {
	t, err := tips(ctx, dir)
	if err != nil {
		return 0, err
	}
	if len(t.shas) == 0 {
		return 0, nil // no commits yet
	}
	current := t.state()
	var last sql.NullString
	if err := st.DB.QueryRowContext(ctx, `SELECT last_head FROM repo WHERE id=?`, repoID).Scan(&last); err != nil {
		return 0, fmt.Errorf("read last head: %w", err)
	}
	if last.String == current {
		return 0, nil
	}
	oldShas, wasShallow := parseState(last.String)
	var exclude []string
	// A clone that stopped being shallow gained older history: read everything again.
	if len(oldShas) > 0 && !(wasShallow && !t.shallow) && allReachable(ctx, dir, oldShas, t.shas) {
		exclude = oldShas
	}
	n := 0
	var gst gitlog.Stats
	err = st.Tx(ctx, func(tx *sql.Tx) error {
		if exclude == nil {
			// Full reload: local history replaces anything we had for this repo.
			if _, err := tx.Exec(`DELETE FROM commits WHERE repo_id=?`, repoID); err != nil {
				return fmt.Errorf("clear commits: %w", err)
			}
		}
		ins, err := newCommitWriter(tx, repoID, cfg.ModuleDepth, sourceGit)
		if err != nil {
			return err
		}
		defer ins.Close()
		gst, err = gitlog.Log(ctx, dir, t.shas, exclude, func(c gitlog.Commit) error {
			n++
			return ins.Write(c)
		})
		if err != nil {
			return err
		}
		issues := map[[2]string]int{}
		if gst.BadRecords > 0 {
			issues[[2]string{"error", "malformed commit record"}] = gst.BadRecords
		}
		if gst.BadLines > 0 {
			issues[[2]string{"error", "malformed numstat line"}] = gst.BadLines
		}
		if err := store.ReplaceIssues(tx, sourceGit, dir, issues); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE repo SET last_head=?, default_branch=COALESCE(default_branch, NULLIF(?, '')) WHERE id=?`, current, t.branch, repoID); err != nil {
			return fmt.Errorf("update head: %w", err)
		}
		return nil
	})
	if err == nil {
		sum.Errors += gst.BadRecords + gst.BadLines
	}
	return n, err
}

// tipSet is what syncCommits reads: commit ids, the default branch name and shallowness.
type tipSet struct {
	shas    []string
	branch  string
	shallow bool
}

const shallowMark = ";shallow"

// state is stored in repo.last_head: sorted ids plus a shallow marker.
func (t tipSet) state() string {
	s := strings.Join(t.shas, ",")
	if t.shallow {
		s += shallowMark
	}
	return s
}

func parseState(s string) ([]string, bool) {
	ids, shallow := strings.CutSuffix(s, shallowMark)
	if ids == "" {
		return nil, shallow
	}
	return strings.Split(ids, ","), shallow
}

// defaultNames are tried, in order, when origin/HEAD is not set.
var defaultNames = []string{"main", "master", "develop", "trunk"}

// tips picks the refs whose history is analysed: the default branch on origin and locally,
// plus HEAD when it is detached (submodules, pinned by the superproject). The default branch is
// origin/HEAD, else the first of main/master/develop/trunk on origin, else locally. Without any
// of these, all local branches count. The checked-out branch is never used on its own, so
// switching branches does not change the numbers.
func tips(ctx context.Context, dir string) (tipSet, error) {
	out, err := gitOut(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes/origin")
	if err != nil {
		return tipSet{}, err
	}
	refs := map[string]bool{}
	var local []string
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		refs[line] = true
		if strings.HasPrefix(line, "refs/heads/") {
			local = append(local, line)
		}
	}
	name := ""
	if head, err := gitOut(ctx, dir, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		name = strings.TrimPrefix(head, "origin/")
	}
	for _, prefix := range []string{"refs/remotes/origin/", "refs/heads/"} {
		for _, n := range defaultNames {
			if name == "" && refs[prefix+n] {
				name = n
			}
		}
	}
	var chosen []string
	for _, ref := range []string{"refs/remotes/origin/" + name, "refs/heads/" + name} {
		if name != "" && refs[ref] {
			chosen = append(chosen, ref)
		}
	}
	if len(chosen) == 0 {
		chosen = local
	}
	code, err := gitExit(ctx, dir, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return tipSet{}, err
	}
	if code == 1 || len(chosen) == 0 {
		// Detached HEAD (submodules) or no branches at all: HEAD, if it points at a commit.
		if hc, err := gitExit(ctx, dir, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
			return tipSet{}, err
		} else if hc == 0 {
			chosen = append(chosen, "HEAD")
		}
	}
	if len(chosen) == 0 {
		return tipSet{}, nil
	}
	return resolve(ctx, dir, chosen, name)
}

// resolve turns refs into sorted, de-duplicated commit ids and reads shallowness, in one call.
func resolve(ctx context.Context, dir string, refs []string, branch string) (tipSet, error) {
	args := []string{"rev-parse", "--is-shallow-repository"}
	for _, r := range refs {
		args = append(args, r+"^{commit}")
	}
	out, err := gitOut(ctx, dir, args...)
	if err != nil {
		return tipSet{}, err
	}
	lines := strings.Split(out, "\n")
	t := tipSet{branch: branch, shallow: lines[0] == "true"}
	seen := map[string]bool{}
	for _, sha := range lines[1:] {
		if sha != "" && !seen[sha] {
			seen[sha] = true
			t.shas = append(t.shas, sha)
		}
	}
	sort.Strings(t.shas)
	return t, nil
}

// allReachable reports whether every old tip is an ancestor of some new tip; otherwise history
// was rewritten (or a tip disappeared) and incremental reading would miss or keep stale commits.
func allReachable(ctx context.Context, dir string, old, current []string) bool {
	for _, o := range old {
		found := false
		for _, c := range current {
			if isAncestor(ctx, dir, o, c) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// gitExit runs git and returns its exit code; only failures to run git at all are errors.
func gitExit(ctx context.Context, dir string, args ...string) (int, error) {
	err := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)...).Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return 1, nil // "-q --verify": the ref does not exist
	default:
		return 0, fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), dir, err)
	}
}

func isAncestor(ctx context.Context, dir, a, b string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir, "-c", "core.fsmonitor=false", "merge-base", "--is-ancestor", a, b).Run() == nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// commitWriter inserts commits and their per-directory line counts.
type commitWriter struct {
	commit, dir *sql.Stmt
	repoID      int64
	depth       int
	source      string
}

func newCommitWriter(tx *sql.Tx, repoID int64, depth int, source string) (*commitWriter, error) {
	c, err := tx.Prepare(`INSERT OR REPLACE INTO commits(repo_id,hash,author_name,author_email,author_time,subject,ctype,is_merge,additions,deletions,files,agents,source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare commit insert: %w", err)
	}
	d, err := tx.Prepare(`INSERT OR REPLACE INTO commit_dir(repo_id,hash,dir,additions,deletions) VALUES(?,?,?,?,?)`)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("prepare dir insert: %w", err)
	}
	return &commitWriter{commit: c, dir: d, repoID: repoID, depth: depth, source: source}, nil
}

func (w *commitWriter) Close() {
	w.commit.Close()
	w.dir.Close()
}

func (w *commitWriter) Write(c gitlog.Commit) error {
	agents := strings.Join(gitlog.DetectAgents(c), ",")
	if _, err := w.commit.Exec(w.repoID, c.Hash, c.AuthorName, strings.ToLower(c.AuthorEmail), c.AuthorTime.Unix(),
		c.Subject, gitlog.ClassifyType(c.Subject, c.IsMerge()), boolInt(c.IsMerge()),
		c.Additions(), c.Deletions(), len(c.Files), agents, w.source); err != nil {
		return fmt.Errorf("insert commit %s: %w", c.Hash, err)
	}
	dirs := map[string][2]int{}
	for _, f := range c.Files {
		d := gitlog.ModuleDir(f.Path, w.depth)
		v := dirs[d]
		dirs[d] = [2]int{v[0] + f.Added, v[1] + f.Deleted}
	}
	for d, v := range dirs {
		if _, err := w.dir.Exec(w.repoID, c.Hash, d, v[0], v[1]); err != nil {
			return fmt.Errorf("insert dir stat: %w", err)
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
