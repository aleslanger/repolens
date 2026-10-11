package web

import (
	"encoding/json"
	"strings"
	"testing"

	"repolens/internal/report"
)

func identifyingBundle() *Bundle {
	repo := report.PortfolioRow{ID: 7, Key: "github.com/secretco/payroll", Name: "payroll", Owner: "secretco",
		LocalPath: "/home/jdoe/work/payroll", Description: "Payroll for ACME", Language: "Go", Commits: 3}
	author := report.Author{Name: "Jane Doe", Emails: []string{"jane@acme.example"}, Commits: 3}
	claude := report.Author{Name: "Claude Dupont", Emails: []string{"claude.d@acme.example"}, Commits: 1}
	bot := report.Author{Name: "dependabot[bot]", Emails: []string{"49699333+dependabot[bot]@users.noreply.github.com"}, Bot: true}
	session := report.SessionRow{ID: "claude:abc", RepoID: 7, Repo: "payroll", CWD: "/home/jdoe/work/payroll/api"}
	agents := &report.AgentView{
		Counters: map[string][]report.CounterRow{
			"skill": {{Name: "acme-deploy", Count: 3}}, "tool": {{Name: "Bash"}, {Name: "mcp__acmecrm__query"}},
			"slash": {{Name: "/acme-release"}, {Name: "/clear (vestavěný)"}}, "mcp": {{Name: "acmecrm"}},
		},
		Unconfigured: map[string][]report.CounterRow{"subagent": {{Name: "acme-reviewer"}}},
		Trends:       map[string]report.Trend{"skill": {Names: []string{"acme-deploy"}}},
		Config: []report.ConfigRow{{Kind: "skill", Name: "acme-deploy", Path: "/home/jdoe/work/payroll/.claude/skills/acme-deploy/SKILL.md", Repo: "payroll", RepoID: 7},
			{Kind: "agent", Name: "acme-reviewer", Path: "/home/jdoe/.claude/agents/acme-reviewer.md"},
			{Kind: "permission", Name: "Read(//home/jdoe/acme-keys/**)", Path: "/home/jdoe/work/payroll/.claude/settings.json"}},
		Projects:  []report.ProjectRow{{RepoID: 7, Name: "payroll"}},
		Sessions:  []report.SessionRow{session},
		Unmatched: []report.CWDCount{{CWD: "/home/jdoe/private-notes", Sessions: 2}},
	}
	return &Bundle{
		Portfolio: []report.PortfolioRow{repo},
		Repos: map[int64]*report.RepoDetail{7: {
			Repo:       repo,
			Worktrees:  []report.Worktree{{Path: "/home/jdoe/.wt/payroll-fix", Branch: "fix/jane-salary-bug"}, {Path: "/srv/payroll", Branch: "(další klon)"}},
			Authors:    []report.Author{author, bot, claude},
			Dirs:       []report.DirStat{{Dir: "internal/salaries"}},
			Recent:     []report.CommitRow{{Hash: "deadbeef", Author: "Jane Doe", Subject: "fix: raise for Bob"}},
			Milestones: []report.Milestone{{Title: "ACME launch"}},
			Releases:   []report.Release{{Tag: "v1.0-acme", Name: "ACME GA"}},
			CI:         []report.CIStats{{Workflow: "deploy-acme"}},
			Labels:     []string{"acme-urgent"},
			Agents:     agents,
			Open:       []report.OpenItem{{Number: 9, Title: "ACME invoice export", Author: "jdoe", Labels: "acme"}},
		}},
		Persons: map[int64]*report.PersonView{1: {Person: report.PersonRow{Author: author, Repos: []report.PersonRepo{{RepoID: 7, Name: "payroll"}}},
			Recent: []report.PersonCommit{{CommitRow: report.CommitRow{Hash: "cafe", Author: "Jane Doe", Subject: "fix: acme"}, RepoID: 7, Repo: "payroll"}}}},
		Attention: []report.AttentionItem{{Kind: "ci", Text: "{repo}", Args: map[string]any{"repo": "payroll"}, RepoID: 7}},
		Agents:    agents,
		People: &report.PeopleView{People: []report.PersonRow{{
			Author:     author,
			Repos:      []report.PersonRepo{{RepoID: 7, Name: "payroll"}},
			Identities: []report.IdentityRow{{Name: "Jane Doe", Email: "jane@acme.example", Via: "e-mail"}, {Name: "jdoe", Email: "123+jdoe@users.noreply.github.com"}},
		}}},
		Status: &report.Status{
			Runs:       []report.Run{{Mode: "all", Target: "/home/jdoe/work, secretco"}},
			LastErrors: []report.RunError{{Target: "/home/jdoe/work/payroll-db", Message: "open /home/jdoe/work/payroll-db: denied"}},
			Issues:     []report.IssueRow{{Source: "claude", Kind: "error", What: "/home/jdoe/.claude/projects/x.jsonl: bad line"}, {Source: "claude", Kind: "unrecognized", What: "attachment:new_type"}, {Source: "git", Kind: "error", What: "payroll-secrets"}},
		},
		Sessions: map[string]*report.SessionView{"claude:abc": {Session: session, Threads: []report.SessionRow{session}}},
	}
}

