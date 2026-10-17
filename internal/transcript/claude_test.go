package transcript

import (
	"reflect"
	"testing"
	"time"
)

// counters returns name->count for one kind.
func counters(s *Session, kind string) map[string]int {
	out := map[string]int{}
	for k, c := range s.Counters {
		if k.Kind == kind && c.Count > 0 {
			out[k.Name] = c.Count
		}
	}
	return out
}

func parseClaude(t *testing.T, name string) (*Session, FileResult) {
	t.Helper()
	res, err := ParseClaudeSession("testdata/claude/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(res.Sessions))
	}
	return res.Sessions[0], res
}

// Expected values were computed independently from the fixture with a Python script.
func TestClaudeRealSessionAggregates(t *testing.T) {
	s, res := parseClaude(t, "session.jsonl")

	if res.Issues.Count(IssueError) != 0 || res.Issues.Count(IssueUnrecognized) != 0 {
		t.Errorf("issues on a real session: %v", res.Issues)
	}
	if s.ID != "session" || s.Tool != "claude" || s.CWD != "/home/user/.herdr/worktrees/demo/feat-x" {
		t.Errorf("id=%q tool=%q cwd=%q", s.ID, s.Tool, s.CWD)
	}
	if got := s.Started.UTC().Format(time.RFC3339Nano); got != "2026-09-26T13:49:46.497Z" {
		t.Errorf("started = %s", got)
	}
	if got := s.Ended.UTC().Format(time.RFC3339Nano); got != "2026-10-07T13:27:42.47Z" {
		t.Errorf("ended = %s", got)
	}
	if s.Turns != 7 || s.ActiveMs != 2068298 {
		t.Errorf("turns=%d active=%d, want 7 and 2068298", s.Turns, s.ActiveMs)
	}

	// Subagent transcripts stream a response as several records whose first output count is
	// partial (e.g. 2, then 136); the expectation takes the maximum per response.
	wantModels := map[string]ModelUsage{
		"claude-sonnet-5": {Requests: 171, Input: 342, Output: 57110, CacheRead: 31986474, CacheWrite: 245163, Reasoning: 18935},
		"claude-opus-5-5": {Requests: 1, Input: 2, Output: 74, CacheRead: 113609, CacheWrite: 4402},
	}
	if len(s.Models) != len(wantModels) {
		t.Errorf("models = %v", s.Models)
	}
	for name, want := range wantModels {
		if got := s.Models[name]; got == nil || *got != want {
			t.Errorf("model %s = %+v, want %+v", name, got, want)
		}
	}

	wantTools := map[string]int{"Bash": 80, "ToolSearch": 3, "EnterWorktree": 1, "Read": 32, "Agent": 1, "ListAgents": 1,
		"SendMessage": 1, "mcp__plugin_playwright_playwright__browser_navigate": 1, "Skill": 2, "Edit": 40, "Monitor": 1}
	checks := []struct {
		kind string
		want map[string]int
	}{
		{KindTool, wantTools},
		{KindSubagent, map[string]int{"fork": 1}},
		{KindToolError, map[string]int{"EnterWorktree": 1, "Bash": 1}},
		{KindDenial, map[string]int{"user-rejected": 1}},
		{KindHook, map[string]int{"SessionStart": 3, "Stop": 12, "SessionEnd": 1, "PreToolUse": 635, "PostToolUse": 1}},
		{KindHookBlock, map[string]int{"hook_blocking_error · PostToolUse:Edit": 1}},
		{KindSkill, map[string]int{"verify": 1, "herdr": 1}},
		{KindMCP, map[string]int{"plugin_playwright_playwright": 1}},
		{KindSlash, map[string]int{"/clear": 1}},
	}
	for _, c := range checks {
		if got := counters(s, c.kind); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %v, want %v", c.kind, got, c.want)
		}
	}
	if d := s.Counters[CounterKey{KindSubagent, "fork"}].DurationMs; d != 1386367 {
		t.Errorf("fork duration = %d ms, want 1386367", d)
	}
	if e := s.Counters[CounterKey{KindTool, "Bash"}].Errors; e != 1 {
		t.Errorf("Bash errors = %d, want 1", e)
	}
	wantCost := map[string]float64{"claude-haiku-4-5-20251001": 0.001847, "claude-sonnet-5": 9.562510300000005}
	if !reflect.DeepEqual(s.ReportedCost, wantCost) {
		t.Errorf("reported cost = %v", s.ReportedCost)
	}
}

