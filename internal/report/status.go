package report

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type Run struct {
	Mode         string `json:"mode"`
	Target       string `json:"target"`
	StartedAt    int64  `json:"startedAt"`
	FinishedAt   int64  `json:"finishedAt"`
	Repos        int    `json:"repos"`
	Skipped      int    `json:"skipped"`
	Commits      int    `json:"commits"`
	Sessions     int    `json:"sessions"`
	Unrecognized int    `json:"unrecognized"`
	Errors       int    `json:"errors"`
}

type IssueRow struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	What   string `json:"what"`
	Count  int    `json:"count"`
}

type Status struct {
	Runs   []Run      `json:"runs"`
	Issues []IssueRow `json:"issues"`
	// LastErrors are the errors of the latest finished run, with what they concern.
	LastErrors []RunError `json:"lastErrors"`
}

// RunError is one error of a run; Target is a directory or repository key ("" when none).
type RunError struct {
	Target  string `json:"target"`
	Message string `json:"message"`
}

const statusRuns = 10

// StatusFor lists recent collection runs and the aggregated parse issues.
func StatusFor(ctx context.Context, db *sql.DB) (*Status, error) {
	s := &Status{Runs: []Run{}, Issues: []IssueRow{}, LastErrors: []RunError{}}
	err := queryEach(ctx, db, `SELECT target, message FROM collect_error
		WHERE run_id=(SELECT MAX(id) FROM collect_run WHERE finished_at IS NOT NULL) ORDER BY rowid`, nil, func(r *sql.Rows) error {
		var e RunError
		if err := r.Scan(&e.Target, &e.Message); err != nil {
			return err
		}
		s.LastErrors = append(s.LastErrors, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = queryEach(ctx, db, `SELECT mode, target, started_at, IFNULL(finished_at,0), repos, skipped, commits, sessions, unrecognized, errors
		FROM collect_run ORDER BY id DESC LIMIT ?`, []any{statusRuns}, func(r *sql.Rows) error {
		var x Run
		if err := r.Scan(&x.Mode, &x.Target, &x.StartedAt, &x.FinishedAt, &x.Repos, &x.Skipped, &x.Commits, &x.Sessions, &x.Unrecognized, &x.Errors); err != nil {
			return err
		}
		s.Runs = append(s.Runs, x)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = queryEach(ctx, db, `SELECT source, kind, what, SUM(count) FROM parse_issue GROUP BY 1,2,3 ORDER BY 4 DESC`, nil,
		func(r *sql.Rows) error {
			var x IssueRow
			if err := r.Scan(&x.Source, &x.Kind, &x.What, &x.Count); err != nil {
				return err
			}
			s.Issues = append(s.Issues, x)
			return nil
		})
	return s, err
}

// LastRunInfo is the latest finished collection, shown in the page header.
type LastRunInfo struct {
	FinishedAt int64  `json:"finishedAt"`
	Mode       string `json:"mode"`
	Errors     int    `json:"errors"`
}

// LastRun returns the latest finished collection or nil when there is none.
func LastRun(ctx context.Context, db *sql.DB) (*LastRunInfo, error) {
	var x LastRunInfo
	err := db.QueryRowContext(ctx, `SELECT finished_at, mode, errors FROM collect_run WHERE finished_at IS NOT NULL ORDER BY finished_at DESC LIMIT 1`).
		Scan(&x.FinishedAt, &x.Mode, &x.Errors)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("last run: %w", err)
	}
	return &x, nil
}
