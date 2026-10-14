//go:build !linux && !windows

package osshim

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProcessTable returns every running process, listed with ps.
func ProcessTable(ctx context.Context) ([]Process, error) {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,etime=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parsePS(string(bytes.TrimSpace(out)), time.Now()), nil
}

// parsePS parses "pid ppid etime command" rows. A row it cannot parse keeps
// its text as the command line, so it still counts toward a busy match. An
// elapsed time it cannot parse leaves Started zero, which reads as unknown.
func parsePS(out string, now time.Time) []Process {
	var procs []Process
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			pid, errPID := strconv.Atoi(fields[0])
			ppid, errPPID := strconv.Atoi(fields[1])
			if errPID == nil && errPPID == nil {
				rest := strings.TrimSpace(line)
				for _, f := range fields[:3] {
					rest = strings.TrimSpace(strings.TrimPrefix(rest, f))
				}
				proc := Process{PID: pid, PPID: ppid, CommandLine: rest, Args: rest}
				if elapsed, ok := parseElapsed(fields[2]); ok {
					proc.Started = now.Add(-elapsed)
				}
				procs = append(procs, proc)
				continue
			}
		}
		text := strings.TrimSpace(line)
		procs = append(procs, Process{CommandLine: text, Args: text})
	}
	return procs
}

// parseElapsed parses ps etime output: [[dd-]hh:]mm:ss.
func parseElapsed(s string) (time.Duration, bool) {
	var days int
	if d, rest, found := strings.Cut(s, "-"); found {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var seconds int64
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return 0, false
		}
		seconds = seconds*60 + int64(n)
	}
	return time.Duration(days)*24*time.Hour + time.Duration(seconds)*time.Second, true
}
