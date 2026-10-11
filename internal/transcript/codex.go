package transcript

import (
	"bytes"
	"encoding/json"
	"strings"
)

const toolCodex = "codex"

// Record types observed in Codex rollouts on this machine (CLI 0.1xx, June–October 2026).
var (
	codexTypes = set("session_meta", "turn_context", "response_item", "event_msg", "token_usage_record",
		"world_state", "compacted", "inter_agent_communication_metadata")
	codexItemTypes = set("message", "reasoning", "function_call", "function_call_output", "custom_tool_call",
		"custom_tool_call_output", "web_search_call", "image_generation_call", "local_shell_call", "tool_search_call",
		"tool_search_output", "agent_message")
	codexEventTypes = set("item_completed", "token_count", "agent_message", "task_started", "task_complete",
		"thread_settings_applied", "patch_apply_end", "user_message", "web_search_end", "turn_aborted",
		"mcp_tool_call_end", "image_generation_end", "context_compacted", "entered_review_mode", "exited_review_mode",
		"agent_reasoning", "exec_command_begin", "exec_command_end", "patch_apply_begin", "mcp_tool_call_begin",
		"error", "stream_error", "background_event", "turn_diff", "plan_update", "thread_rolled_back")
)

type codexRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload is the union of payload fields repolens reads; free text is never declared.
type codexPayload struct {
	Type           string          `json:"type"`
	ID             string          `json:"id"`
	CWD            string          `json:"cwd"`
	Model          string          `json:"model"`
	Name           string          `json:"name"`
	Reason         string          `json:"reason"`
	DurationMs     int64           `json:"duration_ms"`
	Success        *bool           `json:"success"`
	ParentThreadID string          `json:"parent_thread_id"`
	Source         json.RawMessage `json:"source"`
	Info           *struct {
		Total codexTokens `json:"total_token_usage"`
	} `json:"info"`
	Item *struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	} `json:"item"`
	Invocation *struct {
		Server string `json:"server"`
	} `json:"invocation"`
}

type codexTokens struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
}

// delta returns the growth from o to t. A drop in input or output means the counter was reset
// (a new thread after compaction or rollback); drops in secondary components (cached, reasoning,
// cache writes) are recomputations and count as zero, so nothing is counted twice.
func (t codexTokens) delta(o codexTokens) (d codexTokens, reset bool) {
	if t.Input < o.Input || t.Output < o.Output {
		return t, true
	}
	return codexTokens{t.Input - o.Input, max(0, t.Cached-o.Cached), t.Output - o.Output,
		max(0, t.Reasoning-o.Reasoning), max(0, t.CacheWrite-o.CacheWrite)}, false
}

type codexParser struct {
	s       *Session
	issues  Issues
	records int
	model   string
	last    codexTokens
}

// ParseCodexRollout parses one ~/.codex/sessions rollout file.
func ParseCodexRollout(path string) (FileResult, error) {
	p := &codexParser{s: newSession(toolCodex, ""), issues: Issues{}}
	oversize := func() {
		p.records++
		p.issues.add(IssueError, "řádek delší než limit")
	}
	if err := eachLine(path, p.line, oversize); err != nil {
		return FileResult{}, err
	}
	p.s.finish()
	if p.s.ID == "" || p.s.Started.IsZero() {
		p.issues.add(IssueError, "rollout bez session_meta")
		return FileResult{Records: p.records, Issues: p.issues}, nil
	}
	return FileResult{Sessions: []*Session{p.s}, Records: p.records, Issues: p.issues}, nil
}

func (p *codexParser) line(b []byte) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return
	}
	p.records++
	var r codexRecord
	if err := json.Unmarshal(b, &r); err != nil {
		p.issues.add(IssueError, "neplatný JSON řádek")
		return
	}
	p.s.seen(parseTime(r.Timestamp), "")
	if !codexTypes[r.Type] {
		p.issues.add(IssueUnrecognized, "type:"+r.Type)
		return
	}
	var pl codexPayload
	if len(r.Payload) > 0 && r.Payload[0] == '{' {
		if err := json.Unmarshal(r.Payload, &pl); err != nil {
			p.issues.add(IssueError, r.Type+": neplatný payload")
			return
		}
	}
	switch r.Type {
	case "session_meta":
		p.meta(&pl)
	case "turn_context":
		if pl.Model != "" {
			p.model = pl.Model
		}
		p.s.seen(parseTime(r.Timestamp), pl.CWD)
	case "response_item":
		p.item(&pl)
	case "event_msg":
		p.event(&pl)
	}
}

