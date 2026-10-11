package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

const (
	topCounters   = 30
	topUnmatched  = 10
	builtinMarker = "(vestavěný)"
	sessionFilter = "s.parent_id IS NULL"
)

// builtinAgents ship with Claude Code / Codex and are never configured on disk.
// builtinCommands are Claude Code's own slash commands (lower case, without the slash).
var builtinCommands = map[string]bool{"clear": true, "compact": true, "theme": true, "model": true, "reload-plugins": true,
	"reload-skills": true, "feedback": true, "effort": true, "exit": true, "help": true, "config": true, "init": true,
	"review": true, "resume": true, "mcp": true, "agents": true, "cost": true, "status": true, "login": true, "logout": true,
	"memory": true, "permissions": true, "hooks": true, "plugin": true, "doctor": true, "ide": true, "vim": true,
	"export": true, "rewind": true, "context": true, "add-dir": true, "bug": true, "usage": true, "statusline": true,
	"fast": true, "loop": true, "schedule": true, "plan": true, "skills": true, "artifacts": true, "workflows": true}

// Keys are lower case, matching usageKey.
var builtinAgents = map[string]bool{"general-purpose": true, "explore": true, "plan": true, "fork": true,
	"statusline-setup": true, "claude-code-guide": true, "codex-subagent": true, "(neznámý)": true}

type AgentTotals struct {
	Sessions     int     `json:"sessions"`
	Threads      int     `json:"threads"` // agent-spawned threads (Codex subagents)
	ActiveMs     int64   `json:"activeMs"`
	Turns        int     `json:"turns"`
	Tokens       int64   `json:"tokens"`
	Fresh        int64   `json:"fresh"` // input + output, without cache reads and writes
	CostUSD      float64 `json:"costUsd"`
	CostReported float64 `json:"costReported"` // part of CostUSD reported by the agent itself
	// UnpricedTokens are tokens of models without any price; CostUSD excludes them.
	UnpricedTokens int64 `json:"unpricedTokens"`
	First          int64 `json:"first"`
	Last           int64 `json:"last"`
	InWorktree     int   `json:"inWorktree"`
}

type ToolTotals struct {
	Tool     string  `json:"tool"`
	Sessions int     `json:"sessions"`
	ActiveMs int64   `json:"activeMs"`
	Tokens   int64   `json:"tokens"`
	Fresh    int64   `json:"fresh"`
	CostUSD  float64 `json:"costUsd"`
}

type AgentDay struct {
	Day      string `json:"day"`
	Tool     string `json:"tool"`
	Sessions int    `json:"sessions"`
	ActiveMs int64  `json:"activeMs"`
	Tokens   int64  `json:"tokens"`
	Fresh    int64  `json:"fresh"`
	// CostUSD is what is known: reported by the agent or priced from the price list.
	CostUSD float64 `json:"costUsd"`
}

type ModelRow struct {
	Model      string   `json:"model"`
	Sessions   int      `json:"sessions"`
	Requests   int64    `json:"requests"`
	Input      int64    `json:"input"`
	Output     int64    `json:"output"`
	CacheRead  int64    `json:"cacheRead"`
	CacheWrite int64    `json:"cacheWrite"`
	Reasoning  int64    `json:"reasoning"`
	CostUSD    *float64 `json:"costUsd"`
	CostSource string   `json:"costSource"` // reported, estimate, mixed, none
}

type CounterRow struct {
	Name       string `json:"name"`
	Count      int    `json:"count"`
	Errors     int    `json:"errors"`
	DurationMs int64  `json:"durationMs"`
	Sessions   int    `json:"sessions"`
}

