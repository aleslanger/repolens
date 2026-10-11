package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	// maxLogLines bounds the log kept for the browser; older lines are dropped from the front.
	maxLogLines = 5000
	// maxLineBytes bounds an unterminated line; longer output is flushed as a line of its own.
	maxLineBytes = 64 << 10
)

// RunState is what the dashboard shows about the in-process collection.
type RunState struct {
	Running    bool   `json:"running"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	Error      string `json:"error,omitempty"`
	Lines      int    `json:"lines"`
}

// runner executes at most one collection at a time and keeps its log for streaming.
type runner struct {
	base    context.Context
	collect func(context.Context, io.Writer) error

	mu      sync.Mutex
	st      RunState
	lines   []string
	dropped int
	partial []byte
	changed chan struct{} // closed and replaced whenever lines or state change
	done    sync.WaitGroup
}

func newRunner(base context.Context, collect func(context.Context, io.Writer) error) *runner {
	return &runner{base: base, collect: collect, changed: make(chan struct{})}
}

func (r *runner) state() RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.st
	s.Lines = r.dropped + len(r.lines)
	return s
}

// start launches a collection unless one is running.
func (r *runner) start() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st.Running {
		return false
	}
	r.st = RunState{Running: true, StartedAt: time.Now().Unix()}
	r.lines, r.dropped, r.partial = nil, 0, nil
	r.notifyLocked()
	r.done.Add(1)
	go r.run()
	return true
}

// wait blocks until a running collection has finished (server shutdown).
func (r *runner) wait() { r.done.Wait() }

func (r *runner) run() {
	defer r.done.Done()
	err := r.collectSafely()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.partial) > 0 {
		r.appendLocked(string(r.partial))
		r.partial = nil
	}
	r.st.Running = false
	r.st.FinishedAt = time.Now().Unix()
	if err != nil {
		r.st.Error = err.Error()
	}
	r.notifyLocked()
}

// collectSafely turns a panic in the collector into an error of the run instead of a crash of
// the whole server.
func (r *runner) collectSafely() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("sběr spadl: %v", p)
		}
	}()
	return r.collect(r.base, r)
}

// Write implements io.Writer for the collector's log, splitting it into lines.
func (r *runner) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf := append(r.partial, p...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		r.appendLocked(string(buf[:i]))
		buf = buf[i+1:]
	}
	if len(buf) >= maxLineBytes {
		r.appendLocked(string(buf))
		buf = nil
	}
	r.partial = append([]byte(nil), buf...)
	r.notifyLocked()
	return len(p), nil
}

func (r *runner) appendLocked(line string) {
	r.lines = append(r.lines, strings.ReplaceAll(line, "\r", " "))
	if over := len(r.lines) - maxLogLines; over > 0 {
		r.lines = append([]string(nil), r.lines[over:]...)
		r.dropped += over
	}
}

func (r *runner) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// since returns log lines from absolute index next on (dropped lines count), the absolute index
// after them, the final state once the run has finished, and a channel closed on the next change.
func (r *runner) since(next int) (lines []string, end int, done *RunState, changed <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end = r.dropped + len(r.lines)
	from := max(next-r.dropped, 0)
	if from < len(r.lines) {
		lines = append([]string(nil), r.lines[from:]...)
	}
	if !r.st.Running && r.st.StartedAt != 0 {
		s := r.st
		s.Lines = r.dropped + len(r.lines)
		done = &s
	}
	return lines, end, done, r.changed
}
