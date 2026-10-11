package web

import (
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	// dictEntry is one `"cs": "en",` line of the EN dictionary in i18n.js.
	dictEntry = regexp.MustCompile(`(?m)^\s*("(?:[^"\\]|\\.)*"):\s*("(?:[^"\\]|\\.)*"),?\s*$`)
	// tCall matches t("…") with a literal argument.
	tCall = regexp.MustCompile(`\bt\(("(?:[^"\\]|\\.)*")`)
	// pluralCall matches plural(n, ["…", "…", "…"]).
	pluralCall = regexp.MustCompile(`plural\([^,\[]+,\s*\[((?:\s*"(?:[^"\\]|\\.)*",?)+)\s*\]`)
	quoted     = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	staticAttr = regexp.MustCompile(`data-i18n(?:-label)?="([^"]*)"`)
	// Lookup tables hold Czech source labels that are translated where shown: plain maps
	// ({key: "label"}), tables of {label: "…"} objects and [key, "label"] pairs.
	plainMap   = regexp.MustCompile(`(?s)const (?:SOURCES|RUN_MODES|KIND_LABEL|SCOPE_LABEL|COST_SOURCE|GROUP_TITLE) = \{(.*?)\};`)
	plainValue = regexp.MustCompile(`\w+: ("(?:[^"\\]|\\.)*")`)
	objMap     = regexp.MustCompile(`(?s)const (?:SORTS|TYPE_GROUPS) = [\[{](.*?)[\]}];\n`)
	objLabel   = regexp.MustCompile(`label: ("(?:[^"\\]|\\.)*")`)
	pairMap    = regexp.MustCompile(`(?s)const (?:PRICE_KEYS|TREND_KINDS|PERIODS|AGENT_TABS|REPO_TABS) = \[(.*?)\];`)
	// glossary holds [term, text] pairs; both are translated.
	glossaryMap = regexp.MustCompile(`(?s)const GLOSSARY = \[(.*?)\n  \];`)
	pairLabel   = regexp.MustCompile(`\["\w+", ("(?:[^"\\]|\\.)*")\]`)
	// problemText finds validation messages in the config package, shown translated, and
	// attentionText the texts of the attention panel built in the report package.
	problemText   = regexp.MustCompile(`problem\(("(?:[^"\\]|\\.)*")`)
	attentionText = regexp.MustCompile(`Text:\s*("(?:[^"\\]|\\.)*")|it\.Text = ("(?:[^"\\]|\\.)*")`)
)

// dynamicKeys reach t() through variables: theme names, markers stored by the collectors and
// shown as data, and the identity merge reasons.
var dynamicKeys = []string{"auto", "světlý", "tmavý", "(neplatné jméno)", "(vestavěný)", "(neznámý)", "(další klon)",
	"e-mail", "jméno", "GitHub login", "config", "dny", "týdny", "měsíce", "(ostatní)", "(skryto)", "hlavní"}

func unquote(t *testing.T, s string) string {
	t.Helper()
	u, err := strconv.Unquote(s)
	if err != nil {
		t.Fatalf("unquote %s: %v", s, err)
	}
	return u
}

func dictionary(t *testing.T) map[string]string {
	t.Helper()
	src, err := fs.ReadFile(staticFS, "static/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "const EN = {")
	end := strings.Index(string(src), "\n  };")
	if start < 0 || end < start {
		t.Fatal("EN dictionary not found in i18n.js")
	}
	dict := map[string]string{}
	for _, m := range dictEntry.FindAllStringSubmatch(string(src)[start:end], -1) {
		cs := unquote(t, m[1])
		if _, dup := dict[cs]; dup {
			t.Errorf("duplicate dictionary key %q", cs)
		}
		dict[cs] = unquote(t, m[2])
	}
	return dict
}

// usedKeys collects every Czech source string the UI translates.
func usedKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	files, err := fs.Glob(staticFS, "static/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "i18n.js") || strings.HasSuffix(f, ".css") {
			continue
		}
		b, err := fs.ReadFile(staticFS, f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, m := range tCall.FindAllStringSubmatch(src, -1) {
			keys[unquote(t, m[1])] = true
		}
		for _, m := range pluralCall.FindAllStringSubmatch(src, -1) {
			forms := quoted.FindAllString(m[1], -1)
			keys[unquote(t, forms[0])] = true
			keys[unquote(t, forms[len(forms)-1])] = true
		}
		for _, m := range staticAttr.FindAllStringSubmatch(src, -1) {
			keys[m[1]] = true
		}
		for _, m := range glossaryMap.FindAllStringSubmatch(src, -1) {
			for _, q := range quoted.FindAllString(m[1], -1) {
				keys[unquote(t, q)] = true
			}
		}
		for _, table := range []struct{ outer, inner *regexp.Regexp }{{plainMap, plainValue}, {objMap, objLabel}, {pairMap, pairLabel}} {
			for _, m := range table.outer.FindAllStringSubmatch(src, -1) {
				for _, v := range table.inner.FindAllStringSubmatch(m[1], -1) {
					keys[unquote(t, v[1])] = true
				}
			}
		}
	}
	for _, k := range dynamicKeys {
		keys[k] = true
	}
	for _, src := range []string{"../config/config.go", "../config/exclude.go", "../config/save.go"} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range problemText.FindAllStringSubmatch(string(b), -1) {
			keys[unquote(t, m[1])] = true
		}
	}
	report, err := os.ReadFile("../report/overview.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range attentionText.FindAllStringSubmatch(string(report), -1) {
		keys[unquote(t, m[1]+m[2])] = true
	}
	return keys
}

func TestEveryUIStringHasEnglish(t *testing.T) {
	dict := dictionary(t)
	var missing []string
	for k := range usedKeys(t) {
		if _, ok := dict[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		t.Errorf("missing English for %q", k)
	}
}

func TestDictionaryKeepsPlaceholders(t *testing.T) {
	ph := regexp.MustCompile(`\{\w+\}`)
	for cs, en := range dictionary(t) {
		a, b := ph.FindAllString(cs, -1), ph.FindAllString(en, -1)
		sort.Strings(a)
		sort.Strings(b)
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Errorf("placeholders differ: %q → %q", cs, en)
		}
	}
}
