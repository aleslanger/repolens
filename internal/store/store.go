// Package store owns the SQLite database: schema, writes from collectors and the issue log.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	DB *sql.DB
}

// Open creates the database file if needed and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY between goroutines.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

// addedColumns lists columns introduced after the first schema; CREATE TABLE IF NOT EXISTS
// does not add them to an existing database.
var addedColumns = []struct{ table, column, ddl string }{
	{"commits", "in_session", "INTEGER NOT NULL DEFAULT 0"},
	{"agent_session", "parent_id", "TEXT"},
	{"http_cache", "link", "TEXT"},
	{"repo", "scanned_at", "INTEGER"},
}

func migrate(db *sql.DB) error {
	for _, c := range addedColumns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.column).Scan(&n); err != nil {
			return fmt.Errorf("inspect %s: %w", c.table, err)
		}
		if n > 0 {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.ddl)); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.DB.Close() }

// Tx runs fn in a transaction, committing on success.
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Issue is one aggregated parse problem.
type Issue struct {
	Source, Scope, Kind, What string
	Count                     int
}

// ReplaceIssues swaps all issues for (source, scope) with the given set.
func ReplaceIssues(tx *sql.Tx, source, scope string, issues map[[2]string]int) error {
	if _, err := tx.Exec(`DELETE FROM parse_issue WHERE source=? AND scope=?`, source, scope); err != nil {
		return fmt.Errorf("clear issues: %w", err)
	}
	for k, n := range issues {
		if _, err := tx.Exec(`INSERT INTO parse_issue(source,scope,kind,what,count) VALUES(?,?,?,?,?)`,
			source, scope, k[0], k[1], n); err != nil {
			return fmt.Errorf("insert issue: %w", err)
		}
	}
	return nil
}