func TestClaudeEdgeCases(t *testing.T) {
	s, res := parseClaude(t, "edge.jsonl")

	if res.Records != 12 {
		t.Errorf("records = %d, want 12", res.Records)
	}
	wantIssues := Issues{
		{IssueError, "neplatný JSON řádek"}:                    1,
		{IssueError, "assistant bez message"}:                  1,
		{IssueUnrecognized, "type:brand-new-record"}:           1,
		{IssueUnrecognized, "system:brand_new_subtype"}:        1,
		{IssueUnrecognized, "attachment:brand_new_attachment"}: 1,
	}
	if !reflect.DeepEqual(res.Issues, wantIssues) {
		t.Errorf("issues = %v", res.Issues)
	}
	// Duplicate message records count once; <synthetic> is not a model.
	want := ModelUsage{Requests: 1, Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40, Reasoning: 5}
	if len(s.Models) != 1 || s.Models["claude-opus-5-5"] == nil || *s.Models["claude-opus-5-5"] != want {
		t.Errorf("models = %v", s.Models)
	}
	// Slash commands come from prompts only, never from tool output.
	if got := counters(s, KindSlash); !reflect.DeepEqual(got, map[string]int{"/model": 1}) {
		t.Errorf("slash = %v", got)
	}
	if got := counters(s, KindSubagent); !reflect.DeepEqual(got, map[string]int{"general-purpose": 1}) {
		t.Errorf("subagents = %v", got)
	}
	if got := counters(s, KindToolError); !reflect.DeepEqual(got, map[string]int{"Agent": 1}) {
		t.Errorf("tool errors = %v", got)
	}
	if got := counters(s, KindDenial); !reflect.DeepEqual(got, map[string]int{"permission-rule": 1}) {
		t.Errorf("denials = %v", got)
	}
	if h := s.Counters[CounterKey{KindHook, "PreToolUse"}]; h == nil || h.Count != 1 || h.Errors != 1 {
		t.Errorf("hook = %+v", h)
	}
	if got := counters(s, KindAPIError); got["api_error"] != 1 {
		t.Errorf("api errors = %v", got)
	}
	// No turn durations: active time sums gaps under 5 minutes (12 s), skipping the 20-minute pause.
	if s.ActiveMs != 12000 || s.Turns != 0 {
		t.Errorf("active=%d turns=%d, want 12000 and 0", s.ActiveMs, s.Turns)
	}
	// The 20-minute pause splits activity into two windows; the corrupted 10:20:00 line has no
	// timestamp, so the second window is the single 10:20:01 record.
	if len(s.Windows) != 2 || s.Windows[0].End.Sub(s.Windows[0].Start) != 12*time.Second || !s.Windows[1].Start.Equal(s.Windows[1].End) {
		t.Errorf("windows = %+v", s.Windows)
	}
}

func TestClaudeRefusesForbiddenPath(t *testing.T) {
	if _, err := ParseClaudeSession("/tmp/.env.jsonl"); err == nil {
		t.Fatal("expected privacy refusal")
	}
}

// Claude Code 2.1.300+ logs every API request (params, prompt blobs, prompt shape); repolens
// takes tokens from assistant records, so these are known and carry nothing to aggregate.
func TestClaudeRequestLogRecordsAreKnown(t *testing.T) {
	res, err := ParseClaudeSession("testdata/claude/requests.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if res.Issues.Count(IssueError) != 0 || res.Issues.Count(IssueUnrecognized) != 0 {
		t.Errorf("issues on request log records: %v", res.Issues)
	}
}