func TestAnonymizeRemovesIdentifyingValues(t *testing.T) {
	b := identifyingBundle()
	anonymize(b)
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, secret := range []string{"payroll", "secretco", "jdoe", "Jane", "acme", "ACME", "salar", "Bob", "deadbeef", "private-notes", "/srv/", "Dupont"} {
		if strings.Contains(s, secret) {
			t.Errorf("anonymized export still contains %q", secret)
		}
	}
	// Structure and numbers survive; the same repository and person get the same pseudonym everywhere.
	d := b.Repos[7]
	if b.Portfolio[0].Name != d.Repo.Name || b.Agents.Projects[0].Name != d.Repo.Name || b.People.People[0].Repos[0].Name != d.Repo.Name ||
		b.Agents.Config[0].Repo != d.Repo.Name {
		t.Errorf("repo pseudonyms differ: %q %q %q %q", b.Portfolio[0].Name, b.Agents.Projects[0].Name, b.People.People[0].Repos[0].Name, b.Agents.Config[0].Repo)
	}
	if d.Recent[0].Author != d.Authors[0].Name || b.People.People[0].Name != d.Authors[0].Name {
		t.Errorf("person pseudonyms differ: %q %q %q", d.Recent[0].Author, d.Authors[0].Name, b.People.People[0].Name)
	}
	if d.Authors[1].Name != "dependabot[bot]" || d.Worktrees[1].Branch != "(další klon)" || b.Status.Issues[1].What != "attachment:new_type" {
		t.Error("bots, markers and record kinds must stay readable")
	}
	// A user-defined skill keeps one pseudonym everywhere, so usage still matches configuration.
	skill := b.Agents.Counters["skill"][0].Name
	if b.Agents.Config[0].Name != skill || b.Agents.Trends["skill"].Names[0] != skill || b.Agents.Unconfigured["subagent"][0].Name != b.Agents.Config[1].Name {
		t.Errorf("item pseudonyms differ: %q %q %q", skill, b.Agents.Config[0].Name, b.Agents.Trends["skill"].Names[0])
	}
	if c := b.Agents.Counters; c["tool"][0].Name != "Bash" || c["slash"][1].Name != "/clear (vestavěný)" || !strings.HasPrefix(c["slash"][0].Name, "/") {
		t.Errorf("agent vocabulary changed: %+v", c)
	}
	if b.Attention[0].Args["repo"] != d.Repo.Name || b.Persons[1].Recent[0].Repo != d.Repo.Name {
		t.Errorf("attention or person pages keep other pseudonyms: %v %q", b.Attention[0].Args, b.Persons[1].Recent[0].Repo)
	}
	if b.Agents.Unmatched[0].CWD != "/home/user/dir-1" {
		t.Errorf("unmatched cwd = %q; a view shared by two pages must be anonymized once", b.Agents.Unmatched[0].CWD)
	}
	if !strings.HasPrefix(b.Portfolio[0].Key, "github.com/owner-") {
		t.Errorf("key = %q, want the host kept and the owner replaced", b.Portfolio[0].Key)
	}
	if b.Portfolio[0].Commits != 3 || b.Portfolio[0].Language != "Go" || !b.Anonymized {
		t.Errorf("numbers or flags lost: %+v", b.Portfolio[0])
	}
}
