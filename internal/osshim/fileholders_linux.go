//go:build linux

package osshim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
		var fds []os.DirEntry
		skip, err := readProc(base, selfUID, func() (readErr error) {
			fds, readErr = os.ReadDir(filepath.Join(base, "fd"))
			return readErr
		})
		if err != nil {
			return nil, fmt.Errorf("inspect process %s: %w", pid, err)
		}
		if skip {
			continue
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
