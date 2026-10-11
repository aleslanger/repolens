package agentcfg

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"repolens/internal/privacy"
)

// Global inventories ~/.claude (incl. installed plugins) and ~/.codex. ~/.claude.json is never
// read because it can hold MCP credentials; ~/.codex/auth.json is on the privacy denylist.
func Global(claudeHome, codexHome string) ([]Item, []Problem) {
	var items []Item
	var problems []Problem
	add := func(it []Item, pr []Problem) {
		items = append(items, it...)
		problems = append(problems, pr...)
	}
	add(claudeDir(ScopeGlobal, "", claudeHome))
	add(plugins(claudeHome))
	add(codexGlobal(codexHome))
	return items, problems
}

// claudeDir lists a Claude config directory laid out like ~/.claude or a plugin root.
// prefix namespaces plugin items ("ecc:"), as Claude Code shows them.
func claudeDir(scope, prefix, dir string) ([]Item, []Problem) {
	var items []Item
	var problems []Problem
	skills, probs := skillDirs(scope, prefix, filepath.Join(dir, "skills"))
	items = append(items, skills...)
	problems = append(problems, probs...)
	kinds := []struct{ sub, kind string }{
		{"agents", KindAgent}, {"commands", KindCommand}, {"rules", KindRule},
	}
	for _, k := range kinds {
		root := filepath.Join(dir, k.sub)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					problems = append(problems, Problem{path, err.Error()})
				}
				return nil
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") || privacy.Forbidden(path) {
				return nil
			}
			name := strings.TrimSuffix(d.Name(), ".md")
			if k.kind == KindRule {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					problems = append(problems, Problem{path, err.Error()})
					return nil
				}
				name = strings.TrimSuffix(filepath.ToSlash(rel), ".md")
			} else if fm := frontmatterName(path); fm != "" {
				name = fm
			}
			items = append(items, Item{Scope: scope, Tool: "claude", Kind: k.kind, Name: prefix + name, Path: path, Present: true})
			return nil
		})
		if err != nil {
			problems = append(problems, Problem{root, err.Error()})
		}
	}
	if p := filepath.Join(dir, "CLAUDE.md"); fileExists(p) {
		items = append(items, Item{Scope: scope, Tool: "claude", Kind: KindInstructions, Name: prefix + "CLAUDE.md", Path: p, Present: true})
	}
	settings := filepath.Join(dir, "settings.json")
	if scope == ScopePlugin {
		settings = filepath.Join(dir, "hooks", "hooks.json")
	}
	if fileExists(settings) {
		found, err := settingsItems(scope, "claude", settings, settings)
		if err != nil {
			problems = append(problems, Problem{settings, err.Error()})
		}
		for i := range found {
			found[i].Name = prefixHookless(prefix, found[i])
		}
		items = append(items, found...)
	}
	return items, problems
}

// skillDirs lists <root>/<name>/SKILL.md. Entries are stat'ed so symlinked skill folders
// (common for shared skills) are followed.
func skillDirs(scope, prefix, root string) ([]Item, []Problem) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []Problem{{root, err.Error()}}
	}
	var items []Item
	for _, e := range entries {
		skill := filepath.Join(root, e.Name(), "SKILL.md")
		if !fileExists(skill) || privacy.Forbidden(skill) {
			continue
		}
		name := e.Name()
		if fm := frontmatterName(skill); fm != "" {
			name = fm
		}
		items = append(items, Item{Scope: scope, Tool: "claude", Kind: KindSkill, Name: prefix + name, Path: skill, Present: true})
	}
	return items, nil
}

// prefixHookless namespaces everything except hook events, which keep Claude Code's event names.
func prefixHookless(prefix string, it Item) string {
	if it.Kind == KindHook || it.Kind == KindPermission {
		return it.Name
	}
	return prefix + it.Name
}

type installedPlugins struct {
	Plugins map[string][]struct {
		InstallPath string `json:"installPath"`
		Version     string `json:"version"`
	} `json:"plugins"`
}

// plugins lists installed Claude Code plugins and the items they contribute.
func plugins(claudeHome string) ([]Item, []Problem) {
	path := filepath.Join(claudeHome, "plugins", "installed_plugins.json")
	b, err := privacy.ReadFile(path, maxConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []Problem{{path, err.Error()}}
	}
	var ip installedPlugins
	if err := json.Unmarshal(b, &ip); err != nil {
		return nil, []Problem{{path, jsonProblem(err).Error()}}
	}
	enabled := map[string]bool{}
	if s, err := readSettings(filepath.Join(claudeHome, "settings.json")); err == nil {
		enabled = s.EnabledPlugins
	}
	var items []Item
	var problems []Problem
	for key, installs := range ip.Plugins {
		if len(installs) == 0 {
			continue
		}
		name, _, _ := strings.Cut(key, "@")
		state := "vypnutý"
		if enabled[key] {
			state = "zapnutý"
		}
		inst := installs[0]
		items = append(items, Item{Scope: ScopePlugin, Tool: "claude", Kind: KindPlugin, Name: name, Path: inst.InstallPath, Detail: state, Present: true})
		found, probs := claudeDir(ScopePlugin, name+":", inst.InstallPath)
		for i := range found {
			found[i].Detail = strings.TrimSpace(found[i].Detail + " " + state)
		}
		items = append(items, found...)
		problems = append(problems, probs...)
	}
	return items, problems
}

var tomlMCP = regexp.MustCompile(`(?m)^\[mcp_servers\.([A-Za-z0-9_-]+)\]`)

// codexGlobal lists ~/.codex instructions, skills, rules, hooks and MCP server names.
func codexGlobal(home string) ([]Item, []Problem) {
	var items []Item
	var problems []Problem
	if p := filepath.Join(home, "AGENTS.md"); fileExists(p) {
		items = append(items, Item{Scope: ScopeGlobal, Tool: "codex", Kind: KindInstructions, Name: "AGENTS.md", Path: p, Present: true})
	}
	skills, err := os.ReadDir(filepath.Join(home, "skills"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, Problem{filepath.Join(home, "skills"), err.Error()})
	}
	for _, d := range skills {
		if d.IsDir() && fileExists(filepath.Join(home, "skills", d.Name(), "SKILL.md")) {
			items = append(items, Item{Scope: ScopeGlobal, Tool: "codex", Kind: KindSkill, Name: d.Name(), Path: filepath.Join(home, "skills", d.Name()), Present: true})
		}
	}
	rules, err := os.ReadDir(filepath.Join(home, "rules"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, Problem{filepath.Join(home, "rules"), err.Error()})
	}
	for _, d := range rules {
		if !d.IsDir() {
			items = append(items, Item{Scope: ScopeGlobal, Tool: "codex", Kind: KindRule, Name: d.Name(), Path: filepath.Join(home, "rules", d.Name()), Present: true})
		}
	}
	if p := filepath.Join(home, "config.toml"); fileExists(p) {
		b, err := privacy.ReadFile(p, maxConfigFile)
		if err != nil {
			problems = append(problems, Problem{p, err.Error()})
		}
		for _, m := range tomlMCP.FindAllSubmatch(b, -1) {
			items = append(items, Item{Scope: ScopeGlobal, Tool: "codex", Kind: KindMCP, Name: string(m[1]), Path: p, Present: true})
		}
	}
	if p := filepath.Join(home, "hooks.json"); fileExists(p) {
		found, err := settingsItems(ScopeGlobal, "codex", p, p)
		if err != nil {
			problems = append(problems, Problem{p, err.Error()})
		}
		items = append(items, found...)
	}
	return items, problems
}
