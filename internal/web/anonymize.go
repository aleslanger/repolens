package web

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"repolens/internal/report"
)

// hiddenValue replaces free text that cannot be pseudonymized meaningfully.
const hiddenValue = "(skryto)"

var recordKind = regexp.MustCompile(`^[a-z_]+(:[a-z_]+)?$`)

// pseudonyms hands out stable replacement names: the same input always gets the same output
// within one export, so a repository or a person stays recognisable across pages.
type pseudonyms struct {
	prefix string
	seen   map[string]string
}

func newPseudonyms(prefix string) *pseudonyms {
	return &pseudonyms{prefix: prefix, seen: map[string]string{}}
}

func (p *pseudonyms) of(v string) string {
	if v == "" {
		return ""
	}
	if out, ok := p.seen[v]; ok {
		return out
	}
	out := fmt.Sprintf("%s%d", p.prefix, len(p.seen)+1)
	p.seen[v] = out
	return out
}

// anonymizer holds the pseudonym tables of one export.
type anonymizer struct {
	repos, owners, people, emails, dirs, branches, labels, words, paths *pseudonyms
	repoByID                                                            map[int64]string
	repoByName                                                          map[string]string
	doneAgents                                                          map[*report.AgentView]bool
	// names pseudonymizes user-defined agent items (skills, agents, commands, MCP servers,
	// plugins); bots holds author names the database marks as bots, which stay readable.
	names *pseudonyms
	bots  map[string]bool
}

