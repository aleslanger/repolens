package identity

import (
	"sort"
	"strings"
	"testing"

	"repolens/internal/config"
)

// realPatterns mirrors the identity shapes found in the local repositories (names and
// addresses anonymised, structure kept: diacritics variants, noreply logins, shared e-mails, bots).
var realPatterns = []Identity{
	{"Jiří Novák", "novak@mail.example", 3606},
	{"Jiří Novák", "25610549+jnovak@users.noreply.github.com", 1836},
	{"copilot", "copilot@local", 697},
	{"Jiří Novák", "jiri.novak@gmail.example", 609},
	{"Copilot", "copilot@example.invalid", 216},
	{"Scanner Bot", "action@local", 57},
	{"Jiri Novak", "novak@mail.example", 16},
	{"jnovak", "jiri.novak@gmail.example", 14},
	{"Scanner Bot", "dev@example.com", 13},
	{"OCTO_99", "novak@mail.example", 12},
	{"copilot", "copilot@users.noreply.github.com", 10},
	{"dependabot[bot]", "49699333+dependabot[bot]@users.noreply.github.com", 5},
	{"copilot-swe-agent[bot]", "198982749+copilot@users.noreply.github.com", 4},
	{"Jiri Novak", "jiri.novak@gmail.example", 3},
	{"Jiří Novák", "jnovak@users.noreply.github.com", 3},
	{"Copilot CLI", "copilot@example.com", 3},
	{"Jiří Novák", "jiri@work.example", 2},
	{"Claude", "jiri@work.example", 1}, // an agent committing with the human's address
}

func summary(people []Person) []string {
	var out []string
	for _, p := range people {
		total := 0
		for _, m := range p.Members {
			total += m.Commits
		}
		kind := "human"
		if p.Bot {
			kind = "bot"
		}
		out = append(out, strings.Join([]string{p.Name, kind, itoa(len(p.Members)), itoa(total)}, "|"))
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return string(digits[n])
	}
	return itoa(n/10) + string(digits[n%10])
}

func TestMergeRealPatterns(t *testing.T) {
	got := summary(Merge(realPatterns, nil))
	want := []string{
		"Scanner Bot|bot|2|70",
		"Claude|bot|1|1",
		"Copilot CLI|bot|1|3",
		"copilot|bot|4|927",
		"dependabot[bot]|bot|1|5",
		"Jiří Novák|human|9|6101",
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("people:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestMergeRecordsReason(t *testing.T) {
	people := Merge(realPatterns, nil)
	via := map[string]string{}
	for _, p := range people {
		for _, m := range p.Members {
			via[m.Name+" <"+m.Email+">"] = m.Via
		}
	}
	cases := map[string]string{
		"Jiří Novák <novak@mail.example>":                       "",
		"Jiri Novak <novak@mail.example>":                       "e-mail",
		"OCTO_99 <novak@mail.example>":                          "e-mail",
		"Jiří Novák <25610549+jnovak@users.noreply.github.com>": "jméno",
		"jnovak <jiri.novak@gmail.example>":                     "e-mail",
	}
	for id, want := range cases {
		if via[id] != want {
			t.Errorf("via(%s) = %q, want %q", id, via[id], want)
		}
	}
}

func TestManualMapMergesAndNames(t *testing.T) {
	ids := []Identity{
		{"Jane Roe", "jane@a.example", 5},
		{"J. Roe", "roe@b.example", 3},
		{"Bob", "bob@c.example", 1},
	}
	manual := []config.Identity{{Name: "Jane R.", Aliases: []string{"roe@b.example", "Jane Roe"}}}
	got := summary(Merge(ids, manual))
	want := []string{"Bob|human|1|1", "Jane R.|human|2|8"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("people = %v, want %v", got, want)
	}
}

func TestNormalise(t *testing.T) {
	cases := map[string]string{"Jiří Novák": "jiri novak", "  ŽLUŤOUČKÝ  kůň ": "zlutoucky kun", "Eva": "eva", "Ørjan Åse": "orjan ase"}
	for in, want := range cases {
		if got := normName(in); got != want {
			t.Errorf("normName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPeopleNamedLikeAgentsStayHuman(t *testing.T) {
	for name, bot := range map[string]bool{"Claude": true, "Copilot CLI": true, "copilot-swe-agent[bot]": true, "Claude Monet": false, "Devin Smith": false, "Scanner Bot": true} {
		if got := isBot(Identity{Name: name, Email: "x@example.com"}); got != bot {
			t.Errorf("isBot(%q) = %v, want %v", name, got, bot)
		}
	}
}
