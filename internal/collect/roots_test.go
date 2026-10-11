package collect

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/config"
)

func TestDirsCountsOverlappingRootsOnce(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "work", "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	commitFile(t, repo, "a.txt", "a\n", "feat: a")
	other := t.TempDir()
	lib := filepath.Join(other, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, lib, "init", "-q", "-b", "main")
	commitFile(t, lib, "b.txt", "b\n", "feat: b")

	st := newStore(t)
	sum := &Summary{}
	roots := []config.Root{{Path: root, Depth: 3}, {Path: filepath.Join(root, "work"), Depth: 2}, {Path: other, Depth: 1}}
	repos, err := Dirs(context.Background(), st, config.Config{ModuleDepth: 1}, roots, io.Discard, sum)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || sum.Repos != 2 {
		t.Errorf("repos = %d, summary = %d, want 2 (the nested root repeats app)", len(repos), sum.Repos)
	}
	if n, _ := countCommits(t, st); n != 2 {
		t.Errorf("commits = %d, want 2", n)
	}
	var clones int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM worktree`).Scan(&clones); err != nil {
		t.Fatal(err)
	}
	if clones != 0 {
		t.Errorf("worktree rows = %d, want 0 (a path seen twice is not another clone)", clones)
	}
}

func TestDirsRejectsMissingRootBeforeScanning(t *testing.T) {
	st := newStore(t)
	roots := []config.Root{{Path: t.TempDir(), Depth: 1}, {Path: filepath.Join(t.TempDir(), "gone"), Depth: 1}}
	if _, err := Dirs(context.Background(), st, config.Config{}, roots, io.Discard, &Summary{}); err == nil {
		t.Fatal("expected an error for the missing root")
	}
}

func TestMergeTargetsKeepsFirstOfEachRepository(t *testing.T) {
	local := []ghRepo{{ID: 1, InLocal: true}, {ID: 2, InLocal: true}}
	account := []ghRepo{{ID: 2}, {ID: 3}}
	got := mergeTargets(local, account)
	if len(got) != 3 || got[0].ID != 1 || got[1].ID != 2 || !got[1].InLocal || got[2].ID != 3 {
		t.Errorf("merged = %+v", got)
	}
}

func TestOptionsMode(t *testing.T) {
	cases := []struct {
		opts         Options
		mode, target string
	}{
		{Options{Roots: []config.Root{{Path: "/a"}, {Path: "/b"}}}, "dir", "/a, /b"},
		{Options{Accounts: []string{"octo"}}, "github", "octo"},
		{Options{Roots: []config.Root{{Path: "/a"}}, Accounts: []string{"octo"}}, "all", "/a, octo"},
	}
	for _, c := range cases {
		if mode, target := c.opts.mode(); mode != c.mode || target != c.target {
			t.Errorf("mode(%+v) = %s %q, want %s %q", c.opts, mode, target, c.mode, c.target)
		}
	}
}

func TestStrictFailsOnErrorsOrUnrecognized(t *testing.T) {
	cases := []struct {
		sum     Summary
		strict  bool
		wantErr bool
	}{
		{Summary{}, true, false},
		{Summary{Errors: 1}, true, true},
		{Summary{Unrecognized: 2}, true, true},
		{Summary{Errors: 1, Unrecognized: 2}, false, false},
	}
	for _, c := range cases {
		err := strictCheck(Options{Strict: c.strict}, &c.sum)
		if (err != nil) != c.wantErr {
			t.Errorf("strictCheck(%+v, strict=%v) = %v", c.sum, c.strict, err)
		}
	}
}

func TestStillClonedUnmarksDeletedClone(t *testing.T) {
	st := newStore(t)
	clone := t.TempDir()
	if err := os.Mkdir(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO repo(id,key,name,local_path,in_local,in_github) VALUES(1,'github.com/o/a','a',?,1,1),(2,'github.com/o/b','b','/gone/b',1,1)`, clone); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, err := stillCloned(ctx, st, 1); err != nil || !ok {
		t.Errorf("existing clone = %v, %v", ok, err)
	}
	if ok, err := stillCloned(ctx, st, 2); err != nil || ok {
		t.Errorf("deleted clone = %v, %v", ok, err)
	}
	var inLocal int
	if err := st.DB.QueryRow(`SELECT in_local FROM repo WHERE id=2`).Scan(&inLocal); err != nil || inLocal != 0 {
		t.Errorf("in_local = %d, %v; want 0", inLocal, err)
	}
}

func TestMissingTokenFailsRunWhenAccountsWereAsked(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no gh binary
	st := newStore(t)
	sum := &Summary{}
	gh, err := githubFor(context.Background(), st, Options{Roots: []config.Root{{Path: "/x"}}, Accounts: []string{"octo"}, GitHub: true}, io.Discard, sum)
	if gh != nil || err != nil || sum.Errors != 1 {
		t.Errorf("accounts without token: client %v, err %v, errors %d; want a counted error", gh, err, sum.Errors)
	}
	sum = &Summary{}
	if _, err := githubFor(context.Background(), st, Options{Roots: []config.Root{{Path: "/x"}}, GitHub: true}, io.Discard, sum); err != nil || sum.Errors != 0 {
		t.Errorf("local run without token: err %v, errors %d; want a plain skip", err, sum.Errors)
	}
}
