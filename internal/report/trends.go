package report

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sort"
)

// Usage trends, session length and the configuration timeline (agents page and repo detail).

const (
	// trendTop names are kept per kind; the rest is folded into otherName.
	trendTop  = 6
	otherName = "(ostatní)"
	msPerMin  = 60000
)

// trendKinds are the counters whose use over time is shown.
var trendKinds = []string{"skill", "subagent", "tool", "mcp", "slash"}

// Trend is weekly use of the top names of one counter kind. Counts[i][w] belongs to Names[i]
// and Weeks[w] (the Monday that starts the week of the session start).
type Trend struct {
	Weeks  []string `json:"weeks"`
	Names  []string `json:"names"`
	Counts [][]int  `json:"counts"`
}

// DurationBucket is one bar of the session-length histogram; Label is a Czech source label.
type DurationBucket struct {
	Label    string `json:"label"`
	Sessions int    `json:"sessions"`
}

// Durations describes how long sessions are: active time (turn durations, or activity without
// gaps over 5 minutes), not the span between first and last record.
type Durations struct {
	Sessions int              `json:"sessions"`
	MedianMs int64            `json:"medianMs"`
	P90Ms    int64            `json:"p90Ms"`
	Buckets  []DurationBucket `json:"buckets"`
}

// ConfigMonth counts configuration items created in a month and items whose last change fell in
// it (only files with git history have these dates).
type ConfigMonth struct {
	Month   string `json:"month"`
	Kind    string `json:"kind"`
	Created int    `json:"created"`
	Changed int    `json:"changed"`
}

var durationBuckets = []struct {
	label string
	maxMs int64 // exclusive upper bound; the last bucket is open
}{
	{"< 5 min", 5 * msPerMin}, {"5–15 min", 15 * msPerMin}, {"15–30 min", 30 * msPerMin}, {"30–60 min", 60 * msPerMin},
	{"1–2 h", 120 * msPerMin}, {"2–4 h", 240 * msPerMin}, {"> 4 h", math.MaxInt64},
}

// usageTrends groups counters by week of the top-level session start; counters of spawned
// threads count towards their parent.
func usageTrends(ctx context.Context, db *sql.DB, sc scope, v *AgentView) error {
	v.Trends = map[string]Trend{}
	type cell struct {
		week, name string
		count      int
	}
	byKind := map[string][]cell{}
	err := queryEach(ctx, db, `SELECT c.kind, c.name, date(s.started_at,'unixepoch','localtime','-6 days','weekday 1'), SUM(c.count)
		FROM session_counter c JOIN agent_session x ON x.id=c.session_id JOIN agent_session s ON s.id=COALESCE(x.parent_id, x.id)
		WHERE `+sc.where+` AND c.kind IN ('skill','subagent','tool','mcp','slash') GROUP BY 1,2,3`, sc.args, func(r *sql.Rows) error {
		var kind string
		var c cell
		if err := r.Scan(&kind, &c.name, &c.week, &c.count); err != nil {
			return err
		}
		byKind[kind] = append(byKind[kind], c)
		return nil
	})
	if err != nil {
		return fmt.Errorf("usage trends: %w", err)
	}
	for _, kind := range trendKinds {
		cells := byKind[kind]
		if len(cells) == 0 {
			continue
		}
		totals := map[string]int{}
		weekSet := map[string]bool{}
		for _, c := range cells {
			totals[c.name] += c.count
			weekSet[c.week] = true
		}
		names := topNames(totals)
		t := Trend{Weeks: sortedKeys(weekSet), Names: names}
		row := map[string]int{}
		for i, n := range names {
			row[n] = i
		}
		col := map[string]int{}
		for i, w := range t.Weeks {
			col[w] = i
		}
		t.Counts = make([][]int, len(names))
		for i := range t.Counts {
			t.Counts[i] = make([]int, len(t.Weeks))
		}
		for _, c := range cells {
			i, ok := row[c.name]
			if !ok {
				i = row[otherName]
			}
			t.Counts[i][col[c.week]] += c.count
		}
		v.Trends[kind] = t
	}
	return nil
}

// topNames returns the trendTop most used names, then otherName when anything is left over.
func topNames(totals map[string]int) []string {
	names := make([]string, 0, len(totals))
	for n := range totals {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if totals[names[i]] != totals[names[j]] {
			return totals[names[i]] > totals[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) <= trendTop {
		return names
	}
	return append(names[:trendTop:trendTop], otherName)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sessionDurations(ctx context.Context, db *sql.DB, sc scope, v *AgentView) error {
	var ms []int64
	err := queryEach(ctx, db, `SELECT s.active_ms FROM agent_session s WHERE `+sc.where+` ORDER BY s.active_ms`, sc.args, func(r *sql.Rows) error {
		var x int64
		if err := r.Scan(&x); err != nil {
			return err
		}
		ms = append(ms, x)
		return nil
	})
	if err != nil {
		return fmt.Errorf("session durations: %w", err)
	}
	d := Durations{Sessions: len(ms), Buckets: make([]DurationBucket, len(durationBuckets))}
	for i, b := range durationBuckets {
		d.Buckets[i].Label = b.label
	}
	for _, x := range ms {
		for i, b := range durationBuckets {
			if x < b.maxMs {
				d.Buckets[i].Sessions++
				break
			}
		}
	}
	d.MedianMs, d.P90Ms = nearestRank(ms, 0.5), nearestRank(ms, 0.9)
	v.Durations = d
	return nil
}

// nearestRank is the p-th percentile of sorted values by the nearest-rank method.
func nearestRank(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(math.Ceil(p*float64(len(sorted))))-1]
}

func configTimeline(ctx context.Context, db *sql.DB, repoID *int64, v *AgentView) error {
	v.ConfigTimeline = []ConfigMonth{}
	where, args := "1=1", []any{}
	if repoID != nil {
		where, args = "repo_id=?", []any{*repoID}
	}
	return queryEach(ctx, db, `SELECT month, kind, SUM(created), SUM(changed) FROM (
		SELECT strftime('%Y-%m', first_time, 'unixepoch', 'localtime') AS month, kind, 1 AS created, 0 AS changed
			FROM agent_config WHERE first_time IS NOT NULL AND `+where+`
		UNION ALL
		SELECT strftime('%Y-%m', last_time, 'unixepoch', 'localtime'), kind, 0, 1
			FROM agent_config WHERE last_time IS NOT NULL AND last_time != first_time AND `+where+`)
		GROUP BY 1,2 ORDER BY 1,2`, slices.Concat(args, args), func(r *sql.Rows) error {
		var m ConfigMonth
		if err := r.Scan(&m.Month, &m.Kind, &m.Created, &m.Changed); err != nil {
			return err
		}
		v.ConfigTimeline = append(v.ConfigTimeline, m)
		return nil
	})
}
