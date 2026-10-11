package report

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// sessionListMax bounds the session list on the agents page; older sessions are reached through
// a shorter period.
const sessionListMax = 200

// SessionRow is one top-level session; tokens and cost include its spawned threads.
type SessionRow struct {
	ID        string  `json:"id"`
	Tool      string  `json:"tool"`
	RepoID    int64   `json:"repoId"`
	Repo      string  `json:"repo"`
	CWD       string  `json:"cwd"`
	StartedAt int64   `json:"startedAt"`
	EndedAt   int64   `json:"endedAt"`
	ActiveMs  int64   `json:"activeMs"`
	Turns     int     `json:"turns"`
	Tokens    int64   `json:"tokens"`
	Fresh     int64   `json:"fresh"`
	CostUSD   float64 `json:"costUsd"`
	Threads   int     `json:"threads"`
	TopModel  string  `json:"topModel"`
}

const sessionColumns = `s.id, s.tool, IFNULL(s.repo_id,0), IFNULL(r.name,''), s.cwd, s.started_at, s.ended_at, s.active_ms, s.turns,
	IFNULL(` + familyTokens + `,0), IFNULL(` + familyFresh + `,0), IFNULL(` + familyCost + `,0),
	(SELECT COUNT(*) FROM agent_session c WHERE c.parent_id=s.id),
	IFNULL((SELECT m.model FROM session_model m JOIN agent_session c ON c.id=m.session_id WHERE c.id=s.id OR c.parent_id=s.id
		GROUP BY m.model ORDER BY SUM(m.output) DESC LIMIT 1),'')`

func scanSession(r interface{ Scan(...any) error }) (SessionRow, error) {
	var s SessionRow
	err := r.Scan(&s.ID, &s.Tool, &s.RepoID, &s.Repo, &s.CWD, &s.StartedAt, &s.EndedAt, &s.ActiveMs, &s.Turns,
		&s.Tokens, &s.Fresh, &s.CostUSD, &s.Threads, &s.TopModel)
	return s, err
}

func sessionList(ctx context.Context, db *sql.DB, sc scope, v *AgentView) error {
	v.Sessions = []SessionRow{}
	return queryEach(ctx, db, `SELECT `+sessionColumns+` FROM agent_session s LEFT JOIN repo r ON r.id=s.repo_id
		WHERE `+sc.where+` ORDER BY s.started_at DESC LIMIT ?`, append(append([]any{}, sc.args...), sessionListMax), func(r *sql.Rows) error {
		s, err := scanSession(r)
		if err != nil {
			return err
		}
		v.Sessions = append(v.Sessions, s)
		return nil
	})
}

// SessionView is one session with its models, named counters and spawned threads.
type SessionView struct {
	Session  SessionRow              `json:"session"`
	Models   []ModelRow              `json:"models"`
	Counters map[string][]CounterRow `json:"counters"`
	Threads  []SessionRow            `json:"threads"`
	Windows  int                     `json:"windows"`
}

// SessionDetail returns one session, or nil when the id is unknown. Like everything else it holds
// aggregates and names only, never content.
func SessionDetail(ctx context.Context, db *sql.DB, id string) (*SessionView, error) {
	row := db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM agent_session s LEFT JOIN repo r ON r.id=s.repo_id WHERE s.id=?`, id)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	v := &SessionView{Session: s, Models: []ModelRow{}, Counters: map[string][]CounterRow{}, Threads: []SessionRow{}}
	family := scope{where: "(s.id=? OR s.parent_id=?)", args: []any{id, id}}
	steps := []func() error{
		func() error { return sessionModels(ctx, db, family, v) },
		func() error { return sessionCounters(ctx, db, family, v) },
		func() error { return sessionThreads(ctx, db, id, v) },
		func() error {
			return db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_window WHERE session_id=?`, id).Scan(&v.Windows)
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, fmt.Errorf("session %s: %w", id, err)
		}
	}
	return v, nil
}

func sessionModels(ctx context.Context, db *sql.DB, family scope, v *SessionView) error {
	av := &AgentView{}
	if err := agentModels(ctx, db, family, av); err != nil {
		return err
	}
	v.Models = av.Models
	return nil
}

func sessionCounters(ctx context.Context, db *sql.DB, family scope, v *SessionView) error {
	return queryEach(ctx, db, `SELECT c.kind, c.name, SUM(c.count), SUM(c.errors), SUM(c.duration_ms), COUNT(DISTINCT c.session_id)
		FROM session_counter c JOIN agent_session s ON s.id=c.session_id WHERE `+family.where+`
		GROUP BY c.kind, c.name ORDER BY c.kind, SUM(c.count) DESC`, family.args, func(r *sql.Rows) error {
		var kind string
		var c CounterRow
		if err := r.Scan(&kind, &c.Name, &c.Count, &c.Errors, &c.DurationMs, &c.Sessions); err != nil {
			return err
		}
		v.Counters[kind] = append(v.Counters[kind], c)
		return nil
	})
}

func sessionThreads(ctx context.Context, db *sql.DB, id string, v *SessionView) error {
	return queryEach(ctx, db, `SELECT `+sessionColumns+` FROM agent_session s LEFT JOIN repo r ON r.id=s.repo_id
		WHERE s.parent_id=? ORDER BY s.started_at`, []any{id}, func(r *sql.Rows) error {
		s, err := scanSession(r)
		if err != nil {
			return err
		}
		v.Threads = append(v.Threads, s)
		return nil
	})
}

// UnpricedModel is a model whose tokens have no cost: neither reported nor in the price list.
type UnpricedModel struct {
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
}

// UnpricedModels lists models with unpriced tokens, most tokens first, for the price list editor.
func UnpricedModels(ctx context.Context, db *sql.DB) ([]UnpricedModel, error) {
	out := []UnpricedModel{}
	err := queryEach(ctx, db, `SELECT model, SUM(input+output+cache_read+cache_write) FROM session_model
		WHERE cost_usd IS NULL GROUP BY model ORDER BY 2 DESC`, nil, func(r *sql.Rows) error {
		var m UnpricedModel
		if err := r.Scan(&m.Model, &m.Tokens); err != nil {
			return err
		}
		out = append(out, m)
		return nil
	})
	return out, err
}
