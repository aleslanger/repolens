// Package demo fills a database with fictitious, deterministic data for screenshots and trying
// the dashboard out. Every name, e-mail and path is invented (account "octo", example.com).
package demo

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"repolens/internal/collect"
	"repolens/internal/config"
	"repolens/internal/store"
)

const (
	day          = 24 * time.Hour
	historyDays  = 200
	seed         = 42
	sourceGit    = "git-local"
	sourceGitHub = "github-api"
)

type repoSpec struct {
	key, name, owner, desc, lang string
	local, github, fork, archive bool
	activity                     float64 // expected commits per day
	dirs                         []string
}

var repos = []repoSpec{
	{"github.com/octo/atlas", "atlas", "octo", "Command-line map tiler", "Go", true, true, false, false, 2.2, []string{"cmd/atlas", "internal/tiles", "internal/render", "docs"}},
	{"github.com/octo/beacon", "beacon", "octo", "Status page with incident timeline", "TypeScript", true, true, false, false, 3.0, []string{"src/ui", "src/api", "src/store", "e2e"}},
	{"github.com/octo/comet", "comet", "octo", "Fast log shipper", "Rust", false, true, false, false, 1.1, nil},
	{"github.com/octo-labs/dune", "dune", "octo-labs", "Dataset versioning experiments", "Python", true, true, false, false, 0.9, []string{"dune/core", "dune/cli", "notebooks"}},
	{"local:/home/octo/code/notes", "notes", "", "", "", true, false, false, false, 0.5, []string{"journal", "recipes"}},
	{"github.com/octo/ember", "ember", "octo", "Old static site generator", "Go", false, true, false, true, 0.1, nil},
	{"github.com/octo/fjord", "fjord", "octo", "Fork of a markdown linter", "JavaScript", true, true, true, false, 0.4, []string{"lib", "test"}},
}

type author struct{ name, email string }

var (
	octo    = author{"Octo Cat", "octo@example.com"}
	octoGH  = author{"octocat", "1001+octocat@users.noreply.github.com"}
	mona    = author{"Mona Octocat", "mona@example.com"}
	hubot   = author{"Hubot", "hubot@example.com"}
	depbot  = author{"dependabot[bot]", "49699333+dependabot[bot]@users.noreply.github.com"}
	copilot = author{"Copilot", "198982749+Copilot@users.noreply.github.com"}
)

var commitTypes = []struct {
	ctype  string
	weight int
	verb   string
}{
	{"feat", 30, "add"}, {"fix", 22, "fix"}, {"refactor", 10, "simplify"}, {"docs", 8, "document"}, {"test", 10, "cover"},
	{"chore", 10, "bump"}, {"ci", 4, "tune"}, {"perf", 3, "speed up"}, {"other", 3, "update"},
}

var subjects = []string{"tile cache", "status badge", "retry loop", "config loader", "release notes", "error page",
	"search box", "timeline view", "export", "login flow", "dark mode", "rate limiter"}

// gen carries the random source and the time frame of one generation.
type gen struct {
	rng   *rand.Rand
	now   time.Time
	start time.Time
	tx    *sql.Tx
	n     int // running counter for ids and hashes
}

