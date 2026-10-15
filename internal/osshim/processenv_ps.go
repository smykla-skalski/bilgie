//go:build !linux && !windows

package osshim

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// ProcessEnvironments returns the environment of every process the user may
// inspect, listed with "ps eww": the environment follows the command line.
// Another user's processes show no environment, which only matters for a
// cache they do not own.
func ProcessEnvironments(ctx context.Context) ([]ProcessEnv, error) {
	out, err := exec.CommandContext(ctx, "ps", "eww", "-ax", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parsePSEnv(string(bytes.TrimSpace(out))), nil
}

// parsePSEnv splits "pid command env..." rows. A line that does not start with
// a pid continues the previous row (an environment value may hold a newline),
// so no text is dropped.
func parsePSEnv(out string) []ProcessEnv {
	var rows []ProcessEnv
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if pid, err := strconv.Atoi(fields[0]); err == nil {
				rows = append(rows, ProcessEnv{PID: pid, Text: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0]))})
				continue
			}
		}
		if len(rows) > 0 {
			rows[len(rows)-1].Text += "\n" + line
			continue
		}
		rows = append(rows, ProcessEnv{Text: line})
	}
	return rows
}