type ConfigRow struct {
	Scope   string `json:"scope"`
	Tool    string `json:"tool"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Detail  string `json:"detail"`
	Path    string `json:"path"`
	Repo    string `json:"repo"`
	RepoID  int64  `json:"repoId"` // 0 for global and plugin items
	Present bool   `json:"present"`
	First   int64  `json:"first"`
	Last    int64  `json:"last"`
	Changes int    `json:"changes"`
	Uses    int    `json:"uses"`
	Usable  bool   `json:"usable"` // agent/skill/command: usage can be measured
}

type ProjectRow struct {
	RepoID      int64   `json:"repoId"`
	Name        string  `json:"name"`
	Sessions    int     `json:"sessions"`
	ActiveMs    int64   `json:"activeMs"`
	Tokens      int64   `json:"tokens"`
	CostUSD     float64 `json:"costUsd"`
	TopModel    string  `json:"topModel"`
	Configured  int     `json:"configured"`
	LastSession int64   `json:"lastSession"`
	// AgentShare uses the portfolio's definition: trailer or session window, last 90 days.
	AgentShare   float64 `json:"agentShare"`
	SessionShare float64 `json:"sessionShare"`
}

type MonthAttribution struct {
	Month     string `json:"month"`
	Commits   int    `json:"commits"`
	Trailer   int    `json:"trailer"`
	InSession int    `json:"inSession"`
	Either    int    `json:"either"`
}

type CWDCount struct {
	CWD      string `json:"cwd"`
	Sessions int    `json:"sessions"`
}

// AgentView summarises agent sessions, either for one repository or across all of them.
type AgentView struct {
	Totals         AgentTotals             `json:"totals"`
	ByTool         []ToolTotals            `json:"byTool"`
	Daily          []AgentDay              `json:"daily"`
	Models         []ModelRow              `json:"models"`
	Counters       map[string][]CounterRow `json:"counters"`
	Config         []ConfigRow             `json:"config"`
	Unconfigured   map[string][]CounterRow `json:"unconfigured"` // used names with no configured source
	Projects       []ProjectRow            `json:"projects"`
	Attribution    []MonthAttribution      `json:"attribution"`
	Unmatched      []CWDCount              `json:"unmatched"`
	OtherAgents    map[string]int          `json:"otherAgents"`
	Trends         map[string]Trend        `json:"trends"`
	Durations      Durations               `json:"durations"`
	ConfigTimeline []ConfigMonth           `json:"configTimeline"`
	Sessions       []SessionRow            `json:"sessions"`
	// PricesDate is the date of the price list the estimates were computed with ("" = unknown).
	PricesDate string `json:"pricesDate"`
	// PriceListDate is the date of the public price list in use ("" = not used).
	PriceListDate string `json:"priceListDate"`
	// Since is the start of the period (unix seconds; 0 = everything). It limits sessions and
	// what is derived from them; projects, commit attribution and configuration are all-time.
	Since int64 `json:"since"`
}

// scope builds the session filter for an optional repository.
type scope struct {
	where string
	args  []any
}

func scopeFor(repoID *int64, since int64) scope {
	sc := scope{where: sessionFilter}
	if repoID != nil {
		sc.where += " AND s.repo_id=?"
		sc.args = append(sc.args, *repoID)
	}
	if since > 0 {
		sc.where += " AND s.started_at>=?"
		sc.args = append(sc.args, since)
	}
	return sc
}

// AgentsFor builds the all-time agent view; repoID nil means all repositories.
func AgentsFor(ctx context.Context, db *sql.DB, repoID *int64) (*AgentView, error) {
	return AgentsSince(ctx, db, repoID, 0)
}

// AgentsSince builds the agent view for sessions started at or after since (0 = all).
func AgentsSince(ctx context.Context, db *sql.DB, repoID *int64, since int64) (*AgentView, error) {
	sc := scopeFor(repoID, since)
	v := &AgentView{Counters: map[string][]CounterRow{}, Unconfigured: map[string][]CounterRow{}, OtherAgents: map[string]int{}, Since: since}
	steps := []func(context.Context, *sql.DB, scope, *AgentView) error{
		agentTotals, agentByTool, agentDaily, agentModels, agentCounters, usageTrends, sessionDurations, sessionList,
	}
	for _, step := range steps {
		if err := step(ctx, db, sc, v); err != nil {
			return nil, err
		}
	}
	if err := agentConfig(ctx, db, repoID, v); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, `SELECT IFNULL((SELECT value FROM meta WHERE key='prices_date'),''),
		IFNULL((SELECT value FROM meta WHERE key='price_list_date'),'')`).Scan(&v.PricesDate, &v.PriceListDate); err != nil {
		return nil, fmt.Errorf("price list date: %w", err)
	}
	if err := configTimeline(ctx, db, repoID, v); err != nil {
		return nil, err
	}
	if err := attribution(ctx, db, repoID, v); err != nil {
		return nil, err
	}
	if repoID != nil {
		return v, nil
	}
	for _, step := range []func(context.Context, *sql.DB, *AgentView) error{agentProjects, unmatched, otherAgents} {
		if err := step(ctx, db, v); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func agentTotals(ctx context.Context, db *sql.DB, sc scope, v *AgentView) error {
	err := db.QueryRowContext(ctx, `SELECT COUNT(*), IFNULL(SUM(active_ms),0), IFNULL(SUM(turns),0),
		IFNULL(MIN(started_at),0), IFNULL(MAX(ended_at),0), IFNULL(SUM(in_worktree),0)
		FROM agent_session s WHERE `+sc.where, sc.args...).Scan(&v.Totals.Sessions, &v.Totals.ActiveMs, &v.Totals.Turns,
		&v.Totals.First, &v.Totals.Last, &v.Totals.InWorktree)
	if err != nil {
		return fmt.Errorf("agent totals: %w", err)
	}
	// Tokens and cost include agent-spawned threads, which belong to their parent's work.
	err = db.QueryRowContext(ctx, `SELECT IFNULL(SUM(m.input+m.output+m.cache_read+m.cache_write),0), IFNULL(SUM(m.input+m.output),0), IFNULL(SUM(m.cost_usd),0),
		IFNULL(SUM(CASE WHEN m.cost_source='reported' THEN m.cost_usd END),0),
		IFNULL(SUM(CASE WHEN m.cost_usd IS NULL THEN m.input+m.output+m.cache_read+m.cache_write END),0),
		(SELECT COUNT(*) FROM agent_session c JOIN agent_session s ON s.id=c.parent_id WHERE `+sc.where+`)
		FROM session_model m `+joinRoot+` WHERE `+sc.where, slices.Concat(sc.args, sc.args)...).
		Scan(&v.Totals.Tokens, &v.Totals.Fresh, &v.Totals.CostUSD, &v.Totals.CostReported, &v.Totals.UnpricedTokens, &v.Totals.Threads)
	if err != nil {
		return fmt.Errorf("agent token totals: %w", err)
	}
	return nil
}

func agentByTool(ctx context.Context, db *sql.DB, sc scope, v *AgentView) error {
	v.ByTool = []ToolTotals{}
	// Tokens and cost of a session include its agent-spawned threads, as in the totals.
	return queryEach(ctx, db, `SELECT s.tool, COUNT(*), SUM(s.active_ms),
		IFNULL(SUM(`+familyTokens+`),0), IFNULL(SUM(`+familyFresh+`),0),
		IFNULL(SUM(`+familyCost+`),0)
		FROM agent_session s WHERE `+sc.where+` GROUP BY s.tool ORDER BY COUNT(*) DESC`, sc.args, func(r *sql.Rows) error {
		var t ToolTotals
		if err := r.Scan(&t.Tool, &t.Sessions, &t.ActiveMs, &t.Tokens, &t.Fresh, &t.CostUSD); err != nil {
			return err
		}
		v.ByTool = append(v.ByTool, t)
		return nil
	})
}

func agentDaily(ctx context.Context, db *sql.DB, sc scope, v *AgentView) error {
	v.Daily = []AgentDay{}
	return queryEach(ctx, db, `SELECT date(s.started_at,'unixepoch','localtime'), s.tool, COUNT(*), SUM(s.active_ms),
		IFNULL(SUM(`+familyTokens+`),0), IFNULL(SUM(`+familyFresh+`),0), IFNULL(SUM(`+familyCost+`),0)
		FROM agent_session s WHERE `+sc.where+` GROUP BY 1,2 ORDER BY 1`, sc.args, func(r *sql.Rows) error {
		var d AgentDay
		if err := r.Scan(&d.Day, &d.Tool, &d.Sessions, &d.ActiveMs, &d.Tokens, &d.Fresh, &d.CostUSD); err != nil {
			return err
		}
		v.Daily = append(v.Daily, d)
		return nil
	})
}

func agentModels(ctx context.Context, db *sql.DB, sc scope, v *AgentView) error {
	v.Models = []ModelRow{}
	return queryEach(ctx, db, `SELECT m.model, COUNT(DISTINCT m.session_id), SUM(m.requests), SUM(m.input), SUM(m.output),
		SUM(m.cache_read), SUM(m.cache_write), SUM(m.reasoning), SUM(m.cost_usd),
		GROUP_CONCAT(DISTINCT NULLIF(m.cost_source,'')), SUM(m.cost_source='')
		FROM session_model m `+joinRoot+` WHERE `+sc.where+`
		GROUP BY m.model ORDER BY SUM(m.input+m.output+m.cache_read+m.cache_write) DESC`, sc.args, func(r *sql.Rows) error {
		var m ModelRow
		var cost sql.NullFloat64
		var sources sql.NullString
		var unpriced int
		if err := r.Scan(&m.Model, &m.Sessions, &m.Requests, &m.Input, &m.Output, &m.CacheRead, &m.CacheWrite, &m.Reasoning,
			&cost, &sources, &unpriced); err != nil {
			return err
		}
		if cost.Valid {
			m.CostUSD = &cost.Float64
		}
		switch {
		case !sources.Valid:
			m.CostSource = "none"
		case strings.Contains(sources.String, ",") || unpriced > 0:
			m.CostSource = "mixed"
		default:
			m.CostSource = sources.String
		}
		v.Models = append(v.Models, m)
		return nil
	})
}

func agentCounters(ctx context.Context, db *sql.DB, sc scope, v *AgentView) error {
	return queryEach(ctx, db, `SELECT c.kind, c.name, SUM(c.count), SUM(c.errors), SUM(c.duration_ms), COUNT(DISTINCT c.session_id)
		FROM session_counter c JOIN agent_session x ON x.id=c.session_id JOIN agent_session s ON s.id=COALESCE(x.parent_id, x.id)
		WHERE `+sc.where+`
		GROUP BY c.kind, c.name ORDER BY c.kind, SUM(c.count) DESC`, sc.args, func(r *sql.Rows) error {
		var kind string
		var c CounterRow
		if err := r.Scan(&kind, &c.Name, &c.Count, &c.Errors, &c.DurationMs, &c.Sessions); err != nil {
			return err
		}
		if len(v.Counters[kind]) < topCounters {
			v.Counters[kind] = append(v.Counters[kind], c)
		}
		return nil
	})
}

// joinRoot joins a session_model row (alias m) to its top-level session (alias s), so a spawned
// thread counts for its parent's period and repository, like familyTokens does.
const joinRoot = `JOIN agent_session c ON c.id=m.session_id JOIN agent_session s ON s.id=COALESCE(c.parent_id, c.id)`

// familyTokens and familyCost sum a top-level session together with its spawned threads.
const (
	familyTokens = `(SELECT SUM(m.input+m.output+m.cache_read+m.cache_write) FROM session_model m JOIN agent_session c ON c.id=m.session_id WHERE c.id=s.id OR c.parent_id=s.id)`
	familyFresh  = `(SELECT SUM(m.input+m.output) FROM session_model m JOIN agent_session c ON c.id=m.session_id WHERE c.id=s.id OR c.parent_id=s.id)`
	familyCost   = `(SELECT SUM(m.cost_usd) FROM session_model m JOIN agent_session c ON c.id=m.session_id WHERE c.id=s.id OR c.parent_id=s.id)`
)

type useKey struct {
	repo       int64
	kind, name string
}

// usageKey normalises names so "/ecc:plan", "ecc:plan" and "plan" (project) compare.
func usageKey(name string) string { return strings.TrimPrefix(strings.ToLower(name), "/") }

// agentConfig lists configured items and how often each was used in the sessions in scope.
func agentConfig(ctx context.Context, db *sql.DB, repoID *int64, v *AgentView) error {
	// Usage per (repo, kind, name); project items only count sessions in their own repository.
	used := map[useKey]int{}
	usedAll := map[[2]string]int{}
	err := queryEach(ctx, db, `SELECT IFNULL(s.repo_id,0), c.kind, c.name, SUM(c.count) FROM session_counter c
		JOIN agent_session s ON s.id=c.session_id WHERE c.kind IN ('subagent','skill','slash') GROUP BY 1,2,3`, nil,
		func(r *sql.Rows) error {
			var k useKey
			var n int
			if err := r.Scan(&k.repo, &k.kind, &k.name, &n); err != nil {
				return err
			}
			k.name = usageKey(k.name)
			if repoID != nil && k.repo != *repoID {
				return nil
			}
			used[k] += n
			usedAll[[2]string{k.kind, k.name}] += n
			return nil
		})
	if err != nil {
		return err
	}
	where, args := "", []any{}
	if repoID != nil {
		where, args = "WHERE a.repo_id IS NULL OR a.repo_id=?", []any{*repoID}
	}
	v.Config = []ConfigRow{}
	configured := map[[2]string]bool{}
	err = queryEach(ctx, db, `SELECT a.scope, a.tool, a.kind, a.name, a.detail, a.path, IFNULL(r.name,''), IFNULL(a.repo_id,0),
		a.present, IFNULL(a.first_time,0), IFNULL(a.last_time,0), a.changes
		FROM agent_config a LEFT JOIN repo r ON r.id=a.repo_id `+where+` ORDER BY a.scope, a.kind, a.name`, args,
		func(r *sql.Rows) error {
			var c ConfigRow
			if err := r.Scan(&c.Scope, &c.Tool, &c.Kind, &c.Name, &c.Detail, &c.Path, &c.Repo, &c.RepoID, &c.Present, &c.First, &c.Last, &c.Changes); err != nil {
				return err
			}
			c.Uses, c.Usable = configUses(c, c.RepoID, used, usedAll, configured)
			// A repository view lists its own configuration plus global items it actually used.
			if repoID != nil && c.Scope != "project" && c.Uses == 0 {
				return nil
			}
			v.Config = append(v.Config, c)
			return nil
		})
	if err != nil {
		return err
	}
	// Used names without any configured source: built-ins or since-removed configuration.
	for k, n := range usedAll {
		if configured[[2]string{"agent", k[1]}] && k[0] == "subagent" ||
			k[0] != "subagent" && (configured[[2]string{"skill", k[1]}] || configured[[2]string{"command", k[1]}]) {
			continue
		}
		name := k[1]
		if (k[0] == "subagent" && builtinAgents[name]) || (k[0] == "slash" && builtinCommands[name]) {
			name += " " + builtinMarker
		}
		v.Unconfigured[k[0]] = append(v.Unconfigured[k[0]], CounterRow{Name: name, Count: n})
	}
	for kind := range v.Unconfigured {
		rows := v.Unconfigured[kind]
		sort.Slice(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })
	}
	return nil
}

// configUses counts uses of one configured item. Skills can be invoked as a tool or as a
// slash command; commands only as slash commands; agents as subagent types.
func configUses(c ConfigRow, repo int64, used map[useKey]int, usedAll map[[2]string]int, configured map[[2]string]bool) (int, bool) {
	// The Skill tool also runs commands, and skills are reachable as slash commands.
	kinds := map[string][]string{"agent": {"subagent"}, "skill": {"skill", "slash"}, "command": {"slash", "skill"}}[c.Kind]
	if len(kinds) == 0 {
		return 0, false
	}
	name := usageKey(c.Name)
	configured[[2]string{c.Kind, name}] = true
	n := 0
	for _, k := range kinds {
		if c.Scope == "project" {
			n += used[useKey{repo, k, name}]
		} else {
			n += usedAll[[2]string{k, name}]
		}
	}
	return n, true
}

func attribution(ctx context.Context, db *sql.DB, repoID *int64, v *AgentView) error {
	where, args := "", []any{}
	if repoID != nil {
		where, args = "WHERE repo_id=?", []any{*repoID}
	}
	v.Attribution = []MonthAttribution{}
	return queryEach(ctx, db, `SELECT strftime('%Y-%m', author_time, 'unixepoch', 'localtime'), COUNT(*),
		SUM(agents<>''), SUM(in_session), SUM(agents<>'' OR in_session) FROM commits `+where+` GROUP BY 1 ORDER BY 1`, args,
		func(r *sql.Rows) error {
			var m MonthAttribution
			if err := r.Scan(&m.Month, &m.Commits, &m.Trailer, &m.InSession, &m.Either); err != nil {
				return err
			}
			v.Attribution = append(v.Attribution, m)
			return nil
		})
}

func agentProjects(ctx context.Context, db *sql.DB, v *AgentView) error {
	v.Projects = []ProjectRow{}
	return queryEach(ctx, db, `SELECT r.id, r.name, COUNT(s.id), IFNULL(SUM(s.active_ms),0),
		IFNULL((SELECT SUM(m.input+m.output+m.cache_read+m.cache_write) FROM session_model m JOIN agent_session s2 ON s2.id=m.session_id WHERE s2.repo_id=r.id),0),
		IFNULL((SELECT SUM(m.cost_usd) FROM session_model m JOIN agent_session s2 ON s2.id=m.session_id WHERE s2.repo_id=r.id),0),
		IFNULL((SELECT m.model FROM session_model m JOIN agent_session s2 ON s2.id=m.session_id WHERE s2.repo_id=r.id GROUP BY m.model ORDER BY SUM(m.output) DESC LIMIT 1),''),
		(SELECT COUNT(*) FROM agent_config a WHERE a.repo_id=r.id AND a.present=1),
		IFNULL(MAX(s.ended_at),0),
		IFNULL((SELECT AVG(agents<>'' OR in_session) FROM commits c WHERE c.repo_id=r.id AND c.author_time >= CAST(strftime('%s','now','-90 days') AS INTEGER)),0),
		IFNULL((SELECT AVG(in_session) FROM commits c WHERE c.repo_id=r.id AND c.author_time >= CAST(strftime('%s','now','-90 days') AS INTEGER)),0)
		FROM repo r LEFT JOIN agent_session s ON s.repo_id=r.id AND s.parent_id IS NULL
		GROUP BY r.id HAVING COUNT(s.id)>0 OR (SELECT COUNT(*) FROM agent_config a WHERE a.repo_id=r.id AND a.present=1)>0
		ORDER BY COUNT(s.id) DESC`, nil, func(r *sql.Rows) error {
		var p ProjectRow
		if err := r.Scan(&p.RepoID, &p.Name, &p.Sessions, &p.ActiveMs, &p.Tokens, &p.CostUSD, &p.TopModel, &p.Configured,
			&p.LastSession, &p.AgentShare, &p.SessionShare); err != nil {
			return err
		}
		v.Projects = append(v.Projects, p)
		return nil
	})
}

func unmatched(ctx context.Context, db *sql.DB, v *AgentView) error {
	v.Unmatched = []CWDCount{}
	return queryEach(ctx, db, `SELECT cwd, COUNT(*) FROM agent_session WHERE repo_id IS NULL AND parent_id IS NULL
		GROUP BY cwd ORDER BY COUNT(*) DESC LIMIT ?`, []any{topUnmatched}, func(r *sql.Rows) error {
		var c CWDCount
		if err := r.Scan(&c.CWD, &c.Sessions); err != nil {
			return err
		}
		v.Unmatched = append(v.Unmatched, c)
		return nil
	})
}

func otherAgents(ctx context.Context, db *sql.DB, v *AgentView) error {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='other_agents'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("other agents: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &v.OtherAgents); err != nil {
		return fmt.Errorf("decode other agents: %w", err)
	}
	return nil
}