// Generate writes the demo data into an empty store and computes the derived tables with the
// real collectors' code (commit attribution, identity merging).
func Generate(ctx context.Context, st *store.Store, now time.Time) error {
	var existing int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM repo`).Scan(&existing); err != nil {
		return fmt.Errorf("check database: %w", err)
	}
	if existing > 0 {
		return fmt.Errorf("databáze už obsahuje %d repozitářů; demo se zapisuje jen do prázdné databáze", existing)
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	g := &gen{rng: rand.New(rand.NewPCG(seed, seed)), now: now, start: now.Add(-historyDays * day), tx: tx}
	steps := []func() error{g.repos, g.agentConfig, g.runs}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if err := collect.MarkCommits(ctx, st); err != nil {
		return err
	}
	return collect.People(ctx, st, config.Config{})
}

func (g *gen) exec(q string, args ...any) error {
	if _, err := g.tx.Exec(q, args...); err != nil {
		return fmt.Errorf("demo: %w (%s)", err, strings.Join(strings.Fields(q)[:3], " "))
	}
	return nil
}

func (g *gen) next() int { g.n++; return g.n }

// hash is a fake commit id: random high bits and a running counter, so ids never repeat.
func (g *gen) hash() string { return fmt.Sprintf("%024x%016x", g.rng.Uint64(), g.next()) }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (g *gen) repos() error {
	for i, r := range repos {
		id := int64(i + 1)
		path := ""
		if r.local {
			path = "/home/octo/code/" + r.name
		}
		host, owner := "", ""
		if r.github {
			host, owner = "github.com", r.owner
		}
		err := g.exec(`INSERT INTO repo(id,key,name,host,owner,local_path,description,default_branch,is_private,is_fork,is_archived,
			stars,forks,in_local,in_github,gh_synced_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, r.key, r.name, nullable(host), nullable(owner), nullable(path), r.desc, "main", 0, b2i(r.fork), b2i(r.archive),
			g.rng.IntN(400), g.rng.IntN(40), b2i(r.local), b2i(r.github), g.now.Unix())
		if err != nil {
			return err
		}
		if err := g.commits(id, r); err != nil {
			return err
		}
		if r.github {
			if err := g.github(id, r); err != nil {
				return err
			}
		}
		if r.local && r.name != "notes" {
			if err := g.sessions(id, r); err != nil {
				return err
			}
		}
	}
	return g.exec(`INSERT INTO worktree(repo_id,path,branch) VALUES(2,'/home/octo/.worktrees/beacon-timeline','feat/timeline'),
		(1,'/home/octo/code/atlas-render','refactor/render')`)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// growth makes activity rise over the period, as an adopted tool would.
func growth(t, start, now time.Time) float64 {
	return 0.4 + 1.2*float64(t.Sub(start))/float64(now.Sub(start))
}

func (g *gen) pickType() (string, string) {
	total := 0
	for _, c := range commitTypes {
		total += c.weight
	}
	x := g.rng.IntN(total)
	for _, c := range commitTypes {
		if x < c.weight {
			return c.ctype, c.verb
		}
		x -= c.weight
	}
	return "other", "update"
}

func (g *gen) commitAuthor(r repoSpec) author {
	switch x := g.rng.IntN(100); {
	case r.name == "notes" || x < 55:
		if g.rng.IntN(5) == 0 {
			return octoGH
		}
		return octo
	case x < 80:
		return mona
	case x < 88:
		return depbot
	case x < 94:
		return copilot
	default:
		return hubot
	}
}

func (g *gen) commits(id int64, r repoSpec) error {
	source := sourceGit
	if !r.local {
		source = sourceGitHub
	}
	for d := g.start; d.Before(g.now); d = d.Add(day) {
		if r.archive && d.After(g.start.Add(40*day)) {
			break
		}
		n := g.poisson(r.activity * growth(d, g.start, g.now))
		for range n {
			if err := g.commit(id, r, d, source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gen) commit(id int64, r repoSpec, d time.Time, source string) error {
	a := g.commitAuthor(r)
	ctype, verb := g.pickType()
	when := d.Add(time.Duration(8+g.rng.IntN(12))*time.Hour + time.Duration(g.rng.IntN(60))*time.Minute)
	subject := fmt.Sprintf("%s: %s %s", ctype, verb, subjects[g.rng.IntN(len(subjects))])
	if ctype == "other" {
		subject = "Merge pull request #" + fmt.Sprint(g.rng.IntN(300)) + " from octo/topic"
	}
	add, del := 5+g.rng.IntN(220), g.rng.IntN(120)
	agents := ""
	// Agent trailers become more common over the period.
	if (a == octo || a == mona) && g.rng.Float64() < 0.5*growth(when, g.start, g.now)-0.2 {
		agents = "claude"
	}
	if a == copilot {
		agents = "copilot"
	}
	h := g.hash()
	err := g.exec(`INSERT INTO commits(repo_id,hash,author_name,author_email,author_time,subject,ctype,is_merge,additions,deletions,files,agents,source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, h, a.name, a.email, when.Unix(), subject, ctype, b2i(ctype == "other"), add, del, 1+g.rng.IntN(9), agents, source)
	if err != nil || len(r.dirs) == 0 {
		return err
	}
	dir := r.dirs[g.rng.IntN(len(r.dirs))]
	return g.exec(`INSERT INTO commit_dir(repo_id,hash,dir,additions,deletions) VALUES(?,?,?,?,?)`, id, h, dir, add, del)
}

// poisson draws a small count with the given mean (Knuth's method; the means here are small).
func (g *gen) poisson(mean float64) int {
	limit, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= g.rng.Float64()
		if p <= limit {
			return k
		}
		k++
	}
}
