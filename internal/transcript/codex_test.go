package transcript

import (
	"reflect"
	"testing"
	"time"
)

func parseCodex(t *testing.T, name string) (*Session, FileResult) {
	t.Helper()
	res, err := ParseCodexRollout("testdata/codex/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 (issues %v)", len(res.Sessions), res.Issues)
	}
	return res.Sessions[0], res
}

// Expected values were computed independently from the fixtures with a Python script.
func TestCodexRealRollouts(t *testing.T) {
	cases := []struct {
		file, id, cwd, start, end string
		turns                     int
		activeMs                  int64
		model                     ModelUsage
		tools, errors, denials    map[string]int
	}{
		{
			file: "rollout-a.jsonl", id: "019ea545-af8a-7642-8f88-5b0034dfa808", cwd: "/home/user/projects/demo/admin-all",
			start: "2026-06-08T03:28:09.533Z", end: "2026-06-08T14:29:11.883Z", turns: 3, activeMs: 2498337,
			model:   ModelUsage{Requests: 123, Input: 651062, Output: 53823, CacheRead: 15462528, Reasoning: 13482},
			tools:   map[string]int{"exec_command": 169, "update_plan": 5, "write_stdin": 38, "apply_patch": 25},
			errors:  map[string]int{"apply_patch": 1},
			denials: map[string]int{},
		},
		{
			file: "rollout-b.jsonl", id: "019e88ae-215d-7be1-b934-e5225988119a", cwd: "/home/user/projects/demo",
			start: "2026-06-02T14:13:34.551Z", end: "2026-06-03T14:17:53.388Z", turns: 4, activeMs: 2022493,
			model:   ModelUsage{Requests: 81, Input: 801427, Output: 23393, CacheRead: 9738624, Reasoning: 6255},
			tools:   map[string]int{"exec_command": 153, "web_search": 1, "apply_patch": 8, "write_stdin": 11},
			errors:  map[string]int{},
			denials: map[string]int{"interrupted": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			s, res := parseCodex(t, tc.file)
			if n := res.Issues.Count(IssueError) + res.Issues.Count(IssueUnrecognized); n != 0 {
				t.Errorf("issues on a real rollout: %v", res.Issues)
			}
			if s.ID != tc.id || s.CWD != tc.cwd || s.Tool != "codex" || s.Parent != "" {
				t.Errorf("id=%q cwd=%q tool=%q parent=%q", s.ID, s.CWD, s.Tool, s.Parent)
			}
			if got := s.Started.UTC().Format(time.RFC3339Nano); got != tc.start {
				t.Errorf("start = %s", got)
			}
			if got := s.Ended.UTC().Format(time.RFC3339Nano); got != tc.end {
				t.Errorf("end = %s", got)
			}
			if s.Turns != tc.turns || s.ActiveMs != tc.activeMs {
				t.Errorf("turns=%d active=%d", s.Turns, s.ActiveMs)
			}
			if len(s.Models) != 1 || s.Models["gpt-5.5"] == nil || *s.Models["gpt-5.5"] != tc.model {
				t.Errorf("models = %v, want gpt-5.5 %+v", s.Models, tc.model)
			}
			for kind, want := range map[string]map[string]int{KindTool: tc.tools, KindToolError: tc.errors, KindDenial: tc.denials} {
				if got := counters(s, kind); !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %v, want %v", kind, got, want)
				}
			}
		})
	}
}

func TestCodexEdgeCases(t *testing.T) {
	s, res := parseCodex(t, "edge.jsonl")
	wantIssues := Issues{
		{IssueError, "tokeny před turn_context"}:     1,
		{IssueError, "neplatný JSON řádek"}:          1,
		{IssueUnrecognized, "event:brand_new_event"}: 1,
		{IssueUnrecognized, "item:brand_new_item"}:   1,
		{IssueUnrecognized, "type:brand_new_type"}:   1,
	}
	if !reflect.DeepEqual(res.Issues, wantIssues) || res.Records != 13 {
		t.Errorf("records=%d issues=%v", res.Records, res.Issues)
	}
	if s.Parent != "parent-1" {
		t.Errorf("parent = %q", s.Parent)
	}
	// Cached tokens are split out of input; a repeated cumulative value adds nothing.
	if u := s.Models["gpt-6-sol"]; u == nil || *u != (ModelUsage{Requests: 1, Input: 100, CacheRead: 100, Output: 20, Reasoning: 10}) {
		t.Errorf("gpt-6-sol = %+v", u)
	}
	if u := s.Models["(neznámý)"]; u == nil || u.Input != 60 || u.CacheRead != 40 {
		t.Errorf("tokens before turn_context = %+v", u)
	}
	for kind, want := range map[string]map[string]int{
		KindSubagent:  {"codex-subagent": 1},
		KindToolError: {"exec_command": 1},
		KindMCP:       {"playwright": 1},
	} {
		if got := counters(s, kind); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", kind, got, want)
		}
	}
	if s.ActiveMs != 11000 {
		t.Errorf("active = %d, want 11000 from gaps", s.ActiveMs)
	}
}

func TestCodexCounterResetKeepsNewUsage(t *testing.T) {
	p := &codexParser{s: newSession(toolCodex, "x"), issues: Issues{}, model: "gpt-x"}
	for _, tot := range []codexTokens{{Input: 100, Output: 10}, {Input: 150, Output: 20}, {Input: 150, Output: 20}, {Input: 30, Output: 5}, {Input: 40, Output: 9}} {
		p.tokens(&codexPayload{Info: &struct {
			Total codexTokens `json:"total_token_usage"`
		}{tot}})
	}
	// 100+50 before the reset, then 30 (the whole first snapshot after it) and 10 more.
	if u := p.s.Models["gpt-x"]; u == nil || u.Input != 190 || u.Output != 29 || u.Requests != 4 {
		t.Errorf("usage = %+v, want input 190, output 29, 4 requests", u)
	}
}

// A drop in a secondary component (cached tokens recomputed) is not a reset: nothing is counted twice.
func TestCodexSecondaryDropIsNotAReset(t *testing.T) {
	p := &codexParser{s: newSession(toolCodex, "x"), issues: Issues{}, model: "gpt-x"}
	for _, tot := range []codexTokens{{Input: 100, Cached: 60, Output: 10}, {Input: 150, Cached: 50, Output: 20}} {
		p.tokens(&codexPayload{Info: &struct {
			Total codexTokens `json:"total_token_usage"`
		}{tot}})
	}
	// 40 uncached + 60 cached first; then 50 more input and no new cache reads (the drop counts as 0).
	if u := p.s.Models["gpt-x"]; u == nil || u.Input != 90 || u.CacheRead != 60 || u.Output != 20 {
		t.Errorf("usage = %+v, want input 90, cache 60, output 20 (no double count)", u)
	}
}

// Cached tokens are part of input: a step whose cache grew more than its input counts at most
// the input as cached, never a negative uncached input.
func TestCodexCachedNeverExceedsInput(t *testing.T) {
	p := &codexParser{s: newSession(toolCodex, "x"), issues: Issues{}, model: "gpt-x"}
	for _, tot := range []codexTokens{{Input: 100, Cached: 60, Output: 10}, {Input: 110, Cached: 90, Output: 12}} {
		p.tokens(&codexPayload{Info: &struct {
			Total codexTokens `json:"total_token_usage"`
		}{tot}})
	}
	if u := p.s.Models["gpt-x"]; u == nil || u.Input != 40 || u.CacheRead != 70 {
		t.Errorf("usage = %+v, want input 40 and cache 70", u)
	}
}
