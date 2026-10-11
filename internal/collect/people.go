package collect

import (
	"context"
	"database/sql"
	"fmt"

	"repolens/internal/config"
	"repolens/internal/identity"
	"repolens/internal/store"
)

// People rebuilds merged identities from all commits; it is cheap and runs after every collection.
func People(ctx context.Context, st *store.Store, cfg config.Config) error {
	var ids []identity.Identity
	rows, err := st.DB.QueryContext(ctx, `SELECT author_name, author_email, COUNT(*) FROM commits GROUP BY 1, 2`)
	if err != nil {
		return fmt.Errorf("read identities: %w", err)
	}
	for rows.Next() {
		var id identity.Identity
		if err := rows.Scan(&id.Name, &id.Email, &id.Commits); err != nil {
			rows.Close()
			return fmt.Errorf("scan identity: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close identities: %w", err)
	}
	people := identity.Merge(ids, cfg.Identities)
	return st.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM identity`); err != nil {
			return fmt.Errorf("clear identities: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM person`); err != nil {
			return fmt.Errorf("clear people: %w", err)
		}
		for _, p := range people {
			var pid int64
			if err := tx.QueryRow(`INSERT INTO person(name,is_bot) VALUES(?,?) RETURNING id`, p.Name, boolInt(p.Bot)).Scan(&pid); err != nil {
				return fmt.Errorf("insert person: %w", err)
			}
			for _, m := range p.Members {
				via := m.Via
				if via == "" {
					via = "hlavní"
				}
				if _, err := tx.Exec(`INSERT INTO identity(name,email,person_id,via) VALUES(?,?,?,?)`, m.Name, m.Email, pid, via); err != nil {
					return fmt.Errorf("insert identity: %w", err)
				}
			}
		}
		return nil
	})
}
