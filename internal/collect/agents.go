package collect

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"repolens/internal/agentcfg"
	"repolens/internal/config"
	"repolens/internal/store"
	"repolens/internal/transcript"
)

const (
	sourceClaude = "claude-jsonl"
	sourceCodex  = "codex-jsonl"
	sourceConfig = "fs-config"
	// commitGrace extends activity windows: commits often land just after the last record.
	commitGrace = 10 * time.Minute
)

// Agents ingests agent configuration and transcripts, then attributes commits to sessions.
func Agents(ctx context.Context, st *store.Store, cfg config.Config, repos []LocalRepo, log io.Writer, sum *Summary) error {
	if err := syncConfig(ctx, st, cfg, repos, sum); err != nil {
		return err
	}
	if err := resetOnParserChange(ctx, st); err != nil {
		return err
	}
	m, err := loadMatcher(ctx, st.DB, cfg.SessionPaths)
	if err != nil {
		return err
	}
	claudeFiles, err := claudeSessionFiles(filepath.Join(cfg.ClaudeHome, "projects"))
	if err != nil {
		return err
	}
	codexFiles, err := codexRollouts(filepath.Join(cfg.CodexHome, "sessions"))
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "Agenti: %d souborů Claude Code, %d rolloutů Codex…\n", len(claudeFiles), len(codexFiles))
	for _, f := range claudeFiles {
		ingest(ctx, st, cfg, m, f, sourceClaude, transcript.ParseClaudeSession, sum)
	}
	for _, f := range codexFiles {
		ingest(ctx, st, cfg, m, f, sourceCodex, transcript.ParseCodexRollout, sum)
	}
	if err := rematch(ctx, st, m); err != nil {
		return err
	}
	return detectOtherAgents(ctx, st)
}

// rematch retries sessions stored without a repository; a later run may have discovered the
// repository (e.g. collect --dir over a different root), while the transcript is unchanged.
func rematch(ctx context.Context, st *store.Store, m *matcher) error {
	type row struct{ id, cwd, file string }
	var rows []row
	err := func() error {
		rs, err := st.DB.QueryContext(ctx, `SELECT id, cwd, source_file FROM agent_session WHERE repo_id IS NULL`)
		if err != nil {
			return fmt.Errorf("read unmatched sessions: %w", err)
		}
		defer rs.Close()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.id, &r.cwd, &r.file); err != nil {
				return fmt.Errorf("scan unmatched session: %w", err)
			}
			rows = append(rows, r)
		}
		return rs.Err()
	}()
	if err != nil {
		return err
	}
	return st.Tx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			repoID, wt := m.match(r.cwd, r.file)
			if repoID == nil {
				continue
			}
			if _, err := tx.Exec(`UPDATE agent_session SET repo_id=?, in_worktree=? WHERE id=?`, repoID, boolInt(wt), r.id); err != nil {
				return fmt.Errorf("rematch session: %w", err)
			}
		}
		return nil
	})
}

// resetOnParserChange forgets ingest state when the parser changed, so every file is re-read.
func resetOnParserChange(ctx context.Context, st *store.Store) error {
	var v string
	err := st.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='parser_version'`).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read parser version: %w", err)
	}
	if v == transcript.ParserVersion {
		return nil
	}
	return st.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM ingest_file`); err != nil {
			return fmt.Errorf("reset ingest state: %w", err)
		}
		_, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('parser_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, transcript.ParserVersion)
		return err
	})
}

// signature identifies a transcript version; Claude sessions include their subagent folder.
type signature struct{ size, mtime int64 }

func fileSignature(path string, withSubagents bool) (signature, error) {
	st, err := os.Stat(path)
	if err != nil {
		return signature{}, fmt.Errorf("stat %s: %w", path, err)
	}
	sig := signature{st.Size(), st.ModTime().UnixNano()}
	if !withSubagents {
		return sig, nil
	}
	dir := strings.TrimSuffix(path, ".jsonl") + string(filepath.Separator) + "subagents"
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return sig, nil
	}
	if err != nil {
		return sig, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return sig, fmt.Errorf("stat %s: %w", e.Name(), err)
		}
		sig.size += info.Size()
		sig.mtime = max(sig.mtime, info.ModTime().UnixNano())
	}
	return sig, nil
}

