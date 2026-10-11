package collect

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"repolens/internal/config"
	"repolens/internal/store"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=Dev@Example.com",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", msg)
}

func countCommits(t *testing.T, st *store.Store) (n, add int) {
	t.Helper()
	if err := st.DB.QueryRow(`SELECT COUNT(*), IFNULL(SUM(additions),0) FROM commits`).Scan(&n, &add); err != nil {
		t.Fatal(err)
	}
	return n, add
}

func TestDirIsIncrementalAndSurvivesHistoryRewrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "own")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "remote", "add", "origin", "git@github.com:me/own.git")
	commitFile(t, repo, "src/a.go", "a\nb\n", "feat: first\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
	commitFile(t, repo, "docs/x.md", "x\n", "docs: second")

	foreign := filepath.Join(root, "foreign")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, foreign, "init", "-q", "-b", "main")
	git(t, foreign, "remote", "add", "origin", "https://github.com/someone/else.git")
	commitFile(t, foreign, "f.txt", "f\n", "init")

	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{ScanDepth: 2, ModuleDepth: 1, Owners: []string{"github.com/me"}}

	sum := &Summary{}
	if _, err := Dir(ctx, st, cfg, root, io.Discard, sum); err != nil {
		t.Fatal(err)
	}
	if sum.Repos != 1 || sum.Skipped != 1 || sum.Commits != 2 {
		t.Fatalf("first run summary = %+v, want 1 repo, 1 skipped, 2 commits", sum)
	}
	var agents, ctype, email string
	if err := st.DB.QueryRow(`SELECT agents, ctype, author_email FROM commits WHERE subject='feat: first'`).Scan(&agents, &ctype, &email); err != nil {
		t.Fatal(err)
	}
	if agents != "claude" || ctype != "feat" || email != "dev@example.com" {
		t.Errorf("first commit agents=%q type=%q email=%q", agents, ctype, email)
	}

	// Second run with no changes reads nothing.
	sum = &Summary{}
	if _, err := Dir(ctx, st, cfg, root, io.Discard, sum); err != nil {
		t.Fatal(err)
	}
	if sum.Commits != 0 {
		t.Errorf("unchanged run read %d commits", sum.Commits)
	}

	// A new commit is read incrementally.
	commitFile(t, repo, "src/b.go", "1\n2\n3\n", "fix: third")
	sum = &Summary{}
	if _, err := Dir(ctx, st, cfg, root, io.Discard, sum); err != nil {
		t.Fatal(err)
	}
	if n, _ := countCommits(t, st); sum.Commits != 1 || n != 3 {
		t.Errorf("incremental: read %d, stored %d; want 1 and 3", sum.Commits, n)
	}

	// Rewriting history (amend) forces a full reload without duplicates.
	git(t, repo, "commit", "-q", "--amend", "-m", "fix: third, amended")
	sum = &Summary{}
	if _, err := Dir(ctx, st, cfg, root, io.Discard, sum); err != nil {
		t.Fatal(err)
	}
	n, add := countCommits(t, st)
	if n != 3 || add != 6 {
		t.Errorf("after rewrite stored %d commits with %d additions, want 3 and 6", n, add)
	}
	var dirs int
	if err := st.DB.QueryRow(`SELECT COUNT(DISTINCT dir) FROM commit_dir`).Scan(&dirs); err != nil {
		t.Fatal(err)
	}
	if dirs != 2 {
		t.Errorf("distinct dirs = %d, want 2 (src, docs)", dirs)
	}
}

func TestCheckedOutBranchAndNewOriginDoNotChangeTheRepo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	commitFile(t, repo, "a.txt", "a\n", "feat: one")
	commitFile(t, repo, "b.txt", "b\n", "feat: two")
	git(t, repo, "checkout", "-q", "-b", "wip")
	commitFile(t, repo, "c.txt", "c\n", "wip: unmerged")

	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{ScanDepth: 1, ModuleDepth: 1}
	run := func() {
		t.Helper()
		if _, err := Dir(ctx, st, cfg, root, io.Discard, &Summary{}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if n, _ := countCommits(t, st); n != 2 {
		t.Fatalf("commits with wip checked out = %d, want 2 (default branch only)", n)
	}
	git(t, repo, "checkout", "-q", "main")
	run()
	if n, _ := countCommits(t, st); n != 2 {
		t.Fatalf("commits after switching back = %d, want 2", n)
	}

	git(t, repo, "remote", "add", "origin", "git@github.com:me/proj.git")
	run()
	var repos int
	var key string
	if err := st.DB.QueryRow(`SELECT COUNT(*), MAX(key) FROM repo`).Scan(&repos, &key); err != nil {
		t.Fatal(err)
	}
	if repos != 1 || key != "github.com/me/proj" {
		t.Errorf("repos=%d key=%q, want one row re-keyed to the remote", repos, key)
	}
	var host, owner string
	if err := st.DB.QueryRow(`SELECT IFNULL(host,''), IFNULL(owner,'') FROM repo`).Scan(&host, &owner); err != nil {
		t.Fatal(err)
	}
	if host != "github.com" || owner != "me" {
		t.Errorf("host=%q owner=%q after re-key, want github.com/me", host, owner)
	}
	if n, _ := countCommits(t, st); n != 2 {
		t.Errorf("commits after re-key = %d, want 2", n)
	}
}

// Submodules sit on a detached HEAD that the superproject moves ahead of the stale local main.
func TestDetachedHeadAheadOfLocalMainIsCounted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "sub")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	commitFile(t, repo, "a.txt", "a\n", "feat: on main")
	git(t, repo, "checkout", "-q", "--detach")
	commitFile(t, repo, "b.txt", "b\n", "feat: pinned by the superproject")

	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := Dir(ctx, st, config.Config{ScanDepth: 1, ModuleDepth: 1}, root, io.Discard, &Summary{}); err != nil {
		t.Fatal(err)
	}
	if n, _ := countCommits(t, st); n != 2 {
		t.Errorf("commits = %d, want 2 (main + detached HEAD)", n)
	}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func collectDir(t *testing.T, st *store.Store, root string) {
	t.Helper()
	if _, err := Dir(context.Background(), st, config.Config{ScanDepth: 2, ModuleDepth: 1}, root, io.Discard, &Summary{}); err != nil {
		t.Fatal(err)
	}
}

