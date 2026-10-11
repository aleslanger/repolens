package demo

import (
	"fmt"
	"time"
)

var (
	tools     = []string{"Bash", "Read", "Edit", "Grep", "Write", "Glob", "WebFetch", "TodoWrite"}
	skills    = []string{"release-notes", "db-migrate", "changelog", "api-design"}
	subagents = []string{"code-reviewer", "Explore", "test-writer", "security-reviewer"}
	slashes   = []string{"/review", "/compact", "/clear", "/deploy"}
	mcps      = []string{"github", "context7", "playwright"}
	hookEvts  = []string{"PreToolUse", "PostToolUse", "SessionStart", "Stop"}
)

// sessions writes Claude Code and Codex sessions in a repository, with models, counters and
// activity windows; some local commits fall inside the windows.
func (g *gen) sessions(id int64, r repoSpec) error {
	for d := g.start.Add(60 * day); d.Before(g.now); d = d.Add(day) {
		for range g.poisson(r.activity * 0.9 * growth(d, g.start, g.now)) {
			tool := "claude"
			if g.rng.IntN(4) == 0 {
				tool = "codex"
			}
			if err := g.session(id, r, tool, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gen) session(id int64, r repoSpec, tool string, d time.Time) error {
	sid := fmt.Sprintf("%s:%08x-demo-%04d", tool, g.rng.Uint32(), g.next())
	start := d.Add(time.Duration(9+g.rng.IntN(10)) * time.Hour)
	// Session lengths are skewed: most are short, a few run for hours.
	minutes := 2 + int(g.rng.ExpFloat64()*25)
	end := start.Add(time.Duration(minutes) * time.Minute)
	turns := 1 + minutes/4
	err := g.exec(`INSERT INTO agent_session(id,tool,repo_id,cwd,in_worktree,started_at,ended_at,active_ms,turns,source_file)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, sid, tool, id, "/home/octo/code/"+r.name, b2i(g.rng.IntN(5) == 0), start.Unix(), end.Unix(),
		int64(minutes)*60000*7/10, turns, "demo")
	if err != nil {
		return err
	}
	if err := g.exec(`INSERT INTO agent_window(session_id,start_at,end_at) VALUES(?,?,?)`, sid, start.Unix(), end.Unix()); err != nil {
		return err
	}
	if err := g.models(sid, tool, turns); err != nil {
		return err
	}
	if err := g.counters(sid, tool, turns); err != nil {
		return err
	}
	if tool == "codex" && g.rng.IntN(3) == 0 {
		return g.thread(sid, id, r, start)
	}
	return nil
}

func (g *gen) models(sid, tool string, turns int) error {
	model, cost := "gpt-5.5", any(nil)
	if tool == "claude" {
		model = []string{"claude-sonnet-5-5", "claude-sonnet-5-5", "claude-opus-5-5"}[g.rng.IntN(3)]
	}
	in, out := turns*(800+g.rng.IntN(3000)), turns*(300+g.rng.IntN(1500))
	cacheRead, cacheWrite := turns*(20000+g.rng.IntN(60000)), turns*(1000+g.rng.IntN(6000))
	source := ""
	if tool == "claude" {
		cost, source = float64(in*3+out*15+cacheRead/10*3+cacheWrite*4)/1e6, "reported"
	}
	return g.exec(`INSERT INTO session_model(session_id,model,requests,input,output,cache_read,cache_write,reasoning,cost_usd,cost_source)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, sid, model, turns*3, in, out, cacheRead, cacheWrite, out/5, cost, source)
}

func (g *gen) counters(sid, tool string, turns int) error {
	add := func(kind, name string, count, errs int) error {
		return g.exec(`INSERT INTO session_counter(session_id,kind,name,count,errors,duration_ms) VALUES(?,?,?,?,?,?)
			ON CONFLICT DO UPDATE SET count=count+excluded.count`, sid, kind, name, count, errs, 0)
	}
	for _, t := range tools[:3+g.rng.IntN(len(tools)-3)] {
		if err := add("tool", t, 1+g.rng.IntN(turns*3), b2i(g.rng.IntN(6) == 0)); err != nil {
			return err
		}
	}
	if tool == "codex" {
		return nil // Codex rollouts carry no skills, slash commands or hooks
	}
	picks := []struct {
		kind  string
		names []string
		p     int // percent chance
	}{{"skill", skills, 35}, {"subagent", subagents, 40}, {"slash", slashes, 30}, {"mcp", mcps, 25}}
	for _, pk := range picks {
		if g.rng.IntN(100) < pk.p {
			if err := add(pk.kind, pk.names[g.rng.IntN(len(pk.names))], 1+g.rng.IntN(3), 0); err != nil {
				return err
			}
		}
	}
	for _, e := range hookEvts {
		if err := add("hook", e, 1+g.rng.IntN(turns*2), 0); err != nil {
			return err
		}
	}
	if g.rng.IntN(8) == 0 {
		return add("denial", "user-rejected", 1, 0)
	}
	return nil
}

func (g *gen) thread(parent string, id int64, r repoSpec, start time.Time) error {
	sid := fmt.Sprintf("codex:%08x-thread-%04d", g.rng.Uint32(), g.next())
	err := g.exec(`INSERT INTO agent_session(id,tool,parent_id,repo_id,cwd,started_at,ended_at,active_ms,turns,source_file)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, sid, "codex", parent, id, "/home/octo/code/"+r.name, start.Unix()+60, start.Unix()+400, 240000, 3, "demo")
	if err != nil {
		return err
	}
	return g.models(sid, "codex", 3)
}

// agentConfig writes project and global configuration, with creation dates spread over time.
func (g *gen) agentConfig() error {
	type item struct {
		repo             any
		scope, kind      string
		name, path       string
		daysAgo, changes int
	}
	items := []item{
		{1, "project", "skill", "release-notes", ".claude/skills/release-notes/SKILL.md", 120, 4},
		{1, "project", "agent", "code-reviewer", ".claude/agents/code-reviewer.md", 95, 2},
		{1, "project", "command", "deploy", ".claude/commands/deploy.md", 60, 1},
		{1, "project", "instructions", "CLAUDE.md", "CLAUDE.md", 130, 9},
		{2, "project", "skill", "db-migrate", ".claude/skills/db-migrate/SKILL.md", 70, 3},
		{2, "project", "agent", "test-writer", ".claude/agents/test-writer.md", 40, 1},
		{2, "project", "command", "review", ".claude/commands/review.md", 25, 2},
		{2, "project", "skill", "legacy-import", ".claude/skills/legacy-import/SKILL.md", 150, 1},
		{2, "project", "instructions", "AGENTS.md", "AGENTS.md", 80, 5},
		{4, "project", "permission", "Bash(uv run:*)", ".claude/settings.json", 50, 2},
		{nil, "global", "skill", "changelog", "~/.claude/skills/changelog/SKILL.md", 0, 0},
		{nil, "global", "skill", "api-design", "~/.claude/skills/api-design/SKILL.md", 0, 0},
		{nil, "global", "agent", "security-reviewer", "~/.claude/agents/security-reviewer.md", 0, 0},
		{nil, "global", "mcp", "github", "~/.claude.json", 0, 0},
	}
	for _, it := range items {
		var first, last any
		if it.daysAgo > 0 {
			f := g.now.Add(-time.Duration(it.daysAgo) * day)
			first, last = f.Unix(), f.Add(time.Duration(it.changes*7)*day).Unix()
		}
		err := g.exec(`INSERT INTO agent_config(repo_id,scope,tool,kind,name,path,first_time,last_time,changes) VALUES(?,?,?,?,?,?,?,?,?)`,
			it.repo, it.scope, "claude", it.kind, it.name, it.path, first, last, it.changes)
		if err != nil {
			return err
		}
	}
	return nil
}