// anonymize rewrites the bundle in place so it can be shared: repository, owner and people names,
// e-mails, paths, branches, commit subjects, milestones, releases, labels and workflows are
// replaced; counts, times and agent/tool names are kept. Bots keep their names.
func anonymize(b *Bundle) {
	a := &anonymizer{
		repos: newPseudonyms("repo-"), owners: newPseudonyms("owner-"), people: newPseudonyms("Person "),
		emails: newPseudonyms("person-"), dirs: newPseudonyms("dir-"), branches: newPseudonyms("branch-"),
		labels: newPseudonyms("label-"), words: newPseudonyms("item-"), paths: newPseudonyms("/home/user/dir-"),
		repoByID: map[int64]string{}, repoByName: map[string]string{}, doneAgents: map[*report.AgentView]bool{},
		names: newPseudonyms("custom-"), bots: botNames(b),
	}
	// Name repositories in portfolio order so the numbering is stable and readable.
	ids := make([]int64, 0, len(b.Portfolio))
	for _, r := range b.Portfolio {
		ids = append(ids, r.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		a.repoByID[id] = a.repos.of(fmt.Sprint(id))
	}
	for _, r := range b.Portfolio {
		a.repoByName[r.Name] = a.repoByID[r.ID]
	}
	for i := range b.Portfolio {
		a.repoRow(&b.Portfolio[i])
	}
	// Map iteration order is random; walk repositories by id so pseudonyms are reproducible.
	repoIDs := make([]int64, 0, len(b.Repos))
	for id := range b.Repos {
		repoIDs = append(repoIDs, id)
	}
	slices.Sort(repoIDs)
	for _, id := range repoIDs {
		a.detail(b.Repos[id])
	}
	a.agents(b.Agents)
	if b.People != nil {
		for i := range b.People.People {
			a.person(&b.People.People[i])
		}
	}
	a.status(b.Status)
	for _, p := range b.Persons {
		if p == nil {
			continue
		}
		a.person(&p.Person)
		for i := range p.Recent {
			c := &p.Recent[i]
			c.Hash, c.Subject, c.Author = "", hiddenValue, a.personName(c.Author)
			c.Repo = a.repoName(c.RepoID, c.Repo)
		}
	}
	for i := range b.Attention {
		it := &b.Attention[i]
		if name, ok := it.Args["repo"].(string); ok {
			it.Args["repo"] = a.repoName(it.RepoID, name)
		}
	}
	for _, s := range b.Sessions {
		a.sessionRow(&s.Session)
		for i := range s.Threads {
			a.sessionRow(&s.Threads[i])
		}
		a.counters(s.Counters)
	}
	b.Anonymized = true
}

// botNames collects the authors the database marks as bots or agents.
func botNames(b *Bundle) map[string]bool {
	out := map[string]bool{}
	for _, d := range b.Repos {
		for _, x := range d.Authors {
			if x.Bot {
				out[x.Name] = true
			}
		}
	}
	if b.People != nil {
		for _, p := range b.People.People {
			if p.Bot {
				out[p.Name] = true
				for _, id := range p.Identities {
					out[id.Name] = true
				}
			}
		}
	}
	return out
}

// keptKinds are counters whose names come from the agents themselves (tool names, hook events,
// denial kinds), not from the user, and stay readable unless they name an MCP tool.
var keptKinds = map[string]bool{"tool": true, "tool_error": true, "hook": true, "hook_block": true, "denial": true}

// itemName pseudonymizes a user-defined name; built-in items and agent vocabulary are kept. The
// same name gets the same pseudonym in counters, trends and configuration, so usage still matches.
func (a *anonymizer) itemName(kind, name string) string {
	if strings.Contains(name, "(vestavěný)") || (keptKinds[kind] && !strings.HasPrefix(name, "mcp__")) {
		return name
	}
	slash := strings.HasPrefix(name, "/")
	out := a.names.of(strings.ToLower(strings.TrimPrefix(name, "/")))
	if slash {
		return "/" + out
	}
	return out
}

func (a *anonymizer) counters(m map[string][]report.CounterRow) {
	for kind, rows := range m {
		for i := range rows {
			rows[i].Name = a.itemName(kind, rows[i].Name)
		}
	}
}

// configKinds maps configuration kinds to the counter kind their usage is recorded under.
var configKinds = map[string]string{"skill": "skill", "agent": "subagent", "command": "slash", "mcp": "mcp", "plugin": "plugin", "rule": "rule"}

func (a *anonymizer) config(c *report.ConfigRow) {
	base := path.Base(c.Path)
	if kind, ok := configKinds[c.Kind]; ok {
		c.Name = a.itemName(kind, c.Name)
		base = "…" + path.Ext(base) // file names repeat the item's name
	}
	c.Path = base
	// Permission rules name files, hosts and commands: keep only the tool, "Read(…)".
	if tool, _, ok := strings.Cut(c.Name, "("); ok && c.Kind == "permission" {
		c.Name = tool + "(…)"
	}
	if strings.Contains(c.Detail, "mcp__") {
		c.Detail = hiddenValue
	}
	if c.Repo != "" {
		c.Repo = a.repoName(c.RepoID, c.Repo)
	}
}

func (a *anonymizer) repoName(id int64, name string) string {
	if n, ok := a.repoByID[id]; ok {
		return n
	}
	if n, ok := a.repoByName[name]; ok {
		return n
	}
	return a.repos.of("name:" + name)
}

func (a *anonymizer) repoRow(r *report.PortfolioRow) {
	r.Name = a.repoName(r.ID, r.Name)
	if r.Owner != "" {
		r.Owner = a.owners.of(r.Owner)
	}
	switch {
	case strings.HasPrefix(r.Key, "local:"):
		r.Key = "local:" + r.Name
	default:
		host, _, _ := strings.Cut(r.Key, "/") // the hosting service is not identifying
		r.Key = host + "/" + r.Owner + "/" + r.Name
	}
	if r.LocalPath != "" {
		r.LocalPath = "/home/user/code/" + r.Name
	}
	r.Description = ""
}

func (a *anonymizer) detail(d *report.RepoDetail) {
	a.repoRow(&d.Repo)
	for i := range d.Worktrees {
		w := &d.Worktrees[i]
		w.Path = "/home/user/code/" + d.Repo.Name + "-" + fmt.Sprint(i+1)
		if !strings.HasPrefix(w.Branch, "(") {
			w.Branch = a.branches.of(w.Branch)
		}
	}
	for i := range d.Authors {
		a.author(&d.Authors[i])
	}
	for i := range d.Dirs {
		d.Dirs[i].Dir = a.dirs.of(d.Repo.Name + "/" + d.Dirs[i].Dir)
	}
	for i := range d.Recent {
		c := &d.Recent[i]
		c.Hash, c.Subject = "", hiddenValue
		c.Author = a.personName(c.Author)
	}
	for i := range d.Milestones {
		d.Milestones[i].Title = a.words.of("milestone:" + d.Milestones[i].Title)
	}
	for i := range d.Releases {
		r := &d.Releases[i]
		r.Tag, r.Name = a.words.of("tag:"+r.Tag), ""
	}
	for i := range d.CI {
		d.CI[i].Workflow = a.words.of("workflow:" + d.CI[i].Workflow)
	}
	for i := range d.Labels {
		d.Labels[i] = a.labels.of(d.Labels[i])
	}
	for i := range d.Open {
		o := &d.Open[i]
		o.Title, o.Labels = hiddenValue, ""
		if o.Author != "" && !strings.Contains(o.Author, "[bot]") {
			o.Author = a.people.of("login:" + strings.ToLower(o.Author))
		}
	}
	a.agents(d.Agents)
}

// personName maps a person's display name; names the database marks as bots stay as they are.
func (a *anonymizer) personName(name string) string {
	if a.bots[name] || strings.Contains(strings.ToLower(name), "[bot]") {
		return name
	}
	return a.people.of(name)
}

func (a *anonymizer) email(e string) string {
	if strings.Contains(strings.ToLower(e), "[bot]") {
		return e
	}
	return a.emails.of(strings.ToLower(e)) + "@example.com"
}

func (a *anonymizer) author(x *report.Author) {
	if x.Bot {
		return
	}
	x.Name = a.personName(x.Name)
	for i, e := range x.Emails {
		x.Emails[i] = a.email(e)
	}
}

func (a *anonymizer) person(p *report.PersonRow) {
	bot := p.Bot
	a.author(&p.Author)
	for i := range p.Repos {
		p.Repos[i].Name = a.repoName(p.Repos[i].RepoID, p.Repos[i].Name)
	}
	for i := range p.Identities {
		id := &p.Identities[i]
		if !bot {
			id.Name, id.Email = a.personName(id.Name), a.email(id.Email)
		}
	}
}

func (a *anonymizer) agents(v *report.AgentView) {
	// The same view may be referenced twice; rewriting it again would re-map pseudonyms.
	if v == nil || a.doneAgents[v] {
		return
	}
	a.doneAgents[v] = true
	for i := range v.Config {
		a.config(&v.Config[i])
	}
	a.counters(v.Counters)
	a.counters(v.Unconfigured)
	for kind, tr := range v.Trends {
		for i, n := range tr.Names {
			tr.Names[i] = a.itemName(kind, n)
		}
	}
	for i := range v.Projects {
		v.Projects[i].Name = a.repoName(v.Projects[i].RepoID, v.Projects[i].Name)
	}
	for i := range v.Sessions {
		a.sessionRow(&v.Sessions[i])
	}
	for i := range v.Unmatched {
		v.Unmatched[i].CWD = a.paths.of(v.Unmatched[i].CWD)
	}
}

func (a *anonymizer) sessionRow(s *report.SessionRow) {
	if s.RepoID != 0 {
		s.Repo = a.repoName(s.RepoID, s.Repo)
		s.CWD = "/home/user/code/" + s.Repo
		return
	}
	s.CWD = a.paths.of(s.CWD)
}

func (a *anonymizer) status(s *report.Status) {
	if s == nil {
		return
	}
	for i := range s.Runs {
		s.Runs[i].Target = hiddenValue
	}
	for i := range s.LastErrors {
		s.LastErrors[i] = report.RunError{Target: "", Message: hiddenValue}
	}
	// Only record kinds ("attachment:new_type") stay; free text may name files or repositories.
	for i := range s.Issues {
		if !recordKind.MatchString(s.Issues[i].What) {
			s.Issues[i].What = hiddenValue
		}
	}
}
