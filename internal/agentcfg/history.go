package agentcfg

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type History struct {
	First, Last time.Time
	Changes     int
}

// GitHistory reads when each agent-config path was added and changed, in one git call.
// A repository without commits has no history, which is not an error.
func GitHistory(ctx context.Context, repoDir string) (map[string]History, error) {
	if exec.CommandContext(ctx, "git", "-C", repoDir, "-c", "core.fsmonitor=false", "rev-parse", "--verify", "-q", "HEAD").Run() != nil {
		return map[string]History{}, nil
	}
	args := append([]string{"-C", repoDir, "-c", "core.quotepath=off", "-c", "core.fsmonitor=false", "log", "--no-renames", "--format=%x1e%at", "--name-only", "--"}, projectRoots...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log config in %s: %w: %s", repoDir, err, strings.TrimSpace(stderr.String()))
	}
	return parseHistory(out), nil
}

// parseHistory reads `--format=%x1e%at --name-only` output (newest commit first).
func parseHistory(out []byte) map[string]History {
	hist := map[string]History{}
	for rec := range bytes.SplitSeq(out, []byte{0x1e}) {
		sc := bufio.NewScanner(bytes.NewReader(rec))
		var when time.Time
		first := true
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if first {
				first = false
				sec, err := strconv.ParseInt(line, 10, 64)
				if err != nil {
					break // malformed record: skip it
				}
				when = time.Unix(sec, 0)
				continue
			}
			h := hist[line]
			if h.Last.IsZero() || when.After(h.Last) {
				h.Last = when
			}
			if h.First.IsZero() || when.Before(h.First) {
				h.First = when
			}
			h.Changes++
			hist[line] = h
		}
	}
	return hist
}