func claudeSessionFiles(root string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("list claude transcripts: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func codexRollouts(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if !d.IsDir() && transcript.IsCodexRollout(d.Name()) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list codex rollouts: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func ingest(ctx context.Context, st *store.Store, cfg config.Config, m *matcher, path, source string,
	parse func(string) (transcript.FileResult, error), sum *Summary) {
	sig, err := fileSignature(path, source == sourceClaude)
	if err != nil {
		sum.fail(err)
		return
	}
	var size, mtime int64
	err = st.DB.QueryRowContext(ctx, `SELECT size, mtime_ns FROM ingest_file WHERE path=?`, path).Scan(&size, &mtime)
	if err == nil && size == sig.size && mtime == sig.mtime {
		return // unchanged since the last run
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		sum.fail(fmt.Errorf("ingest state %s: %w", path, err))
		return
	}
	res, err := parse(path)
	if err != nil {
		sum.fail(err)
		return
	}
	unrec, errs := res.Issues.Count(transcript.IssueUnrecognized), res.Issues.Count(transcript.IssueError)
	sum.Unrecognized += unrec
	sum.Errors += errs
	err = st.Tx(ctx, func(tx *sql.Tx) error {
		for _, s := range res.Sessions {
			if err := writeSession(tx, cfg, m, s, path); err != nil {
				return err
			}
		}
		if err := store.ReplaceIssues(tx, source, path, map[[2]string]int(res.Issues)); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO ingest_file(path,size,mtime_ns,records,unrecognized,errors) VALUES(?,?,?,?,?,?)
			ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns, records=excluded.records,
			unrecognized=excluded.unrecognized, errors=excluded.errors`, path, sig.size, sig.mtime, res.Records, unrec, errs)
		return err
	})
	if err != nil {
		sum.fail(fmt.Errorf("uložení %s: %w", path, err))
		return
	}
	sum.Sessions += topLevel(res.Sessions)
}

// topLevel counts sessions that were not spawned by another one, matching the database total
// in the summary (spawned threads belong to their parent).
func topLevel(sessions []*transcript.Session) int {
	n := 0
	for _, s := range sessions {
		if s.Parent == "" {
			n++
		}
	}
	return n
}

func writeSession(tx *sql.Tx, cfg config.Config, m *matcher, s *transcript.Session, path string) error {
	id := s.Tool + ":" + s.ID
	if _, err := tx.Exec(`DELETE FROM agent_session WHERE id=?`, id); err != nil {
		return fmt.Errorf("clear session: %w", err)
	}
	repoID, inWorktree := m.match(s.CWD, path)
	var parent any
	if s.Parent != "" {
		parent = s.Tool + ":" + s.Parent
	}
	if _, err := tx.Exec(`INSERT INTO agent_session(id,tool,parent_id,repo_id,cwd,in_worktree,started_at,ended_at,active_ms,turns,source_file)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, s.Tool, parent, repoID, s.CWD, boolInt(inWorktree),
		s.Started.Unix(), s.Ended.Unix(), s.ActiveMs, s.Turns, path); err != nil {
		return fmt.Errorf("insert session %s: %w", id, err)
	}
	for _, w := range s.Windows {
		if _, err := tx.Exec(`INSERT INTO agent_window(session_id,start_at,end_at) VALUES(?,?,?)`, id, w.Start.Unix(), w.End.Unix()); err != nil {
			return fmt.Errorf("insert window: %w", err)
		}
	}
	if err := writeModels(tx, cfg, id, s); err != nil {
		return err
	}
	for k, c := range s.Counters {
		if _, err := tx.Exec(`INSERT INTO session_counter(session_id,kind,name,count,errors,duration_ms) VALUES(?,?,?,?,?,?)`,
			id, k.Kind, k.Name, c.Count, c.Errors, c.DurationMs); err != nil {
			return fmt.Errorf("insert counter: %w", err)
		}
	}
	return nil
}

// writeModels stores token usage per model. Cost comes from the agent's own report when present,
// otherwise from the configured price table; both are estimates and labelled by source.
func writeModels(tx *sql.Tx, cfg config.Config, id string, s *transcript.Session) error {
	names := map[string]bool{}
	for n := range s.Models {
		names[n] = true
	}
	for n := range s.ReportedCost {
		names[n] = true // e.g. background models with cost but no transcript records
	}
	for name := range names {
		u := s.Models[name]
		if u == nil {
			u = &transcript.ModelUsage{}
		}
		var cost any
		source := ""
		if c, ok := s.ReportedCost[name]; ok {
			cost, source = c, "reported"
		} else if p, ok := lookupPrice(cfg, name); ok {
			cost, source = estimate(p, u), "estimate"
		}
		if _, err := tx.Exec(`INSERT INTO session_model(session_id,model,requests,input,output,cache_read,cache_write,reasoning,cost_usd,cost_source)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, id, name, u.Requests, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Reasoning, cost, source); err != nil {
			return fmt.Errorf("insert model usage: %w", err)
		}
	}
	return nil
}

const perMillion = 1e6

// priceFor finds a price by exact model name or the longest configured prefix.
// lookupPrice finds the price of a model: the user's list first (full name or prefix), then the
// fetched list by exact name only (its thousands of keys would make prefixes ambiguous).
func lookupPrice(cfg config.Config, model string) (config.Price, bool) {
	if p, ok := priceFor(cfg.Prices, model); ok {
		return p, true
	}
	p, ok := cfg.ListPrices[model]
	return p, ok
}

func priceFor(prices map[string]config.Price, model string) (config.Price, bool) {
	if p, ok := prices[model]; ok {
		return p, true
	}
	best, found := "", false
	for k := range prices {
		if strings.HasPrefix(model, k) && len(k) > len(best) {
			best, found = k, true
		}
	}
	return prices[best], found
}

func estimate(p config.Price, u *transcript.ModelUsage) float64 {
	return usage{u.Input, u.Output, u.CacheRead, u.CacheWrite}.cost(p)
}

// matcher maps a working directory to the repository (or worktree) containing it.
type matcher struct {
	roots   []matchRoot          // sorted longest path first
	encoded map[string]matchRoot // Claude's project-dir encoding of each root
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9-]`)

// claudeEncode mirrors how Claude Code names ~/.claude/projects folders and scratchpads.
func claudeEncode(path string) string { return nonAlnum.ReplaceAllString(path, "-") }

type matchRoot struct {
	path     string
	repoID   int64
	worktree bool
}

// loadMatcher reads repository, worktree and clone paths, plus the manual session paths of the
// configuration (directory → repository key) for sessions in moved or deleted clones.
func loadMatcher(ctx context.Context, db *sql.DB, manual map[string]string) (*matcher, error) {
	m := &matcher{}
	rows, err := db.QueryContext(ctx, `SELECT local_path, id, 0 FROM repo WHERE local_path IS NOT NULL
		UNION ALL SELECT path, repo_id, 1 FROM worktree`)
	if err != nil {
		return nil, fmt.Errorf("load repo paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r matchRoot
		if err := rows.Scan(&r.path, &r.repoID, &r.worktree); err != nil {
			return nil, fmt.Errorf("scan repo path: %w", err)
		}
		m.roots = append(m.roots, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo paths: %w", err)
	}
	for path, key := range manual {
		var id int64
		err := db.QueryRowContext(ctx, `SELECT id FROM repo WHERE key=?`, key).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue // the repository is not collected (yet)
		}
		if err != nil {
			return nil, fmt.Errorf("manual session path %s: %w", path, err)
		}
		m.roots = append(m.roots, matchRoot{path: path, repoID: id})
	}
	sort.Slice(m.roots, func(i, j int) bool { return len(m.roots[i].path) > len(m.roots[j].path) })
	m.encoded = make(map[string]matchRoot, len(m.roots))
	for _, r := range m.roots {
		m.encoded[claudeEncode(r.path)] = r
	}
	return m, nil
}

// match finds the repository for a session: by cwd prefix first, then by an encoded project
// name inside the cwd (scratchpads such as /tmp/claude-1000/-home-u-projects-x/<id>/...) or in
// the transcript's folder name.
func (m *matcher) match(cwd, transcriptPath string) (any, bool) {
	cwd = filepath.Clean(cwd)
	for _, r := range m.roots {
		if cwd == r.path || strings.HasPrefix(cwd, r.path+string(filepath.Separator)) {
			return r.repoID, r.worktree
		}
	}
	candidates := append(strings.Split(cwd, string(filepath.Separator)), filepath.Base(filepath.Dir(transcriptPath)))
	for _, seg := range candidates {
		if r, ok := m.encoded[seg]; ok && seg != "" {
			return r.repoID, r.worktree
		}
	}
	return nil, false
}

// MarkCommits flags commits authored inside (or just after) an agent session's activity window
// in the same repository. This catches agent work when Co-Authored-By trailers are disabled.
func MarkCommits(ctx context.Context, st *store.Store) error {
	grace := int64(commitGrace.Seconds())
	// Only rows whose flag changes are written.
	_, err := st.DB.ExecContext(ctx, `UPDATE commits SET in_session = 1 - in_session
		WHERE in_session <> EXISTS (
			SELECT 1 FROM agent_window w JOIN agent_session s ON s.id = w.session_id
			WHERE s.repo_id = commits.repo_id AND commits.author_time BETWEEN w.start_at AND w.end_at + ?)`, grace)
	if err != nil {
		return fmt.Errorf("mark commits: %w", err)
	}
	return nil
}

// syncConfig replaces the stored agent configuration with what is on disk now.
func syncConfig(ctx context.Context, st *store.Store, cfg config.Config, repos []LocalRepo, sum *Summary) error {
	global, problems := agentcfg.Global(cfg.ClaudeHome, cfg.CodexHome)
	for _, p := range problems {
		sum.fail(fmt.Errorf("konfigurace %s: %s", p.Path, p.Err))
	}
	err := st.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM agent_config WHERE repo_id IS NULL`); err != nil {
			return fmt.Errorf("clear global config: %w", err)
		}
		return insertConfig(tx, nil, global)
	})
	if err != nil {
		return err
	}
	for _, r := range repos {
		items, problems := agentcfg.Project(r.Path)
		for _, p := range problems {
			sum.fail(fmt.Errorf("konfigurace %s: %s", p.Path, p.Err))
		}
		hist, err := agentcfg.GitHistory(ctx, r.Path)
		if err != nil {
			sum.fail(err)
		}
		items = agentcfg.Annotate(items, hist)
		err = st.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM agent_config WHERE repo_id=?`, r.ID); err != nil {
				return fmt.Errorf("clear repo config: %w", err)
			}
			return insertConfig(tx, r.ID, items)
		})
		if err != nil {
			sum.fail(fmt.Errorf("%s: %w", r.Path, err))
		}
	}
	return nil
}

func insertConfig(tx *sql.Tx, repoID any, items []agentcfg.Item) error {
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO agent_config(repo_id,scope,tool,kind,name,path,detail,first_time,last_time,changes,present)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare config insert: %w", err)
	}
	defer stmt.Close()
	for _, it := range items {
		var first, last any
		if !it.First.IsZero() {
			first, last = it.First.Unix(), it.Last.Unix()
		}
		if _, err := stmt.Exec(repoID, it.Scope, it.Tool, it.Kind, it.Name, it.Path, it.Detail, first, last, it.Changes, boolInt(it.Present)); err != nil {
			return fmt.Errorf("insert config %s: %w", it.Name, err)
		}
	}
	return nil
}

// otherAgentDirs are locations of agents whose transcript formats repolens does not parse yet.
var otherAgentDirs = []struct{ name, dir string }{
	{"Cursor", ".cursor/chats"},
	{"Cursor (projekty)", ".cursor/projects"},
	{"Gemini CLI", ".gemini/tmp"},
	{"GitHub Copilot", ".config/github-copilot"},
}

// detectOtherAgents counts files of unparsed agents so the UI can say they exist.
func detectOtherAgents(ctx context.Context, st *store.Store) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	found := map[string]int{}
	for _, a := range otherAgentDirs {
		n := 0
		err := filepath.WalkDir(filepath.Join(home, a.dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return filepath.SkipDir // missing or unreadable: nothing to count
			}
			if !d.IsDir() {
				n++
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan %s: %w", a.dir, err)
		}
		if n > 0 {
			found[a.name] = n
		}
	}
	b, err := json.Marshal(found)
	if err != nil {
		return fmt.Errorf("encode other agents: %w", err)
	}
	_, err = st.DB.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('other_agents',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(b))
	return err
}
