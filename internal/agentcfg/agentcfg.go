// Package agentcfg inventories coding-agent configuration: instructions, agents, skills,
// commands, rules, hooks, permissions and MCP servers. Only names and paths are kept.
package agentcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"repolens/internal/privacy"
)

// Scopes.
const (
	ScopeProject = "project"
	ScopeGlobal  = "global"
	ScopePlugin  = "plugin"
)

// Kinds.
const (
	KindInstructions = "instructions"
	KindAgent        = "agent"
	KindSkill        = "skill"
	KindCommand      = "command"
	KindRule         = "rule"
	KindHook         = "hook"
	KindPermission   = "permission"
	KindMCP          = "mcp"
	KindSettings     = "settings"
	KindPlugin       = "plugin"
)

const maxConfigFile = 1 << 20

type Item struct {
	Scope, Tool, Kind, Name string
	Path                    string // relative to the repo for project items, absolute otherwise
	Detail                  string
	Present                 bool
	First, Last             time.Time
	Changes                 int
}

// Problem is a config file that could not be read or parsed; it is reported, not fatal.
type Problem struct{ Path, Err string }

// classify maps a repo-relative path to (tool, kind, name). ok=false for unrelated files.
func classify(rel string) (tool, kind, name string, ok bool) {
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	switch {
	case rel == "CLAUDE.md" || rel == ".claude/CLAUDE.md" || rel == "CLAUDE.local.md":
		return "claude", KindInstructions, rel, true
	case rel == "AGENTS.md":
		return "generic", KindInstructions, rel, true
	case rel == "GEMINI.md":
		return "gemini", KindInstructions, rel, true
	case rel == ".github/copilot-instructions.md" || strings.HasPrefix(rel, ".github/instructions/"):
		return "copilot", KindInstructions, rel, true
	case rel == ".cursorrules":
		return "cursor", KindRule, rel, true
	case strings.HasPrefix(rel, ".cursor/rules/"):
		return "cursor", KindRule, stem, true
	case strings.HasPrefix(rel, ".claude/agents/") && strings.HasSuffix(rel, ".md"):
		return "claude", KindAgent, stem, true
	case strings.HasPrefix(rel, ".claude/skills/") && base == "SKILL.md":
		return "claude", KindSkill, filepath.Base(filepath.Dir(rel)), true
	case strings.HasPrefix(rel, ".claude/commands/") && strings.HasSuffix(rel, ".md"):
		return "claude", KindCommand, stem, true
	case strings.HasPrefix(rel, ".claude/rules/") && strings.HasSuffix(rel, ".md"):
		return "claude", KindRule, stem, true
	case rel == ".claude/settings.json" || rel == ".claude/settings.local.json":
		return "claude", KindSettings, base, true
	case rel == ".mcp.json":
		return "mcp", KindSettings, base, true
	case strings.HasPrefix(rel, ".codex/"):
		return "codex", KindSettings, strings.TrimPrefix(rel, ".codex/"), true
	}
	return "", "", "", false
}

// projectRoots are the paths inside a repository that hold agent configuration.
var projectRoots = []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", "GEMINI.md", ".claude", ".mcp.json",
	".codex", ".github/copilot-instructions.md", ".github/instructions", ".cursor", ".cursorrules"}

// Project inventories a repository's agent configuration from disk; history is added by Annotate.
func Project(repoDir string) ([]Item, []Problem) {
	var items []Item
	var problems []Problem
	for _, root := range projectRoots {
		abs := filepath.Join(repoDir, root)
		err := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					problems = append(problems, Problem{path, err.Error()})
				}
				return nil
			}
			if d.IsDir() {
				if privacy.Forbidden(path) || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(repoDir, path)
			if err != nil {
				return nil
			}
			found, probs := fileItems(ScopeProject, rel, path)
			items = append(items, found...)
			problems = append(problems, probs...)
			return nil
		})
		if err != nil {
			problems = append(problems, Problem{abs, err.Error()})
		}
	}
	return items, problems
}

// fileItems turns one config file into items; settings files expand into permissions, hooks and MCP servers.
func fileItems(scope, rel, abs string) ([]Item, []Problem) {
	tool, kind, name, ok := classify(rel)
	if !ok || privacy.Forbidden(abs) {
		return nil, nil
	}
	item := Item{Scope: scope, Tool: tool, Kind: kind, Name: name, Path: rel, Present: true}
	switch {
	case kind == KindAgent || kind == KindSkill || kind == KindCommand:
		if fm := frontmatterName(abs); fm != "" {
			item.Name = fm
		}
	case rel == ".mcp.json" || strings.HasSuffix(rel, "settings.json") || strings.HasSuffix(rel, "settings.local.json"):
		expanded, err := settingsItems(scope, tool, rel, abs)
		if err != nil {
			return []Item{item}, []Problem{{abs, err.Error()}}
		}
		return append([]Item{item}, expanded...), nil
	}
	return []Item{item}, nil
}

