package collect

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"repolens/internal/config"
	"repolens/internal/github"
	"repolens/internal/store"
)

// priceList refreshes the stored public price list and returns it. Without GitHub, or when the
// fetch fails, the list stored by an earlier run is used (a failure counts as an error).
func priceList(ctx context.Context, st *store.Store, gh *github.Client, log io.Writer, sum *Summary) (map[string]config.Price, error) {
	switch {
	case gh == nil:
		fmt.Fprintln(log, "Ceník z GitHubu: GitHub není k dispozici, používám uložený.")
	default:
		list, err := gh.FetchPriceList(ctx)
		if err != nil {
			sum.fail(err)
			break
		}
		if err := storePriceList(ctx, st, list); err != nil {
			return nil, err
		}
		fmt.Fprintf(log, "Ceník %s k %s: %d modelů\n", github.PriceListSource, list.Date, len(list.Prices))
	}
	return loadPriceList(ctx, st.DB)
}

func storePriceList(ctx context.Context, st *store.Store, list *github.PriceList) error {
	return st.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM price_list`); err != nil {
			return fmt.Errorf("clear price list: %w", err)
		}
		stmt, err := tx.Prepare(`INSERT INTO price_list(model,input,output,cache_read,cache_write) VALUES(?,?,?,?,?)`)
		if err != nil {
			return fmt.Errorf("prepare price list: %w", err)
		}
		defer stmt.Close()
		for m, p := range list.Prices {
			if _, err := stmt.Exec(m, p.Input, p.Output, p.CacheRead, p.CacheWrite); err != nil {
				return fmt.Errorf("store price %s: %w", m, err)
			}
		}
		_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES('price_list_date',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, list.Date)
		return err
	})
}

func loadPriceList(ctx context.Context, db *sql.DB) (map[string]config.Price, error) {
	out := map[string]config.Price{}
	rows, err := db.QueryContext(ctx, `SELECT model, input, output, cache_read, cache_write FROM price_list`)
	if err != nil {
		return nil, fmt.Errorf("load price list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		var p config.Price
		if err := rows.Scan(&m, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite); err != nil {
			return nil, fmt.Errorf("scan price: %w", err)
		}
		out[m] = p
	}
	return out, rows.Err()
}

// Reprice recomputes estimated costs of stored sessions with the current prices. Costs the agent
// reported itself are never touched; a model that lost its price loses its estimate.
func Reprice(ctx context.Context, st *store.Store, cfg config.Config) error {
	type row struct {
		id    int64
		model string
		u     usage
	}
	var todo []row
	rows, err := st.DB.QueryContext(ctx, `SELECT rowid, model, input, output, cache_read, cache_write FROM session_model WHERE cost_source IN ('','estimate')`)
	if err != nil {
		return fmt.Errorf("reprice: %w", err)
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.model, &r.u.input, &r.u.output, &r.u.cacheRead, &r.u.cacheWrite); err != nil {
			rows.Close()
			return fmt.Errorf("reprice scan: %w", err)
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reprice rows: %w", err)
	}
	return st.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`UPDATE session_model SET cost_usd=?, cost_source=? WHERE rowid=?`)
		if err != nil {
			return fmt.Errorf("prepare reprice: %w", err)
		}
		defer stmt.Close()
		for _, r := range todo {
			var cost any
			source := ""
			if p, ok := lookupPrice(cfg, r.model); ok {
				cost, source = r.u.cost(p), "estimate"
			}
			if _, err := stmt.Exec(cost, source, r.id); err != nil {
				return fmt.Errorf("reprice %s: %w", r.model, err)
			}
		}
		return nil
	})
}

// usage holds the token columns of a stored model row.
type usage struct{ input, output, cacheRead, cacheWrite int64 }

func (u usage) cost(p config.Price) float64 {
	return (float64(u.input)*p.Input + float64(u.output)*p.Output +
		float64(u.cacheRead)*p.CacheRead + float64(u.cacheWrite)*p.CacheWrite) / perMillion
}