func (p *codexParser) meta(pl *codexPayload) {
	if p.s.ID == "" {
		p.s.ID = pl.ID
	}
	p.s.seen(p.s.Started, pl.CWD)
	if pl.ParentThreadID != "" {
		p.s.Parent = pl.ParentThreadID
	} else if bytes.Contains(pl.Source, []byte(`"subagent"`)) {
		p.s.Parent = "(neznámý)"
	}
}

func (p *codexParser) item(pl *codexPayload) {
	if !codexItemTypes[pl.Type] {
		p.issues.add(IssueUnrecognized, "item:"+pl.Type)
		return
	}
	switch pl.Type {
	case "function_call", "custom_tool_call":
		p.s.count(KindTool, pl.Name, 1)
		if pl.Name == "spawn_agent" {
			p.s.count(KindSubagent, "codex-subagent", 1)
		}
	case "web_search_call":
		p.s.count(KindTool, "web_search", 1)
	case "image_generation_call":
		p.s.count(KindTool, "image_generation", 1)
	case "local_shell_call":
		p.s.count(KindTool, "local_shell", 1)
	}
}

func (p *codexParser) event(pl *codexPayload) {
	if !codexEventTypes[pl.Type] {
		p.issues.add(IssueUnrecognized, "event:"+pl.Type)
		return
	}
	switch pl.Type {
	case "token_count":
		p.tokens(pl)
	case "task_complete":
		p.s.Turns++
		p.s.turnMs += pl.DurationMs
		p.s.turnsKnown = true
	case "turn_aborted":
		p.s.count(KindDenial, pl.Reason, 1)
		p.s.turnMs += pl.DurationMs
		p.s.turnsKnown = true
	case "item_completed":
		if pl.Item != nil && pl.Item.Type == "CommandExecution" && pl.Item.Status == "failed" {
			p.toolError("exec_command")
		}
	case "patch_apply_end":
		if pl.Success != nil && !*pl.Success {
			p.toolError("apply_patch")
		}
	case "mcp_tool_call_end":
		if pl.Invocation != nil && pl.Invocation.Server != "" {
			p.s.count(KindMCP, pl.Invocation.Server, 1)
		}
	case "error", "stream_error":
		p.s.count(KindAPIError, pl.Type, 1)
	}
}

func (p *codexParser) toolError(name string) {
	p.s.count(KindToolError, name, 1)
	if c := p.s.Counters[CounterKey{KindTool, name}]; c != nil {
		c.Errors++
	}
}

// tokens attributes the growth of the cumulative counter to the current model.
// OpenAI reports cached tokens inside input_tokens, so they are split out here.
func (p *codexParser) tokens(pl *codexPayload) {
	if pl.Info == nil {
		return
	}
	if pl.Info.Total == p.last {
		return // repeated snapshot
	}
	d, _ := pl.Info.Total.delta(p.last)
	p.last = pl.Info.Total
	if d == (codexTokens{}) {
		return
	}
	// Cached tokens are part of input in OpenAI's accounting; never subtract more than there is.
	d.Cached = min(d.Cached, d.Input)
	model := p.model
	if model == "" {
		model = "(neznámý)"
		p.issues.add(IssueError, "tokeny před turn_context")
	}
	u := p.s.model(model)
	u.Requests++
	u.Input += d.Input - d.Cached
	u.CacheRead += d.Cached
	u.CacheWrite += d.CacheWrite
	u.Output += d.Output
	u.Reasoning += d.Reasoning
}

// IsCodexRollout reports whether a file name looks like a Codex rollout.
func IsCodexRollout(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
}
