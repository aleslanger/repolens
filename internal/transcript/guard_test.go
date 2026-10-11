package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeNameRejectsFreeText(t *testing.T) {
	ok := []string{"Bash", "mcp__plugin_playwright_playwright__browser_click", "ecc:security-reviewer", "claude-opus-5-5[1m]", "/claude-md-management:claude-md-improver", "hook_cancelled · PreToolUse:Bash", "gpt-5.5"}
	bad := []string{"SECRET=abc\nTOKEN=def", strings.Repeat("a", 101), "<script>alert(1)</script>", "rm -rf / && echo \"x\"", ""}
	for _, s := range ok {
		if SafeName(s) != s {
			t.Errorf("SafeName(%q) rejected a real name", s)
		}
	}
	for _, s := range bad {
		if SafeName(s) != invalidName {
			t.Errorf("SafeName(%q) = %q, want placeholder", s, SafeName(s))
		}
	}
}

func TestModelWrittenSkillNameIsNotStoredVerbatim(t *testing.T) {
	dir := t.TempDir()
	line := `{"type":"assistant","timestamp":"2026-10-01T10:00:00Z","requestId":"r","message":{"id":"m","model":"claude-x","content":[{"type":"tool_use","id":"t","name":"Skill","input":{"skill":"API_KEY=sk-live-123\nDB=postgres://u:p@h"}}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ParseClaudeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	for k := range res.Sessions[0].Counters {
		if strings.Contains(k.Name, "sk-live") || strings.Contains(k.Name, "postgres") {
			t.Fatalf("free text stored as a counter name: %q", k.Name)
		}
	}
	if res.Sessions[0].Counters[CounterKey{KindSkill, invalidName}] == nil {
		t.Error("invalid skill name not counted under the placeholder")
	}
}

func TestOversizedLineIsSkippedAndCounted(t *testing.T) {
	old := maxLine
	maxLine = 150 // the short record (~76 B) fits, the long one (~260 B) does not
	defer func() { maxLine = old }()
	dir := t.TempDir()
	long := `{"type":"user","timestamp":"2026-10-01T10:00:01Z","x":"` + strings.Repeat("y", 200) + `"}`
	short := `{"type":"system","subtype":"api_error","timestamp":"2026-10-01T10:00:02Z"}`
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(long+"\n"+short+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ParseClaudeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 2 || res.Issues[[2]string{IssueError, "řádek delší než limit"}] != 1 {
		t.Errorf("records=%d issues=%v", res.Records, res.Issues)
	}
	if res.Sessions[0].Counters[CounterKey{KindAPIError, "api_error"}] == nil {
		t.Error("the line after an oversized one was not parsed")
	}
}

func TestUsageIsMaxPerResponseRegardlessOfOrder(t *testing.T) {
	rec := func(out int) string {
		return `{"type":"assistant","timestamp":"2026-10-01T10:00:00Z","requestId":"r","message":{"id":"m","model":"claude-x","content":[],"usage":{"input_tokens":1,"output_tokens":` + itoa(out) + `}}}` + "\n"
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	// The final count arrives first here; a partial one follows. The response still counts once, with its maximum.
	if err := os.WriteFile(path, []byte(rec(700)+rec(3)), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ParseClaudeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Sessions[0].Models["claude-x"]; u == nil || u.Output != 700 || u.Requests != 1 {
		t.Errorf("usage = %+v, want output 700 in one request", u)
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// A forked subagent's transcript repeats the parent's records up to the spawn; records stamped
// exactly at the spawn are copies too and must not be counted twice.
func TestForkCopyAtSpawnTimeIsSkipped(t *testing.T) {
	dir := t.TempDir()
	const spawn = "2026-10-01T10:00:00.000Z"
	main := `{"type":"assistant","timestamp":"` + spawn + `","requestId":"r1","message":{"id":"m1","model":"claude-x","content":[{"type":"tool_use","id":"tu1","name":"Agent","input":{"subagent_type":"fork"}}],"usage":{"input_tokens":1,"output_tokens":1}}}
{"type":"attachment","timestamp":"` + spawn + `","attachment":{"type":"hook_success","hookEvent":"PreToolUse","hookName":"PreToolUse:Agent"}}
`
	sub := `{"type":"attachment","timestamp":"` + spawn + `","attachment":{"type":"hook_success","hookEvent":"PreToolUse","hookName":"PreToolUse:Agent"}}
{"type":"attachment","timestamp":"2026-10-01T10:00:05.000Z","attachment":{"type":"hook_success","hookEvent":"PreToolUse","hookName":"PreToolUse:Bash"}}
`
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(dir, "s", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-a.jsonl"), []byte(sub), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-a.meta.json"), []byte(`{"agentType":"fork","toolUseId":"tu1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ParseClaudeSession(filepath.Join(dir, "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if h := res.Sessions[0].Counters[CounterKey{KindHook, "PreToolUse"}]; h == nil || h.Count != 2 {
		t.Errorf("PreToolUse hooks = %+v, want 2 (parent's one + the subagent's own)", h)
	}
	if d := res.Sessions[0].Counters[CounterKey{KindSubagent, "fork"}].DurationMs; d != 5000 {
		t.Errorf("fork duration = %d ms, want 5000", d)
	}
}
