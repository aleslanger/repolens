package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"repolens/internal/privacy"
)

const toolClaude = "claude"

// Record types and subtypes observed in Claude Code 2.1.x transcripts on this machine.
// Anything else is counted as unrecognized rather than silently dropped.
var (
	claudeTypes = set("attachment", "assistant", "user", "atis-latch", "last-prompt", "ai-title", "mode",
		"permission-mode", "bridge-session", "system", "queue-operation", "pr-link", "file-history-delta",
		"file-history-snapshot", "cost-state", "frame-link", "artifact-autoreact-ledger", "agent-setting",
		"agent-name", "relocated", "worktree-state", "artifact-comment-monitor", "fork-context-ref",
		"continued-in", "summary", "custom-title", "api-request", "api-request-blob", "api-request-shape")
	claudeSystemSubtypes = set("stop_hook_summary", "turn_duration", "away_summary", "local_command",
		"scheduled_task_fire", "compact_boundary", "api_error", "informational", "bridge_status")
	claudeAttachmentTypes = set("hook_success", "async_hook_response", "total_tokens_reminder",
		"deferred_tools_record", "prompt_snapshot", "environment", "deferred_tools_delta", "date",
		"nested_memory", "skill_listing", "model", "session_context", "agent_listing_delta",
		"hook_additional_context", "instructions", "diagnostics", "edited_text_file", "remote_session_change",
		"queued_command", "mcp_instructions_delta", "credential_org", "structured_output", "language",
		"silent_turn_reminder", "auto_mode", "task_reminder", "batching_reminder_sent", "file",
		"bash_output_audience_note", "compact_file_reference", "command_permissions", "hook_blocking_error",
		"hook_cancelled", "max_turns_reached", "read_truncation_notice", "thinking_drop", "task_status",
		"plan_mode", "plan_mode_exit", "invoked_skills", "hook_non_blocking_error", "directory",
		"unknown_command_fallback", "skill_mention")
	hookFailureTypes = set("hook_blocking_error", "hook_non_blocking_error", "hook_cancelled")
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

var slashCommand = regexp.MustCompile(`<command-name>(/?[\w:.-]+)</command-name>`)

// claudeRecord declares only the fields repolens aggregates; everything else is ignored by the decoder.
type claudeRecord struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	Timestamp      string `json:"timestamp"`
	CWD            string `json:"cwd"`
	RequestID      string `json:"requestId"`
	DurationMs     int64  `json:"durationMs"`
	ToolDenialKind string `json:"toolDenialKind"`
	Message        *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			Details    *struct {
				Thinking int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"message"`
	Attachment *struct {
		Type      string `json:"type"`
		HookEvent string `json:"hookEvent"`
		HookName  string `json:"hookName"`
	} `json:"attachment"`
	ModelUsage map[string]struct {
		CostUSD float64 `json:"costUSD"`
	} `json:"modelUsage"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Text      string          `json:"text"`
	Input     json.RawMessage `json:"input"`
}

type toolInput struct {
	SubagentType string `json:"subagent_type"`
	Skill        string `json:"skill"`
}

type subagentMeta struct {
	AgentType string `json:"agentType"`
	ToolUseID string `json:"toolUseId"`
}

// claudeParser accumulates one session: the main file plus its subagent transcripts.
type claudeParser struct {
	s        *Session
	issues   Issues
	records  int
	usage    map[string]*msgUsage // per API response: the largest values seen across its records
	anon     int                  // counter for responses without ids
	toolName map[string]string    // tool_use id -> tool name, to attribute errors
	toolTime map[string]time.Time // tool_use id -> when it was issued, to find subagent spawns
}

// ParseClaudeSession parses <dir>/<session>.jsonl and <dir>/<session>/subagents/*.
func ParseClaudeSession(path string) (FileResult, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	p := &claudeParser{s: newSession(toolClaude, id), issues: Issues{}, usage: map[string]*msgUsage{}, toolName: map[string]string{}, toolTime: map[string]time.Time{}}
	if err := eachLine(path, func(b []byte) { p.line(b, false, time.Time{}) }, p.oversize); err != nil {
		return FileResult{}, err
	}
	if err := p.subagents(strings.TrimSuffix(path, ".jsonl") + string(filepath.Separator) + "subagents"); err != nil {
		return FileResult{}, err
	}
	p.flushUsage()
	p.s.finish()
	if p.s.Started.IsZero() {
		return FileResult{Records: p.records, Issues: p.issues}, nil // no timestamped records: nothing to report
	}
	return FileResult{Sessions: []*Session{p.s}, Records: p.records, Issues: p.issues}, nil
}

func (p *claudeParser) subagents(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read subagents %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		var meta subagentMeta
		metaPath := filepath.Join(dir, strings.TrimSuffix(name, ".jsonl")+".meta.json")
		b, err := privacy.ReadFile(metaPath, 1<<16)
		switch {
		case err == nil:
			if json.Unmarshal(b, &meta) != nil {
				p.issues.add(IssueError, "subagent meta: neplatný JSON")
			}
		case !errors.Is(err, os.ErrNotExist):
			p.issues.add(IssueError, "subagent meta: nelze číst")
		}
		// Forked subagents start with a copy of the parent's context; only records from the
		// spawn onwards belong to the subagent, otherwise tools and tokens count twice.
		spawn := p.toolTime[meta.ToolUseID]
		first, last := spawn, time.Time{}
		err = eachLine(filepath.Join(dir, name), func(b []byte) {
			if t := p.line(b, true, spawn); !t.IsZero() {
				if first.IsZero() || t.Before(first) {
					first = t
				}
				if t.After(last) {
					last = t
				}
			}
		}, p.oversize)
		if err != nil {
			return err
		}
		agentType := meta.AgentType
		if agentType == "" {
			agentType = "(neznámý)"
		}
		c := p.s.count(KindSubagent, agentType, 0)
		if !first.IsZero() {
			c.DurationMs += last.Sub(first).Milliseconds()
		}
	}
	return nil
}

