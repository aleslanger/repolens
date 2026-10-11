-- repolens schema. Every data table carries `source` so the UI can show provenance:
-- git-local, github-api, claude-jsonl, codex-jsonl, fs-config.

CREATE TABLE IF NOT EXISTS repo (
  id             INTEGER PRIMARY KEY,
  key            TEXT NOT NULL UNIQUE,      -- host/owner/name, or local:<abs path>
  name           TEXT NOT NULL,
  host           TEXT, owner TEXT,
  local_path     TEXT,
  description    TEXT,
  default_branch TEXT,
  is_private     INTEGER, is_fork INTEGER, is_archived INTEGER,
  stars          INTEGER, forks INTEGER,
  open_issues    INTEGER, open_prs INTEGER,
  pushed_at      INTEGER,
  last_head      TEXT,                      -- incremental marker for local git log
  gh_synced_at   INTEGER,                   -- incremental marker for GitHub
  in_local       INTEGER NOT NULL DEFAULT 0,
  in_github      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS worktree (
  repo_id INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
  path    TEXT NOT NULL PRIMARY KEY,
  branch  TEXT
);

CREATE TABLE IF NOT EXISTS commits (
  repo_id      INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
  hash         TEXT NOT NULL,
  author_name  TEXT NOT NULL,
  author_email TEXT NOT NULL,
  author_time  INTEGER NOT NULL,
  subject      TEXT NOT NULL,
  ctype        TEXT NOT NULL,
  is_merge     INTEGER NOT NULL,
  additions    INTEGER NOT NULL,
  deletions    INTEGER NOT NULL,
  files        INTEGER NOT NULL,
  agents       TEXT NOT NULL DEFAULT '',   -- comma separated agent families (from trailers)
  in_session   INTEGER NOT NULL DEFAULT 0, -- authored inside an agent session's activity window
  source       TEXT NOT NULL,
  PRIMARY KEY (repo_id, hash)
);
CREATE INDEX IF NOT EXISTS commits_time ON commits(author_time);
CREATE INDEX IF NOT EXISTS commits_email ON commits(author_email);

CREATE TABLE IF NOT EXISTS commit_dir (
  repo_id   INTEGER NOT NULL,
  hash      TEXT NOT NULL,
  dir       TEXT NOT NULL,
  additions INTEGER NOT NULL,
  deletions INTEGER NOT NULL,
  PRIMARY KEY (repo_id, hash, dir),
  FOREIGN KEY (repo_id, hash) REFERENCES commits(repo_id, hash) ON DELETE CASCADE
);

-- Identity merging: each (name,email) maps to a person.
CREATE TABLE IF NOT EXISTS person (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  is_bot INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS identity (
  name      TEXT NOT NULL,
  email     TEXT NOT NULL,
  person_id INTEGER NOT NULL REFERENCES person(id) ON DELETE CASCADE,
  via       TEXT NOT NULL,                  -- email, name, noreply, config
  PRIMARY KEY (name, email)
);

CREATE TABLE IF NOT EXISTS gh_issue (
  repo_id    INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
  number     INTEGER NOT NULL,
  is_pr      INTEGER NOT NULL,
  state      TEXT NOT NULL,                 -- OPEN, CLOSED, MERGED
  title      TEXT NOT NULL,
  author     TEXT,
  created_at INTEGER NOT NULL,
  closed_at  INTEGER,
  merged_at  INTEGER,
  milestone  TEXT,
  labels     TEXT NOT NULL DEFAULT '',      -- comma separated
  source     TEXT NOT NULL DEFAULT 'github-api',
  PRIMARY KEY (repo_id, is_pr, number)
);

CREATE TABLE IF NOT EXISTS gh_milestone (
  repo_id       INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
  number        INTEGER NOT NULL,
  title         TEXT NOT NULL,
  state         TEXT NOT NULL,
  due_on        INTEGER,
  open_issues   INTEGER NOT NULL,
  closed_issues INTEGER NOT NULL,
  PRIMARY KEY (repo_id, number)
);

CREATE TABLE IF NOT EXISTS gh_label (
  repo_id INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
  name    TEXT NOT NULL,
  color   TEXT,
  PRIMARY KEY (repo_id, name)
);

CREATE TABLE IF NOT EXISTS gh_release (
  repo_id      INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
  tag          TEXT NOT NULL,
  name         TEXT,
  published_at INTEGER,
  prerelease   INTEGER NOT NULL,
  PRIMARY KEY (repo_id, tag)
);

CREATE TABLE IF NOT EXISTS gh_language (
  repo_id INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
  lang    TEXT NOT NULL,
  bytes   INTEGER NOT NULL,
  PRIMARY KEY (repo_id, lang)
);

CREATE TABLE IF NOT EXISTS gh_run (
  repo_id    INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
  id         INTEGER NOT NULL,
  workflow   TEXT NOT NULL,
  event      TEXT,
  branch     TEXT,
  status     TEXT NOT NULL,
  conclusion TEXT,
  created_at INTEGER NOT NULL,
  duration_s INTEGER,
  PRIMARY KEY (repo_id, id)
);

-- Agent configuration found on disk (project, global or plugin scope).
CREATE TABLE IF NOT EXISTS agent_config (
  repo_id      INTEGER REFERENCES repo(id) ON DELETE CASCADE,  -- NULL for global/plugin
  scope        TEXT NOT NULL,               -- project, global, plugin
  tool         TEXT NOT NULL,               -- claude, codex, copilot, cursor, gemini, mcp, generic
  kind         TEXT NOT NULL,               -- instructions, agent, skill, command, rule, hook, permission, mcp, settings
  name         TEXT NOT NULL,
  path         TEXT NOT NULL,
  detail       TEXT NOT NULL DEFAULT '',
  first_time   INTEGER,                     -- first commit touching the path
  last_time    INTEGER,
  changes      INTEGER NOT NULL DEFAULT 0,
  present      INTEGER NOT NULL DEFAULT 1   -- 0 = only in git history
);
CREATE UNIQUE INDEX IF NOT EXISTS agent_config_uq ON agent_config(IFNULL(repo_id, 0), scope, tool, kind, name, path, detail);

CREATE TABLE IF NOT EXISTS agent_session (
  id          TEXT PRIMARY KEY,             -- <tool>:<session id>
  tool        TEXT NOT NULL,                -- claude, codex
  parent_id   TEXT,                         -- set for agent-spawned threads (Codex subagents)
  repo_id     INTEGER REFERENCES repo(id) ON DELETE SET NULL,
  cwd         TEXT NOT NULL,
  in_worktree INTEGER NOT NULL DEFAULT 0,
  started_at  INTEGER NOT NULL,
  ended_at    INTEGER NOT NULL,
  active_ms   INTEGER NOT NULL DEFAULT 0,   -- sum of reported turn durations
  turns       INTEGER NOT NULL DEFAULT 0,
  source_file TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_session_repo ON agent_session(repo_id);
-- Spawned threads are summed into their parent (familyTokens); without this every lookup scans.
CREATE INDEX IF NOT EXISTS agent_session_parent ON agent_session(parent_id);

CREATE TABLE IF NOT EXISTS session_model (
  session_id   TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
  model        TEXT NOT NULL,
  requests     INTEGER NOT NULL,
  input        INTEGER NOT NULL,
  output       INTEGER NOT NULL,
  cache_read   INTEGER NOT NULL,
  cache_write  INTEGER NOT NULL,
  reasoning    INTEGER NOT NULL,
  cost_usd     REAL,                        -- NULL when no price is known
  cost_source  TEXT NOT NULL DEFAULT '',    -- reported (by the agent) or estimate (price table)
  PRIMARY KEY (session_id, model)
);

-- Spans of continuous activity, used to attribute commits to sessions.
CREATE TABLE IF NOT EXISTS agent_window (
  session_id TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
  start_at   INTEGER NOT NULL,
  end_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_window_session ON agent_window(session_id);

-- Named counters per session: tool, subagent, skill, slash, denial, hook, tool_error.
CREATE TABLE IF NOT EXISTS session_counter (
  session_id  TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  name        TEXT NOT NULL,
  count       INTEGER NOT NULL,
  errors      INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, kind, name)
);

-- Incremental ingestion of transcript files.
CREATE TABLE IF NOT EXISTS ingest_file (
  path         TEXT PRIMARY KEY,
  size         INTEGER NOT NULL,
  mtime_ns     INTEGER NOT NULL,
  records      INTEGER NOT NULL,
  unrecognized INTEGER NOT NULL,
  errors       INTEGER NOT NULL
);

-- Unrecognized records and parse errors, aggregated by what they were.
CREATE TABLE IF NOT EXISTS parse_issue (
  source TEXT NOT NULL,                     -- claude-jsonl, codex-jsonl, git-local, github-api, ...
  scope  TEXT NOT NULL,                     -- file path or repo key the issue came from
  kind   TEXT NOT NULL,                     -- unrecognized, error
  what   TEXT NOT NULL,
  count  INTEGER NOT NULL,
  PRIMARY KEY (source, scope, kind, what)
);

CREATE TABLE IF NOT EXISTS http_cache (
  url        TEXT PRIMARY KEY,
  etag       TEXT,
  link       TEXT,
  body       BLOB NOT NULL,
  fetched_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS collect_run (
  id           INTEGER PRIMARY KEY,
  mode         TEXT NOT NULL,
  target       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,
  finished_at  INTEGER,
  repos        INTEGER NOT NULL DEFAULT 0,
  skipped      INTEGER NOT NULL DEFAULT 0,
  commits      INTEGER NOT NULL DEFAULT 0,
  sessions     INTEGER NOT NULL DEFAULT 0,
  unrecognized INTEGER NOT NULL DEFAULT 0,
  errors       INTEGER NOT NULL DEFAULT 0
);

-- Errors of each collection run, with the directory or repository they concern.
CREATE TABLE IF NOT EXISTS collect_error (
  run_id  INTEGER NOT NULL REFERENCES collect_run(id) ON DELETE CASCADE,
  target  TEXT NOT NULL,
  message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS collect_error_run ON collect_error(run_id);

-- The public price list (USD per million tokens) last fetched through the GitHub API.
CREATE TABLE IF NOT EXISTS price_list (
  model       TEXT PRIMARY KEY,
  input       REAL NOT NULL,
  output      REAL NOT NULL,
  cache_read  REAL NOT NULL,
  cache_write REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
