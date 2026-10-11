package gitlog

import (
	"os"
	"reflect"
	"testing"
)

func parseFixture(t *testing.T, name string) ([]Commit, Stats) {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var commits []Commit
	stats, err := Parse(f, func(c Commit) error {
		commits = append(commits, c)
		return nil
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return commits, stats
}

func TestParseRealRepoFixture(t *testing.T) {
	commits, stats := parseFixture(t, "akcie_anon.log")

	if stats.Records != 30 || stats.BadRecords != 0 || len(commits) != 30 {
		t.Fatalf("records=%d bad=%d commits=%d, want 30/0/30", stats.Records, stats.BadRecords, len(commits))
	}
	first := commits[0]
	if first.Hash != "43587b32af68d8a4ef15b7d849a8918a8f5ea027" {
		t.Errorf("hash = %q", first.Hash)
	}
	if first.AuthorName != "Dev A" || first.AuthorEmail != "dev1@example.com" {
		t.Errorf("author = %q <%q>", first.AuthorName, first.AuthorEmail)
	}
	if got := first.AuthorTime.UTC().Format("2006-01-02T15:04:05"); got != "2026-07-28T14:57:37" {
		t.Errorf("author time UTC = %s", got)
	}
	if first.Subject != "fix(report): anonymized subject 1" {
		t.Errorf("subject = %q", first.Subject)
	}
	if first.Additions() != 250 || first.Deletions() != 11 || len(first.Files) != 7 {
		t.Errorf("additions=%d deletions=%d files=%d, want 250/11/7", first.Additions(), first.Deletions(), len(first.Files))
	}
	if first.Files[1].Path != "d1/src/app/d4/d5/f6.ts" {
		t.Errorf("file path = %q", first.Files[1].Path)
	}

	merges := 0
	for _, c := range commits {
		if c.IsMerge() {
			merges++
		}
	}
	if merges != 7 {
		t.Errorf("merges = %d, want 7", merges)
	}

	last := commits[len(commits)-1]
	binary := 0
	for _, f := range last.Files {
		if f.Binary {
			binary++
		}
	}
	if binary != 16 || len(last.Files) != 48 {
		t.Errorf("binary=%d files=%d, want 16/48", binary, len(last.Files))
	}
}

func TestDetectAgentsFromRealTrailers(t *testing.T) {
	commits, _ := parseFixture(t, "akcie_anon.log")
	counts := map[string]int{}
	for _, c := range commits {
		for _, a := range DetectAgents(c) {
			counts[a]++
		}
	}
	want := map[string]int{"claude": 15, "copilot": 2}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("agent counts = %v, want %v", counts, want)
	}
}

func TestParseEdgeCases(t *testing.T) {
	commits, stats := parseFixture(t, "edge.log")

	if stats.Records != 4 || stats.BadRecords != 2 || len(commits) != 2 {
		t.Fatalf("records=%d bad=%d commits=%d, want 4/2/2", stats.Records, stats.BadRecords, len(commits))
	}
	if stats.BadLines != 1 {
		t.Errorf("bad numstat lines = %d, want 1", stats.BadLines)
	}

	bot := commits[0]
	if got := DetectAgents(bot); !reflect.DeepEqual(got, []string{"codex", "copilot"}) {
		t.Errorf("agents = %v, want [codex copilot]", got)
	}
	if len(bot.Trailers) != 2 || bot.Trailers[1].Key != "Signed-off-by" {
		t.Errorf("trailers = %+v", bot.Trailers)
	}

	revert := commits[1]
	if len(revert.Files) != 1 || revert.Additions() != 7 {
		t.Errorf("revert files=%d additions=%d, want 1/7", len(revert.Files), revert.Additions())
	}
}

func TestClassifyType(t *testing.T) {
	cases := []struct {
		subject string
		merge   bool
		want    string
	}{
		{"feat: add x", false, "feat"},
		{"fix(report): y", false, "fix"},
		{"feat!: breaking", false, "feat"},
		{"refactor(core)!: z", false, "refactor"},
		{"Feat: capitalised", false, "feat"},
		{"Revert \"feat: y\"", false, "revert"},
		{"Merge branch 'main'", true, "merge"},
		{"wip: unknown type", false, "other"},
		{"plain message", false, "other"},
		{"fix:missing space", false, "fix"},
	}
	for _, tc := range cases {
		if got := ClassifyType(tc.subject, tc.merge); got != tc.want {
			t.Errorf("ClassifyType(%q) = %q, want %q", tc.subject, got, tc.want)
		}
	}
}

func TestModuleDir(t *testing.T) {
	cases := map[string]string{
		"main.go":                "(root)",
		"src/a.go":               "src",
		"d1/src/app/d4/d5/f6.ts": "d1/src",
		"docs/README.md":         "docs",
	}
	for path, want := range cases {
		if got := ModuleDir(path, 2); got != want {
			t.Errorf("ModuleDir(%q) = %q, want %q", path, got, want)
		}
	}
}