func upstream(t *testing.T, branch string, commits int) string {
	t.Helper()
	up := filepath.Join(t.TempDir(), "up")
	if err := os.MkdirAll(up, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, up, "init", "-q", "-b", branch)
	for i := range commits {
		commitFile(t, up, "f.txt", fmt.Sprint(i), fmt.Sprintf("feat: %d", i))
	}
	return up
}

// Audit P1: a shallow clone that is later unshallowed must load its full history.
func TestUnshallowLoadsFullHistory(t *testing.T) {
	up := upstream(t, "main", 5)
	root := t.TempDir()
	git(t, root, "clone", "-q", "--depth", "1", "file://"+up, "shallow")
	st := newStore(t)
	collectDir(t, st, root)
	if n, _ := countCommits(t, st); n != 1 {
		t.Fatalf("shallow clone commits = %d, want 1", n)
	}
	git(t, filepath.Join(root, "shallow"), "fetch", "-q", "--unshallow")
	collectDir(t, st, root)
	if n, _ := countCommits(t, st); n != 5 {
		t.Errorf("after unshallow commits = %d, want 5", n)
	}
}

// Audit P1: a default branch named develop, without origin/HEAD, must not fall back to the
// checked-out feature branch.
func TestDevelopDefaultWithoutOriginHead(t *testing.T) {
	up := upstream(t, "develop", 5)
	root := t.TempDir()
	git(t, root, "clone", "-q", "file://"+up, "dev")
	clone := filepath.Join(root, "dev")
	git(t, clone, "remote", "set-head", "origin", "-d")
	git(t, clone, "checkout", "-q", "-b", "feature")
	commitFile(t, clone, "x.txt", "x", "wip: feature only")
	st := newStore(t)
	collectDir(t, st, root)
	if n, _ := countCommits(t, st); n != 5 {
		t.Errorf("commits = %d, want 5 (develop only, not the checked-out feature)", n)
	}
}

// Without any default-looking branch, all local branches count, independent of the checkout.
func TestNoDefaultBranchUsesAllLocalBranches(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "odd")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "work")
	commitFile(t, repo, "a.txt", "a", "feat: a")
	git(t, repo, "checkout", "-q", "-b", "other")
	commitFile(t, repo, "b.txt", "b", "feat: b")
	st := newStore(t)
	collectDir(t, st, root)
	first, _ := countCommits(t, st)
	git(t, repo, "checkout", "-q", "work")
	collectDir(t, st, root)
	second, _ := countCommits(t, st)
	if first != 2 || second != 2 {
		t.Errorf("commits = %d then %d, want 2 both times", first, second)
	}
}

// Two clones of the same remote: one repository row, the second clone's path still maps
// sessions to it, and alternating scans do not reload history.
func TestTwoClonesOfOneRemote(t *testing.T) {
	up := upstream(t, "main", 3)
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		git(t, root, "clone", "-q", "file://"+up, name)
		git(t, filepath.Join(root, name), "remote", "set-url", "origin", "git@github.com:me/same.git")
	}
	commitFile(t, filepath.Join(root, "b"), "local.txt", "x", "wip: only in clone b")
	st := newStore(t)
	sum := &Summary{}
	if _, err := Dir(context.Background(), st, config.Config{ScanDepth: 1, ModuleDepth: 1}, root, io.Discard, sum); err != nil {
		t.Fatal(err)
	}
	var repos, aliases int
	if err := st.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM repo), (SELECT COUNT(*) FROM worktree WHERE path=?)`, filepath.Join(root, "b")).Scan(&repos, &aliases); err != nil {
		t.Fatal(err)
	}
	if repos != 1 || aliases != 1 {
		t.Errorf("repos=%d, second clone mapped=%d; want 1 and 1", repos, aliases)
	}
	again := &Summary{}
	if _, err := Dir(context.Background(), st, config.Config{ScanDepth: 1, ModuleDepth: 1}, root, io.Discard, again); err != nil {
		t.Fatal(err)
	}
	if again.Commits != 0 {
		t.Errorf("unchanged clones re-read %d commits", again.Commits)
	}
}

func TestDirRejectsMissingOrNonDirectoryRoot(t *testing.T) {
	st := newStore(t)
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(t.TempDir(), "missing"), file} {
		if _, err := Dir(context.Background(), st, config.Config{ScanDepth: 1}, root, io.Discard, &Summary{}); err == nil {
			t.Errorf("Dir(%s) succeeded, want an error", root)
		}
	}
}