var fmName = regexp.MustCompile(`(?m)^name:\s*["']?([^"'\n]+?)["']?\s*$`)

// frontmatterName reads `name:` from YAML front matter, if present.
func frontmatterName(path string) string {
	const head = 4096
	b, err := privacy.ReadFile(path, head)
	if err != nil || !bytes.HasPrefix(b, []byte("---")) {
		return ""
	}
	end := bytes.Index(b[3:], []byte("\n---"))
	if end < 0 {
		return ""
	}
	m := fmName.FindSubmatch(b[3 : 3+end])
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

type settingsFile struct {
	Permissions struct {
		Allow []string `json:"allow"`
		Ask   []string `json:"ask"`
		Deny  []string `json:"deny"`
	} `json:"permissions"`
	Hooks map[string][]struct {
		Matcher *string `json:"matcher"`
	} `json:"hooks"`
	MCPServers     map[string]json.RawMessage `json:"mcpServers"`
	EnabledPlugins map[string]bool            `json:"enabledPlugins"`
}

func readSettings(abs string) (settingsFile, error) {
	var s settingsFile
	b, err := privacy.ReadFile(abs, maxConfigFile)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, jsonProblem(err)
	}
	return s, nil
}

// jsonProblem describes a parse error by position only; Go's messages quote the offending
// character, which would leak file content (e.g. through a symlink to a secret).
func jsonProblem(err error) error {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return fmt.Errorf("neplatný JSON (bajt %d)", syn.Offset)
	}
	var typ *json.UnmarshalTypeError
	if errors.As(err, &typ) {
		return fmt.Errorf("neočekávaná struktura JSON v poli %q", typ.Field)
	}
	return errors.New("neplatný JSON")
}

// settingsItems lists permission rules, hook registrations (event + matcher only, never the
// command) and MCP server names (never their env or args).
func settingsItems(scope, tool, rel, abs string) ([]Item, error) {
	s, err := readSettings(abs)
	if err != nil {
		return nil, err
	}
	var out []Item
	seen := map[string]int{}
	add := func(kind, name, detail string) {
		// Redaction can map different rules to the same text; number the repeats so each stays
		// a separate item instead of collapsing into one.
		key := kind + "\x00" + name + "\x00" + detail
		if seen[key]++; seen[key] > 1 {
			name = fmt.Sprintf("%s (#%d)", name, seen[key])
		}
		out = append(out, Item{Scope: scope, Tool: tool, Kind: kind, Name: name, Detail: detail, Path: rel, Present: true})
	}
	for _, r := range s.Permissions.Allow {
		add(KindPermission, RedactRule(r), "allow")
	}
	for _, r := range s.Permissions.Ask {
		add(KindPermission, RedactRule(r), "ask")
	}
	for _, r := range s.Permissions.Deny {
		add(KindPermission, RedactRule(r), "deny")
	}
	for event, entries := range s.Hooks {
		for _, e := range entries {
			m := "*"
			if e.Matcher != nil && *e.Matcher != "" {
				m = RedactRule(*e.Matcher)
			}
			add(KindHook, event, m)
		}
	}
	for name := range s.MCPServers {
		add(KindMCP, name, "")
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Kind+out[i].Name+out[i].Detail < out[j].Kind+out[j].Name+out[j].Detail
	})
	return out, nil
}

// Annotate adds git history (first/last commit, change count) to project items and appends
// items that only exist in history.
func Annotate(items []Item, hist map[string]History) []Item {
	byPath := map[string]bool{}
	for i := range items {
		byPath[items[i].Path] = true
		if h, ok := hist[filepath.ToSlash(items[i].Path)]; ok {
			items[i].First, items[i].Last, items[i].Changes = h.First, h.Last, h.Changes
		}
	}
	paths := make([]string, 0, len(hist))
	for p := range hist {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if byPath[p] {
			continue
		}
		tool, kind, name, ok := classify(p)
		if !ok {
			continue
		}
		h := hist[p]
		items = append(items, Item{Scope: ScopeProject, Tool: tool, Kind: kind, Name: name, Path: p,
			Present: false, First: h.First, Last: h.Last, Changes: h.Changes})
	}
	return items
}

// fileExists reports whether a regular file exists (errors other than not-exist count as absent too).
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