func (p *claudeParser) oversize() {
	p.records++
	p.issues.add(IssueError, "řádek delší než limit")
}

// line handles one JSONL record and returns its timestamp (zero when absent or skipped).
// Records timestamped at or before notBefore are copies of earlier context and are skipped.
func (p *claudeParser) line(b []byte, sub bool, notBefore time.Time) time.Time {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return time.Time{}
	}
	p.records++
	var r claudeRecord
	if err := json.Unmarshal(b, &r); err != nil {
		p.issues.add(IssueError, "neplatný JSON řádek")
		return time.Time{}
	}
	ts := parseTime(r.Timestamp)
	if !ts.IsZero() && !notBefore.IsZero() && !ts.After(notBefore) {
		return time.Time{}
	}
	p.s.seen(ts, r.CWD)
	switch {
	case !claudeTypes[r.Type]:
		p.issues.add(IssueUnrecognized, "type:"+r.Type)
	case r.Type == "assistant":
		p.assistant(&r, ts)
	case r.Type == "user":
		p.user(&r, sub)
	case r.Type == "system":
		p.system(&r, sub)
	case r.Type == "attachment":
		p.attachment(&r)
	case r.Type == "cost-state":
		// Cumulative snapshots: the last one wins. Variants such as "claude-opus-5-5[1m]" are
		// folded into the base model, under which transcript usage is recorded.
		snapshot := map[string]float64{}
		for model, u := range r.ModelUsage {
			base, _, _ := strings.Cut(model, "[")
			snapshot[base] += u.CostUSD
		}
		maps.Copy(p.s.ReportedCost, snapshot)
	}
	return ts
}

func (p *claudeParser) assistant(r *claudeRecord, ts time.Time) {
	m := r.Message
	if m == nil {
		p.issues.add(IssueError, "assistant bez message")
		return
	}
	// One API response is split into several records that repeat its usage; streamed records
	// can carry partial output counts, so the maximum per response is kept.
	if m.Usage != nil && m.Model != "<synthetic>" {
		key := m.ID + "|" + r.RequestID
		if m.ID == "" && r.RequestID == "" {
			p.anon++
			key = fmt.Sprintf("anon-%d", p.anon)
		}
		mu := p.usage[key]
		if mu == nil {
			mu = &msgUsage{model: m.Model}
			p.usage[key] = mu
		}
		thinking := int64(0)
		if m.Usage.Details != nil {
			thinking = m.Usage.Details.Thinking
		}
		mu.raise(transcriptUsage{m.Usage.Input, m.Usage.Output, m.Usage.CacheRead, m.Usage.CacheWrite, thinking})
	}
	var blocks []contentBlock
	if len(m.Content) == 0 || m.Content[0] != '[' {
		return
	}
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		p.issues.add(IssueError, "assistant content: neplatná struktura")
		return
	}
	for _, bl := range blocks {
		// A tool call is counted once even if a forked transcript repeats it.
		if _, dup := p.toolName[bl.ID]; bl.Type == "tool_use" && !dup {
			p.toolTime[bl.ID] = ts
			p.toolUse(bl)
		}
	}
}

