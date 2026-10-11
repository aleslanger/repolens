package report

import (
	"context"
	"os"
	"testing"
	"time"

	"repolens/internal/store"
)

// TestStepTimings reports how long each agent-view step takes on the database in
// REPOLENS_BENCH_DB; it is skipped without it.
func TestStepTimings(t *testing.T) {
	path := os.Getenv("REPOLENS_BENCH_DB")
	if path == "" {
		t.Skip("set REPOLENS_BENCH_DB")
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sc := scopeFor(nil, 0)
	v := &AgentView{Counters: map[string][]CounterRow{}, Unconfigured: map[string][]CounterRow{}, OtherAgents: map[string]int{}}
	for name, f := range map[string]func() error{
		"totals":    func() error { return agentTotals(ctx, st.DB, sc, v) },
		"byTool":    func() error { return agentByTool(ctx, st.DB, sc, v) },
		"daily":     func() error { return agentDaily(ctx, st.DB, sc, v) },
		"models":    func() error { return agentModels(ctx, st.DB, sc, v) },
		"counters":  func() error { return agentCounters(ctx, st.DB, sc, v) },
		"trends":    func() error { return usageTrends(ctx, st.DB, sc, v) },
		"durations": func() error { return sessionDurations(ctx, st.DB, sc, v) },
		"sessions":  func() error { return sessionList(ctx, st.DB, sc, v) },
		"config":    func() error { return agentConfig(ctx, st.DB, nil, v) },
		"timeline":  func() error { return configTimeline(ctx, st.DB, nil, v) },
		"attrib":    func() error { return attribution(ctx, st.DB, nil, v) },
		"projects":  func() error { return agentProjects(ctx, st.DB, v) },
		"unmatched": func() error { return unmatched(ctx, st.DB, v) },
	} {
		start := time.Now()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%-10s %v", name, time.Since(start).Round(time.Millisecond))
	}
}
