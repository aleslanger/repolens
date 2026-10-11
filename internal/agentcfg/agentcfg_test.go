package agentcfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func keys(items []Item) []string {
	var out []string
	for _, it := range items {
		k := it.Scope + "|" + it.Tool + "|" + it.Kind + "|" + it.Name
		if it.Detail != "" {
			k += "|" + it.Detail
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Settings mirror the structure of a real ~/.claude/settings.json (permissions, hooks with
// empty, missing and explicit matchers) with neutral values.
const settings = `{
 "permissions": {"allow": ["Bash(go test:*)"], "ask": ["Bash(git push:*)"], "deny": ["Read(./.env)"], "defaultMode": "auto"},
 "hooks": {
  "PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "secret-script --token abc"}]}, {"matcher": "", "hooks": []}],
  "Stop": [{"hooks": [{"type": "command", "command": "x"}]}]
 },
 "env": {"API_KEY": "should-never-be-read-into-items"}
}`

func TestProjectInventoryAndHistory(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	write(t, repo, "CLAUDE.md", "v1")
	write(t, repo, "AGENTS.md", "temp")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "add")
	write(t, repo, "CLAUDE.md", "v2")
	git(t, repo, "rm", "-q", "AGENTS.md")
	write(t, repo, ".claude/agents/reviewer.md", "---\nname: code-reviewer\ndescription: x\n---\nbody")
	write(t, repo, ".claude/skills/deploy/SKILL.md", "no front matter")
	write(t, repo, ".claude/commands/ship.md", "x")
	write(t, repo, ".claude/settings.json", settings)
	write(t, repo, ".mcp.json", `{"mcpServers": {"playwright": {"command": "npx", "env": {"TOKEN": "x"}}}}`)
	write(t, repo, ".claude/.env", "SECRET=1")
	write(t, repo, ".github/copilot-instructions.md", "x")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "more")

	items, problems := Project(repo)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	hist, err := GitHistory(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	items = Annotate(items, hist)
	want := []string{
		"project|claude|agent|code-reviewer",
		"project|claude|command|ship",
		"project|claude|hook|PostToolUse|*",
		"project|claude|hook|PostToolUse|Write",
		"project|claude|hook|Stop|*",
		"project|claude|instructions|CLAUDE.md",
		"project|claude|permission|Bash(git push:*)|ask",
		"project|claude|permission|Bash(go test:*)|allow",
		"project|claude|permission|Read(./.env)|deny",
		"project|claude|settings|settings.json",
		"project|claude|skill|deploy",
		"project|copilot|instructions|.github/copilot-instructions.md",
		"project|generic|instructions|AGENTS.md",
		"project|mcp|mcp|playwright",
		"project|mcp|settings|.mcp.json",
	}
	if got := keys(items); !reflect.DeepEqual(got, want) {
		t.Errorf("items:\n got %v\nwant %v", got, want)
	}
	for _, it := range items {
		switch it.Path {
		case "CLAUDE.md":
			if it.Changes != 2 || !it.Present {
				t.Errorf("CLAUDE.md history = %+v", it)
			}
		case "AGENTS.md":
			if it.Present || it.Changes != 2 {
				t.Errorf("deleted AGENTS.md = %+v, want present=false changes=2", it)
			}
		}
	}
}

func TestGlobalWithPlugins(t *testing.T) {
	home := t.TempDir()
	codex := t.TempDir()
	plugin := filepath.Join(home, "plugins", "cache", "m", "ecc", "1.0")
	write(t, home, "settings.json", `{"enabledPlugins": {"ecc@m": true, "off@m": false}, "hooks": {"Stop": [{"hooks": []}]}}`)
	shared := t.TempDir()
	write(t, shared, "herdr/SKILL.md", "---\nname: herdr\n---")
	if err := os.MkdirAll(filepath.Join(home, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Shared skills are usually symlinked into ~/.claude/skills.
	if err := os.Symlink(filepath.Join(shared, "herdr"), filepath.Join(home, "skills", "herdr")); err != nil {
		t.Fatal(err)
	}
	write(t, home, "agents/.gitkeep", "")
	write(t, home, "plugins/installed_plugins.json", `{"version": 2, "plugins": {"ecc@m": [{"installPath": "`+plugin+`", "version": "1.0"}], "off@m": [{"installPath": "`+filepath.Join(home, "nope")+`"}]}}`)
	write(t, plugin, "skills/go-review/SKILL.md", "x")
	write(t, plugin, "agents/security-reviewer.md", "---\nname: security-reviewer\n---")
	write(t, plugin, "commands/plan.md", "x")
	write(t, plugin, "hooks/hooks.json", `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": []}]}}`)
	write(t, codex, "AGENTS.md", "x")
	write(t, codex, "skills/s1/SKILL.md", "x")
	write(t, codex, "config.toml", "model = \"x\"\n[mcp_servers.context7]\ncommand = \"npx\"\n[mcp_servers.context7.env]\nKEY = \"secret\"\n")
	write(t, codex, "auth.json", `{"token": "secret"}`)

	items, problems := Global(home, codex)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	want := []string{
		"global|claude|hook|Stop|*",
		"global|claude|skill|herdr",
		"global|codex|instructions|AGENTS.md",
		"global|codex|mcp|context7",
		"global|codex|skill|s1",
		"plugin|claude|agent|ecc:security-reviewer|zapnutý",
		"plugin|claude|command|ecc:plan|zapnutý",
		"plugin|claude|hook|PreToolUse|Bash zapnutý",
		"plugin|claude|plugin|ecc|zapnutý",
		"plugin|claude|plugin|off|vypnutý",
		"plugin|claude|skill|ecc:go-review|zapnutý",
	}
	if got := keys(items); !reflect.DeepEqual(got, want) {
		t.Errorf("items:\n got %v\nwant %v", got, want)
	}
}

func TestParseHistorySkipsMalformed(t *testing.T) {
	out := []byte("\x1e1700000000\n\nCLAUDE.md\n\x1enot-a-time\n\nAGENTS.md\n\x1e1600000000\n\nCLAUDE.md\n")
	h := parseHistory(out)
	if len(h) != 1 || h["CLAUDE.md"].Changes != 2 || h["CLAUDE.md"].First.Unix() != 1600000000 || h["CLAUDE.md"].Last.Unix() != 1700000000 {
		t.Errorf("history = %+v", h)
	}
}

// Rules that redact to the same text must stay separate items, not collapse into one.
func TestRedactedCollisionsStayDistinct(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, ".claude/settings.json", `{"permissions": {"deny": ["Bash(mysql pw1:*)", "Bash(mysql pw2:*)"]}}`)
	items, problems := Project(repo)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	names := map[string]bool{}
	for _, it := range items {
		if it.Kind == KindPermission {
			names[it.Name] = true
		}
	}
	if len(names) != 2 {
		t.Errorf("permission names = %v, want 2 distinct entries", names)
	}
	for n := range names {
		if strings.Contains(n, "pw1") || strings.Contains(n, "pw2") {
			t.Errorf("secret leaked: %q", n)
		}
	}
}

// A parse error must not quote the file's content (a symlinked secret would leak through it).
func TestSettingsParseErrorDoesNotQuoteContent(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, ".claude/settings.json", "Zsecret-content")
	_, problems := Project(repo)
	if len(problems) != 1 {
		t.Fatalf("problems = %v", problems)
	}
	if strings.Contains(problems[0].Err, "'Z'") || strings.Contains(problems[0].Err, "secret") {
		t.Errorf("error quotes file content: %q", problems[0].Err)
	}
}
