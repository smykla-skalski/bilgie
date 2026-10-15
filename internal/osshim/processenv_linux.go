//go:build linux

package osshim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ProcessEnvironments returns the environment of every process, read from
// /proc/<pid>/environ.
func ProcessEnvironments(ctx context.Context) ([]ProcessEnv, error) {
	return environmentsFromProc(ctx, "/proc", os.Getuid())
}

// environmentsFromProc reads the environment of each process under a
// /proc-style root. A process owned by another user that cannot be read is
// skipped, since the cache is not theirs to hold; an unreadable process of the
// caller's own user is an error so the caller fails closed.
func environmentsFromProc(ctx context.Context, root string, selfUID int) ([]ProcessEnv, error) {
	pids, err := pidDirs(root)
	if err != nil {
		return nil, err
	}
	rows := make([]ProcessEnv, 0, len(pids))
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		base := filepath.Join(root, pid)
		raw, err := os.ReadFile(filepath.Join(base, "environ"))
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ESRCH):
			continue
		case errors.Is(err, os.ErrPermission) && !ownedBy(base, selfUID):
			continue
		default:
			return nil, fmt.Errorf("read environment of process %s: %w", pid, err)
		}
		num, _ := strconv.Atoi(pid)
		rows = append(rows, ProcessEnv{PID: num, Text: string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '}))})
	}
	return rows, nil
}

// ownedBy reports whether the process directory belongs to uid; an unknown
// owner counts as ours so the read fails closed.
func ownedBy(base string, uid int) bool {
	info, err := os.Stat(base)
	if err != nil {
		return true
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == uid
}
