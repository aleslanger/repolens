package report

import (
	"context"
	"testing"

	"repolens/internal/store"
)

const later = monday + 30*86400

// seedSessions stores an old and a new session in one repository; the new one spawned a thread.
func seedSessions(t *testing.T) *store.Store {
	t.Helper()
	st := trendStore(t)
	mustExec(t, st, `INSERT INTO repo(id,key,name,in_local) VALUES(1,'local:/r','r',1)`)
	mustExec(t, st, `INSERT INTO agent_session(id,tool,parent_id,repo_id,cwd,started_at,ended_at,active_ms,turns,source_file) VALUES
		('claude:old','claude',NULL,1,'/r',?,?,60000,2,'f'),
		('claude:new','claude',NULL,1,'/r',?,?,120000,5,'f'),
		('codex:kid','codex','claude:new',1,'/r',?,?,1000,1,'f')`,
		monday, monday+60, later, later+120, later+10, later+20)
	mustExec(t, st, `INSERT INTO session_model(session_id,model,requests,input,output,cache_read,cache_write,reasoning,cost_usd,cost_source) VALUES
		('claude:old','claude-sonnet-5',1,10,1,0,0,0,0.5,'reported'),
		('claude:new','claude-opus-5',2,100,10,0,0,0,2.0,'reported'),
		('codex:kid','gpt-5',1,40,4,0,0,0,NULL,'')`)
	mustExec(t, st, `INSERT INTO session_counter(session_id,kind,name,count,errors) VALUES
		('claude:new','tool','Bash',7,1), ('claude:new','skill','deploy',1,0), ('claude:old','tool','Read',3,0)`)
	return st
}

func TestAgentsSinceFiltersByPeriod(t *testing.T) {
	st := seedSessions(t)
	v, err := AgentsSince(context.Background(), st.DB, nil, later-86400)
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.Sessions != 1 || v.Totals.Tokens != 154 || len(v.Models) != 2 {
		t.Errorf("totals = %+v, models = %d; want only the new session with its thread", v.Totals, len(v.Models))
	}
	if tools := v.Counters["tool"]; len(tools) != 1 || tools[0].Name != "Bash" {
		t.Errorf("tool counters = %+v, want only Bash", tools)
	}
	if v.Durations.Sessions != 1 || len(v.Sessions) != 1 {
		t.Errorf("durations %+v, sessions %d", v.Durations, len(v.Sessions))
	}
	all, err := AgentsFor(context.Background(), st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if all.Totals.Sessions != 2 || all.Since != 0 || v.Since != later-86400 {
		t.Errorf("all-time sessions = %d, since = %d / %d", all.Totals.Sessions, all.Since, v.Since)
	}
}

func TestDailyCostIncludesThreads(t *testing.T) {
	st := seedSessions(t)
	v, err := AgentsFor(context.Background(), st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cost float64
	var tokens int64
	for _, d := range v.Daily {
		cost += d.CostUSD
		tokens += d.Tokens
	}
	if cost != 2.5 || tokens != 165 {
		t.Errorf("daily cost = %v tokens = %d, want 2.5 and 165", cost, tokens)
	}
}

func TestSessionListNewestFirstWithFamilyTotals(t *testing.T) {
	st := seedSessions(t)
	v, err := AgentsFor(context.Background(), st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Sessions) != 2 {
		t.Fatalf("sessions = %+v", v.Sessions)
	}
	s := v.Sessions[0]
	if s.ID != "claude:new" || s.Repo != "r" || s.RepoID != 1 || s.Tokens != 154 || s.CostUSD != 2.0 || s.Threads != 1 || s.TopModel != "claude-opus-5" {
		t.Errorf("first row = %+v", s)
	}
}

func TestSessionDetail(t *testing.T) {
	st := seedSessions(t)
	d, err := SessionDetail(context.Background(), st.DB, "claude:new")
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Session.Turns != 5 || len(d.Models) != 2 || len(d.Threads) != 1 || d.Threads[0].ID != "codex:kid" {
		t.Fatalf("detail = %+v", d)
	}
	if c := d.Counters["tool"]; len(c) != 1 || c[0].Name != "Bash" || c[0].Errors != 1 {
		t.Errorf("tool counters = %+v", c)
	}
	missing, err := SessionDetail(context.Background(), st.DB, "claude:nope")
	if err != nil || missing != nil {
		t.Errorf("unknown session = %+v, %v; want nil, nil", missing, err)
	}
}

func TestUnpricedModels(t *testing.T) {
	st := seedSessions(t)
	got, err := UnpricedModels(context.Background(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Model != "gpt-5" || got[0].Tokens != 44 {
		t.Errorf("unpriced = %+v, want gpt-5 with 44 tokens", got)
	}
}

// A thread belongs to its parent's period: a parent outside the period hides its thread too, so
// totals agree with the per-tool and per-session numbers.
func TestThreadsFollowTheirParentsPeriod(t *testing.T) {
	st := trendStore(t)
	mustExec(t, st, `INSERT INTO agent_session(id,tool,parent_id,cwd,started_at,ended_at,source_file) VALUES
		('codex:p','codex',NULL,'/x',?,?,'f'), ('codex:t','codex','codex:p','/x',?,?,'f')`, monday, monday+60, later, later+60)
	mustExec(t, st, `INSERT INTO session_model(session_id,model,requests,input,output,cache_read,cache_write,reasoning,cost_usd,cost_source) VALUES
		('codex:t','gpt-5',1,40,4,0,0,0,NULL,'')`)
	mustExec(t, st, `INSERT INTO session_counter(session_id,kind,name,count) VALUES('codex:t','tool','exec_command',2)`)
	v, err := AgentsSince(context.Background(), st.DB, nil, later-86400)
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.Tokens != 0 || v.Totals.Threads != 0 || len(v.Models) != 0 || len(v.Counters["tool"]) != 0 {
		t.Errorf("thread of an out-of-period parent leaked: totals %+v, models %d, tools %v", v.Totals, len(v.Models), v.Counters["tool"])
	}
}
