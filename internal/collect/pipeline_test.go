package collect

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"repolens/internal/config"
	"repolens/internal/report"
	"repolens/internal/store"
)

func gitAt(t *testing.T, dir, when string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Jiří Novák", "GIT_AUTHOR_EMAIL=jiri@example.com",
		"GIT_COMMITTER_NAME=Jiří Novák", "GIT_COMMITTER_EMAIL=jiri@example.com",
		"GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sessionJSONL mirrors the record shapes of a real Claude Code 2.1 transcript (see
// internal/transcript/testdata/claude) for a short session in repoDir.
func sessionJSONL(repoDir string) string {
	rec := func(ts, body string) string {
		return fmt.Sprintf(`{"timestamp":"%s","cwd":"%s","sessionId":"s1",%s}`+"\n", ts, repoDir, body)
	}
	return rec("2026-09-01T10:00:00.000Z", `"type":"user","message":{"role":"user","content":"<command-name>/deploy</command-name>"}`) +
		rec("2026-09-01T10:01:00.000Z", `"type":"assistant","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-5","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"subagent_type":"code-reviewer"}}],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":1000,"cache_creation_input_tokens":10}}`) +
		rec("2026-09-01T10:02:00.000Z", `"type":"assistant","requestId":"r2","message":{"id":"m2","model":"claude-sonnet-5","role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"secret-should-not-be-stored"}}],"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`) +
		rec("2026-09-01T10:03:00.000Z", `"type":"user","toolDenialKind":"user-rejected","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"secret output"}]}`) +
		rec("2026-09-01T10:04:00.000Z", `"type":"system","subtype":"turn_duration","durationMs":240000`) +
		rec("2026-09-01T10:05:00.000Z", `"type":"cost-state","modelUsage":{"claude-sonnet-5":{"costUSD":0.25}}`)
}

func TestPipelineFromGitAndTranscriptsToReports(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "demo")
	writeFile(t, filepath.Join(repo, ".claude/skills/deploy/SKILL.md"), "---\nname: deploy\n---")
	writeFile(t, filepath.Join(repo, ".claude/agents/rev.md"), "---\nname: code-reviewer\n---")
	writeFile(t, filepath.Join(repo, ".claude/commands/never-used.md"), "x")
	gitAt(t, repo, "2026-08-01T09:00:00Z", "init", "-q", "-b", "main")
	gitAt(t, repo, "2026-08-01T09:00:00Z", "add", "-A")
	gitAt(t, repo, "2026-08-01T09:00:00Z", "commit", "-q", "-m", "chore: agent config\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
	writeFile(t, filepath.Join(repo, "main.go"), "package main\n")
	gitAt(t, repo, "2026-09-01T10:06:00Z", "add", "-A")
	gitAt(t, repo, "2026-09-01T10:06:00Z", "commit", "-q", "-m", "feat: inside the session window")
	writeFile(t, filepath.Join(repo, "b.go"), "package main\n")
	gitAt(t, repo, "2026-09-03T10:00:00Z", "add", "-A")
	gitAt(t, repo, "2026-09-03T10:00:00Z", "commit", "-q", "-m", "fix: outside any session")

	claudeHome := t.TempDir()
	writeFile(t, filepath.Join(claudeHome, "projects", claudeEncode(repo), "s1.jsonl"), sessionJSONL(repo))
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{ScanDepth: 2, ModuleDepth: 1, ClaudeHome: claudeHome, CodexHome: t.TempDir()}

	sum := &Summary{}
	repos, err := Dir(ctx, st, cfg, root, io.Discard, sum)
	if err != nil {
		t.Fatal(err)
	}
	if err := Agents(ctx, st, cfg, repos, io.Discard, sum); err != nil {
		t.Fatal(err)
	}
	if err := MarkCommits(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := People(ctx, st, cfg); err != nil {
		t.Fatal(err)
	}
	if sum.Repos != 1 || sum.Commits != 3 || sum.Sessions != 1 || sum.Unrecognized != 0 || sum.Errors != 0 {
		t.Fatalf("summary = %+v", sum)
	}

	av, err := report.AgentsFor(ctx, st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if av.Totals.Sessions != 1 || av.Totals.Tokens != 1170 || av.Totals.CostUSD != 0.25 || av.Totals.ActiveMs != 240000 {
		t.Errorf("totals = %+v", av.Totals)
	}
	uses := map[string]int{}
	for _, c := range av.Config {
		if c.Usable {
			uses[c.Kind+":"+c.Name] = c.Uses
		}
	}
	want := map[string]int{"skill:deploy": 1, "agent:code-reviewer": 1, "command:never-used": 0}
	for k, n := range want {
		if got, ok := uses[k]; !ok || got != n {
			t.Errorf("uses[%s] = %d (present %v), want %d", k, got, ok, n)
		}
	}
	if len(av.Counters["denial"]) != 1 || av.Counters["tool_error"][0].Name != "Bash" {
		t.Errorf("counters = %+v", av.Counters)
	}
	var trailer, inSession int
	for _, m := range av.Attribution {
		trailer += m.Trailer
		inSession += m.InSession
	}
	if trailer != 1 || inSession != 1 {
		t.Errorf("attribution trailer=%d inSession=%d, want 1 and 1", trailer, inSession)
	}

	// Privacy: nothing from prompts or tool I/O reaches the database.
	var leaks int
	if err := st.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM session_counter WHERE name LIKE '%secret%')
		+ (SELECT COUNT(*) FROM agent_session WHERE cwd LIKE '%secret%')`).Scan(&leaks); err != nil {
		t.Fatal(err)
	}
	if leaks != 0 {
		t.Errorf("transcript content leaked into the database (%d rows)", leaks)
	}

	pf, err := report.Portfolio(ctx, st.DB, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(pf) != 1 || pf[0].Sessions != 1 || pf[0].Commits != 3 {
		t.Fatalf("portfolio = %+v", pf)
	}
	d, err := report.RepoDetailFor(ctx, st.DB, time.Now(), pf[0].ID)
	if err != nil || d == nil || len(d.Authors) != 1 || d.Agents.Totals.Sessions != 1 {
		t.Fatalf("detail err=%v authors=%v", err, d)
	}
	people, err := report.People(ctx, st.DB)
	if err != nil || len(people.People) != 1 || people.People[0].Commits != 3 || people.People[0].AgentCommits != 2 {
		t.Fatalf("people err=%v %+v", err, people)
	}

	// A second run re-reads nothing.
	again := &Summary{}
	if err := Agents(ctx, st, cfg, repos, io.Discard, again); err != nil {
		t.Fatal(err)
	}
	if again.Sessions != 0 {
		t.Errorf("unchanged transcripts re-ingested: %d", again.Sessions)
	}
}
