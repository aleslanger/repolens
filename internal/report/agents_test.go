package report

import (
	"context"
	"path/filepath"
	"testing"

	"repolens/internal/store"
)

// Per-tool and per-day token totals must include agent-spawned threads, like the overall total.
func TestTokenBreakdownsIncludeChildThreads(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO agent_session(id,tool,parent_id,cwd,started_at,ended_at,source_file) VALUES
		('codex:p','codex',NULL,'/x',1790000000,1790000100,'a'), ('codex:c','codex','codex:p','/x',1790000010,1790000050,'b')`)
	exec(`INSERT INTO session_model(session_id,model,requests,input,output,cache_read,cache_write,reasoning,cost_usd,cost_source) VALUES
		('codex:p','gpt',1,100,10,0,0,0,1.0,'estimate'), ('codex:c','gpt',1,50,5,0,0,0,0.5,'estimate')`)

	v, err := AgentsFor(context.Background(), st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.Tokens != 165 || v.Totals.Sessions != 1 || v.Totals.Threads != 1 {
		t.Fatalf("totals = %+v", v.Totals)
	}
	if len(v.ByTool) != 1 || v.ByTool[0].Tokens != 165 || v.ByTool[0].CostUSD != 1.5 {
		t.Errorf("by tool = %+v, want 165 tokens and $1.5 including the child thread", v.ByTool)
	}
	var daily int64
	for _, d := range v.Daily {
		daily += d.Tokens
	}
	if daily != 165 {
		t.Errorf("daily tokens = %d, want 165", daily)
	}
}

func TestPortfolioIsAnEmptyListWithoutData(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pf, err := Portfolio(context.Background(), st.DB, timeNow())
	if err != nil || pf == nil || len(pf) != 0 {
		t.Errorf("portfolio = %#v, %v; want an empty, non-nil list", pf, err)
	}
}
