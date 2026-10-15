//go:build !linux && !windows

package osshim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// FileHolders returns the pids of the processes that have path open, from
// lsof. A path that does not exist has no holders. An error means the answer
// is unknown.
func FileHolders(ctx context.Context, path string) ([]int, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil, errors.New("lsof not found")
	}
	out, err := exec.CommandContext(ctx, lsof, "-t", "--", path).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, err
		}
		if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
			return nil, fmt.Errorf("lsof could not inspect every file: %s", msg)
		}
	}
	return parsePIDs(string(out))
}

// parsePIDs reads one pid per line.
func parsePIDs(out string) ([]int, error) {
	var pids []int
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("unexpected lsof output %q", line)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}
