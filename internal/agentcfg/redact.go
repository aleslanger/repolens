package agentcfg

import (
	"regexp"
	"strings"
)

var (
	// envAssign matches shell-style VAR=value prefixes; values may be quoted.
	envAssign = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=("[^"]*"|'[^']*'|\S+)$`)
	// commandWord is a program name such as git, python3 or docker-compose.
	commandWord = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)
	// flagWord is a value-less switch: -f, or --long-name. Single-dash words longer than one
	// letter are refused because tools such as mysql take -pSECRET.
	flagWord = regexp.MustCompile(`^(-[a-zA-Z]|--[a-z][a-z-]*)$`)
	// urlCreds matches user:password@ in URLs.
	urlCreds = regexp.MustCompile(`(://[^/\s:@]+):[^@\s]+@`)
	// urlQuery drops query strings and fragments, where API keys often live.
	urlQuery = regexp.MustCompile(`(https?://[^\s?#)]*)[?#][^\s)]*`)
	// tokenLiteral matches well-known credential formats anywhere in a rule.
	tokenLiteral = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{16,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)`)
	bashRule     = regexp.MustCompile(`(?s)^Bash\((.*)\)$`)
)

// maxKeptWords bounds how many words after the command a Bash rule keeps.
const maxKeptWords = 2

// subcommands are words that may follow a command without revealing anything: well-known
// subcommands and script names. Anything else (arguments, hosts, passwords) is cut.
var subcommands = set(
	"add", "apply", "build", "cat", "check", "checkout", "ci", "clean", "clippy", "clone", "commit", "compose",
	"config", "create", "delete", "deploy", "describe", "dev", "diff", "doc", "down", "exec", "fetch", "fmt",
	"format", "generate", "get", "help", "init", "install", "lint", "list", "log", "logs", "ls", "merge", "mod",
	"pr", "preview", "ps", "publish", "pull", "push", "rebase", "release", "remove", "reset", "restart", "restore",
	"rm", "run", "show", "start", "stash", "status", "stop", "switch", "sync", "tag", "test", "tidy", "typecheck",
	"up", "update", "version", "view", "vet", "watch", "worktree")

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// RedactRule reduces permission rules and hook matchers to what is safe to store.
// Bash rules keep variable names, the command, known subcommands and value-less flags:
// "Bash(PGPASSWORD=x psql -h db:*)" becomes "Bash(PGPASSWORD=*** psql …:*)".
// Other rules keep their shape with URL credentials, query strings and token literals removed.
func RedactRule(rule string) string {
	if m := bashRule.FindStringSubmatch(rule); m != nil {
		return "Bash(" + redactCommand(m[1]) + ")"
	}
	out := urlCreds.ReplaceAllString(rule, "$1:***@")
	out = urlQuery.ReplaceAllString(out, "$1")
	return tokenLiteral.ReplaceAllString(out, "***")
}

// valueLessFlag reports whether fields[i] is a switch with no value after it ("--force" at the
// end, or followed by another switch). "-p secret" or "--token x" are not kept at all.
func valueLessFlag(fields []string, i int) bool {
	if !flagWord.MatchString(fields[i]) {
		return false
	}
	return i+1 == len(fields) || strings.HasPrefix(fields[i+1], "-")
}

func redactCommand(cmd string) string {
	body, wildcard := strings.CutSuffix(cmd, ":*")
	var kept []string
	command, extra, truncated := false, 0, false
	fields := strings.Fields(body)
	for i, f := range fields {
		if !command {
			if m := envAssign.FindStringSubmatch(f); m != nil {
				kept = append(kept, m[1]+"=***")
				continue
			}
			if !commandWord.MatchString(f) {
				truncated = true
				break
			}
			kept, command = append(kept, f), true
			continue
		}
		if extra < maxKeptWords && (subcommands[f] || valueLessFlag(fields, i)) {
			kept, extra = append(kept, f), extra+1
			continue
		}
		truncated = true
		break
	}
	out := strings.Join(kept, " ")
	if truncated {
		out = strings.TrimSpace(out + " …")
	}
	if wildcard {
		out += ":*"
	}
	return out
}
