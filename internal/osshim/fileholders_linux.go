//go:build linux

package osshim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// FileHolders returns the pids of the processes that have path open, read
// from /proc/<pid>/fd. A path that does not exist has no holders. An
// uninspectable process of the caller's own user is an error.
func FileHolders(ctx context.Context, path string) ([]int, error) {
	return holdersFromProc(ctx, "/proc", path, os.Getuid())
}

func holdersFromProc(ctx context.Context, root, path string, selfUID int) ([]int, error) {
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pids, err := pidDirs(root)
	if err != nil {
		return nil, err
	}
	var holders []int
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		base := filepath.Join(root, pid)
		fds, err := os.ReadDir(filepath.Join(base, "fd"))
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ESRCH):
			continue
		case errors.Is(err, os.ErrPermission) && !ownedBy(base, selfUID):
			continue
		default:
			return nil, fmt.Errorf("inspect process %s: %w", pid, err)
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
			if err == nil && link == target {
				num, _ := strconv.Atoi(pid)
				holders = append(holders, num)
				break
			}
		}
	}
	return holders, nil
}
