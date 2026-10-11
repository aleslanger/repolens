// Package transcript parses coding-agent session logs into aggregates.
// Only names, counts and timestamps leave this package; prompt and output text never does.
package transcript

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"repolens/internal/privacy"
)

// ParserVersion changes whenever parsing results change, so stored transcripts are re-read.
const ParserVersion = "5"

// Counter kinds stored per session.
const (
	KindTool      = "tool"
	KindToolError = "tool_error"
	KindSubagent  = "subagent"
	KindSkill     = "skill"
	KindSlash     = "slash"
	KindDenial    = "denial"
	KindHook      = "hook"
	KindHookBlock = "hook_block"
	KindMCP       = "mcp"
	KindAPIError  = "api_error"
)

// Issue kinds.
const (
	IssueUnrecognized = "unrecognized"
	IssueError        = "error"
)

// idleGap splits activity when no explicit turn durations exist.
const idleGap = 5 * time.Minute

type ModelUsage struct {
	Requests, Input, Output, CacheRead, CacheWrite, Reasoning int64
}

func (m ModelUsage) Total() int64 { return m.Input + m.Output + m.CacheRead + m.CacheWrite }

type CounterKey struct{ Kind, Name string }

type Window struct{ Start, End time.Time }

type Counter struct {
	Count, Errors int
	DurationMs    int64
}

type Session struct {
	Tool     string
	ID       string
	Parent   string // parent session for agent-spawned threads (Codex); empty for top-level sessions
	CWD      string
	Started  time.Time
	Ended    time.Time
	ActiveMs int64
	Turns    int
	Models   map[string]*ModelUsage
	Counters map[CounterKey]*Counter
	// ReportedCost is USD per model as reported by the agent itself (Claude Code cost-state).
	ReportedCost map[string]float64
	// Windows are spans of continuous activity (gaps under idleGap), used to match commits.
	Windows []Window

	cwds       map[string]int
	times      []time.Time
	turnMs     int64
	turnsKnown bool
}

func newSession(tool, id string) *Session {
	return &Session{
		Tool: tool, ID: id,
		Models:       map[string]*ModelUsage{},
		Counters:     map[CounterKey]*Counter{},
		ReportedCost: map[string]float64{},
		cwds:         map[string]int{},
	}
}

// validName limits stored names to identifier-like strings. Fields such as Skill.input.skill are
// written by the model; anything that does not look like a name is never stored verbatim.
var validName = regexp.MustCompile(`^[\p{L}\p{N}_:./@+\-\[\]()| ·*]{1,100}$`)

const invalidName = "(neplatné jméno)"

// SafeName returns name when it looks like an identifier, otherwise a placeholder.
func SafeName(name string) string {
	if validName.MatchString(name) {
		return name
	}
	return invalidName
}

func (s *Session) count(kind, name string, n int) *Counter {
	k := CounterKey{kind, SafeName(name)}
	c := s.Counters[k]
	if c == nil {
		c = &Counter{}
		s.Counters[k] = c
	}
	c.Count += n
	return c
}

func (s *Session) model(name string) *ModelUsage {
	name = SafeName(name)
	m := s.Models[name]
	if m == nil {
		m = &ModelUsage{}
		s.Models[name] = m
	}
	return m
}

func (s *Session) seen(t time.Time, cwd string) {
	if !t.IsZero() {
		s.times = append(s.times, t)
	}
	if cwd != "" {
		s.cwds[cwd]++
	}
}

// finish derives start/end, the dominant cwd and active time.
func (s *Session) finish() {
	if len(s.times) > 0 {
		sort.Slice(s.times, func(i, j int) bool { return s.times[i].Before(s.times[j]) })
		s.Started, s.Ended = s.times[0], s.times[len(s.times)-1]
	}
	best := 0
	for c, n := range s.cwds {
		if n > best || (n == best && c < s.CWD) {
			s.CWD, best = c, n
		}
	}
	if s.turnsKnown {
		s.ActiveMs = s.turnMs
	} else {
		s.ActiveMs = activeFromGaps(s.times)
	}
	s.Windows = windows(s.times)
	s.times, s.cwds = nil, nil
}

// windows merges sorted timestamps into spans separated by pauses of at least idleGap.
func windows(ts []time.Time) []Window {
	var out []Window
	for i, t := range ts {
		if i == 0 || t.Sub(ts[i-1]) >= idleGap {
			out = append(out, Window{Start: t, End: t})
			continue
		}
		out[len(out)-1].End = t
	}
	return out
}

func activeFromGaps(ts []time.Time) int64 {
	var total time.Duration
	for i := 1; i < len(ts); i++ {
		if gap := ts[i].Sub(ts[i-1]); gap < idleGap {
			total += gap
		}
	}
	return total.Milliseconds()
}

// Issues aggregates unrecognized records and errors as {kind, what} -> count.
type Issues map[[2]string]int

// add records an issue; a "prefix:value" what has its value checked like any other name.
func (is Issues) add(kind, what string) {
	if prefix, value, ok := strings.Cut(what, ":"); ok && !strings.Contains(prefix, " ") {
		what = prefix + ":" + SafeName(value)
	}
	is[[2]string{kind, what}]++
}

// Count returns how many issues of a kind were seen.
func (is Issues) Count(kind string) int {
	n := 0
	for k, v := range is {
		if k[0] == kind {
			n += v
		}
	}
	return n
}

type FileResult struct {
	Sessions []*Session
	Records  int
	Issues   Issues
}

// maxLine bounds one JSONL record; real transcripts stay far below it (images are the largest).
// It is a variable so tests can lower it.
var maxLine = 64 << 20

// eachLine streams a JSONL file through the privacy guard, one line at a time. Lines longer
// than maxLine are skipped (onOversize is called) instead of being buffered without bound.
func eachLine(path string, fn func([]byte), onOversize func()) error {
	f, err := privacy.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var buf []byte
	skipping := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !skipping {
			if len(buf)+len(chunk) > maxLine {
				skipping, buf = true, buf[:0]
				onOversize()
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == nil || errors.Is(err, io.EOF):
			if !skipping && len(buf) > 0 {
				fn(buf)
			}
			buf, skipping = buf[:0], false
			if err != nil {
				return nil
			}
		default:
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