func (p *claudeParser) toolUse(bl contentBlock) {
	p.toolName[bl.ID] = bl.Name
	p.s.count(KindTool, bl.Name, 1)
	if server, ok := mcpServer(bl.Name); ok {
		p.s.count(KindMCP, server, 1)
	}
	if bl.Name != "Agent" && bl.Name != "Task" && bl.Name != "Skill" {
		return
	}
	var in toolInput
	if len(bl.Input) > 0 && json.Unmarshal(bl.Input, &in) != nil {
		p.issues.add(IssueError, bl.Name+": neplatný input")
		return
	}
	switch bl.Name {
	case "Skill":
		if in.Skill != "" {
			p.s.count(KindSkill, in.Skill, 1)
		}
	default:
		t := in.SubagentType
		if t == "" {
			t = "general-purpose" // older versions omit the type for the default agent
		}
		p.s.count(KindSubagent, t, 1)
	}
}

type transcriptUsage struct{ input, output, cacheRead, cacheWrite, reasoning int64 }

type msgUsage struct {
	model string
	max   transcriptUsage
}

func (m *msgUsage) raise(u transcriptUsage) {
	m.max.input = max(m.max.input, u.input)
	m.max.output = max(m.max.output, u.output)
	m.max.cacheRead = max(m.max.cacheRead, u.cacheRead)
	m.max.cacheWrite = max(m.max.cacheWrite, u.cacheWrite)
	m.max.reasoning = max(m.max.reasoning, u.reasoning)
}

// flushUsage adds one request per API response to the session's model totals.
func (p *claudeParser) flushUsage() {
	for _, mu := range p.usage {
		u := p.s.model(mu.model)
		u.Requests++
		u.Input += mu.max.input
		u.Output += mu.max.output
		u.CacheRead += mu.max.cacheRead
		u.CacheWrite += mu.max.cacheWrite
		u.Reasoning += mu.max.reasoning
	}
}

// mcpServer extracts the server from mcp__<server>__<tool>.
func mcpServer(tool string) (string, bool) {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return "", false
	}
	server, _, ok := strings.Cut(rest, "__")
	return server, ok && server != ""
}

func (p *claudeParser) user(r *claudeRecord, sub bool) {
	if r.ToolDenialKind != "" {
		p.s.count(KindDenial, r.ToolDenialKind, 1)
	}
	if r.Message == nil || len(r.Message.Content) == 0 {
		return
	}
	c := r.Message.Content
	if c[0] == '"' {
		var text string
		if json.Unmarshal(c, &text) == nil && !sub {
			p.slash(text)
		}
		return
	}
	var blocks []contentBlock
	if err := json.Unmarshal(c, &blocks); err != nil {
		p.issues.add(IssueError, "user content: neplatná struktura")
		return
	}
	for _, bl := range blocks {
		switch {
		case bl.Type == "text" && !sub:
			p.slash(bl.Text)
		case bl.Type == "tool_result" && bl.IsError:
			name := p.toolName[bl.ToolUseID]
			if name == "" {
				name = "(neznámý)"
			}
			p.s.count(KindToolError, name, 1)
			if c := p.s.Counters[CounterKey{KindTool, name}]; c != nil {
				c.Errors++
			}
		}
	}
}

// slash records the command names in a user prompt; the prompt text itself is discarded.
func (p *claudeParser) slash(text string) {
	if !strings.Contains(text, "<command-name>") {
		return
	}
	for _, m := range slashCommand.FindAllStringSubmatch(text, -1) {
		p.s.count(KindSlash, m[1], 1)
	}
}

func (p *claudeParser) system(r *claudeRecord, sub bool) {
	if !claudeSystemSubtypes[r.Subtype] {
		p.issues.add(IssueUnrecognized, "system:"+r.Subtype)
		return
	}
	switch r.Subtype {
	case "turn_duration":
		if !sub {
			p.s.Turns++
			p.s.turnMs += r.DurationMs
			p.s.turnsKnown = true
		}
	case "api_error":
		p.s.count(KindAPIError, "api_error", 1)
	}
}

func (p *claudeParser) attachment(r *claudeRecord) {
	a := r.Attachment
	if a == nil {
		p.issues.add(IssueError, "attachment bez obsahu")
		return
	}
	if !claudeAttachmentTypes[a.Type] {
		p.issues.add(IssueUnrecognized, "attachment:"+a.Type)
		return
	}
	if a.HookEvent != "" {
		c := p.s.count(KindHook, a.HookEvent, 1)
		if hookFailureTypes[a.Type] {
			c.Errors++
		}
	}
	if hookFailureTypes[a.Type] {
		name := a.HookName
		if name == "" {
			name = a.HookEvent
		}
		p.s.count(KindHookBlock, a.Type+" · "+name, 1)
	}
}
