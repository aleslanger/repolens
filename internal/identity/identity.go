// Package identity merges commit author identities (name + e-mail) into people.
package identity

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"repolens/internal/config"
)

type Identity struct {
	Name, Email string
	Commits     int
}

// Member is an identity inside a person, with the rule that attached it ("" for the first one).
type Member struct {
	Identity
	Via string
}

type Person struct {
	Name    string
	Bot     bool
	Members []Member
}

const (
	viaEmail  = "e-mail"
	viaName   = "jméno"
	viaLogin  = "GitHub login"
	viaConfig = "config"
)

var (
	noreply = regexp.MustCompile(`^(?:\d+\+)?([^@]+)@users\.noreply\.github\.com$`)
	botWord = regexp.MustCompile(`(?i)(\[bot\]|\bbot\b)`)
	// agentish matches names that are an agent's name as a whole ("Claude", "Copilot CLI"),
	// so a person called "Claude Monet" or "Devin Smith" stays human.
	agentish = regexp.MustCompile(`(?i)^(github )?(copilot|claude|codex|cursor|gemini|devin|anthropic|openai|dependabot|renovate|greptile)( (cli|code|agent|autofix|bot|ai))*$`)
)

// translit folds Latin letters with diacritics that NFD would decompose; the standard library
// has no Unicode normaliser, so the common European set is listed explicitly.
var translit = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a", "ą", "a", "č", "c", "ć", "c", "ç", "c",
	"ď", "d", "é", "e", "è", "e", "ê", "e", "ë", "e", "ě", "e", "ę", "e", "í", "i", "ì", "i", "î", "i", "ï", "i",
	"ľ", "l", "ĺ", "l", "ł", "l", "ň", "n", "ń", "n", "ñ", "n", "ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o",
	"ø", "o", "ő", "o", "ř", "r", "ŕ", "r", "š", "s", "ś", "s", "ş", "s", "ť", "t", "ú", "u", "ù", "u", "û", "u",
	"ü", "u", "ů", "u", "ű", "u", "ý", "y", "ÿ", "y", "ž", "z", "ź", "z", "ż", "z", "ß", "ss", "æ", "ae", "œ", "oe",
)

// normName lower-cases, strips diacritics and collapses whitespace.
func normName(s string) string {
	return strings.Join(strings.Fields(translit.Replace(strings.ToLower(s))), " ")
}

// compact keeps letters and digits only: "Jiří Novák" -> "jirinovak", comparable to logins.
func compact(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, normName(s))
}

func isBot(id Identity) bool {
	return botWord.MatchString(id.Name) || botWord.MatchString(id.Email) || agentish.MatchString(id.Name)
}

// keys returns the merge keys of an identity in priority order with the reason each implies.
func keys(id Identity) [][2]string {
	out := [][2]string{{"e:" + strings.ToLower(id.Email), viaEmail}}
	if n := normName(id.Name); n != "" {
		out = append(out, [2]string{"n:" + n, viaName})
	}
	if c := compact(id.Name); c != "" {
		out = append(out, [2]string{"l:" + c, viaLogin})
	}
	if m := noreply.FindStringSubmatch(strings.ToLower(id.Email)); m != nil {
		out = append(out, [2]string{"l:" + compact(strings.TrimSuffix(m[1], "[bot]")), viaLogin})
	}
	return out
}

type unionFind struct{ parent []int }

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *unionFind) union(a, b int) { u.parent[u.find(b)] = u.find(a) }

// Merge groups identities into people. Identities are processed by commit count, so the most
// used one anchors each person. Bots never merge with humans automatically.
func Merge(ids []Identity, manual []config.Identity) []Person {
	sorted := append([]Identity(nil), ids...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Commits > sorted[j].Commits })
	n := len(sorted)
	uf := &unionFind{parent: make([]int, n)}
	via := make([]string, n)
	bot := make([]bool, n)
	owner := map[string]int{} // merge key -> first identity holding it (per bot class)
	for i, id := range sorted {
		uf.parent[i] = i
		bot[i] = isBot(id)
		for _, k := range keys(id) {
			key := k[0]
			if bot[i] {
				key = "bot|" + key
			}
			if j, ok := owner[key]; ok {
				if uf.find(i) != uf.find(j) {
					if via[i] == "" && uf.find(i) == i {
						via[i] = k[1]
					}
					uf.union(j, i)
				}
				continue
			}
			owner[key] = i
		}
	}
	names := applyManual(sorted, manual, uf, via)
	return collect(sorted, uf, via, bot, names)
}

// applyManual merges identities listed in config and returns display names by group root.
func applyManual(ids []Identity, manual []config.Identity, uf *unionFind, via []string) map[int]string {
	names := map[int]string{}
	pending := map[int]string{}
	for _, m := range manual {
		aliases := map[string]bool{}
		for _, a := range append(m.Aliases, m.Name) {
			aliases[strings.ToLower(a)] = true
			aliases[normName(a)] = true
		}
		first := -1
		for i, id := range ids {
			login := ""
			if mm := noreply.FindStringSubmatch(strings.ToLower(id.Email)); mm != nil {
				login = mm[1]
			}
			if !aliases[strings.ToLower(id.Email)] && !aliases[normName(id.Name)] && (login == "" || !aliases[login]) {
				continue
			}
			if first < 0 {
				first = i
				continue
			}
			if uf.find(i) != uf.find(first) {
				if via[i] == "" {
					via[i] = viaConfig
				}
				uf.union(first, i)
			}
		}
		if first >= 0 {
			pending[first] = m.Name
		}
	}
	for i, name := range pending {
		names[uf.find(i)] = name
	}
	return names
}

func collect(ids []Identity, uf *unionFind, via []string, bot []bool, names map[int]string) []Person {
	groups := map[int]*Person{}
	var order []int
	for i, id := range ids {
		root := uf.find(i)
		p := groups[root]
		if p == nil {
			p = &Person{Name: id.Name, Bot: true}
			groups[root] = p
			order = append(order, root)
		}
		v := via[i]
		if i == root {
			v = ""
		}
		p.Members = append(p.Members, Member{Identity: id, Via: v})
		p.Bot = p.Bot && bot[i]
	}
	people := make([]Person, 0, len(order))
	for _, root := range order {
		p := groups[root]
		if name, ok := names[root]; ok {
			p.Name = name
		}
		people = append(people, *p)
	}
	return people
}
