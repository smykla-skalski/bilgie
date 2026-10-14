//go:build !windows

package osshim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// AnyFileOpen reports whether any process has one of the named files open,
// using lsof. Paths that do not exist are skipped. An error means the answer
// is unknown and must count as open.
func AnyFileOpen(ctx context.Context, paths []string) (bool, error) {
	var existing []string
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return false, nil
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return false, errors.New("lsof not found")
	}

	out, err := exec.CommandContext(ctx, lsof, append([]string{"-t", "--"}, existing...)...).Output()
	if len(strings.TrimSpace(string(out))) > 0 {
		return true, nil
	}
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
			return false, fmt.Errorf("lsof could not inspect every file: %s", msg)
		}
		return false, nil
	}
	return false, err
}
