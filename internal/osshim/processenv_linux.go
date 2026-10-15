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
	"time"
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
		var raw []byte
		skip, err := readProc(base, selfUID, func() (readErr error) {
			raw, readErr = os.ReadFile(filepath.Join(base, "environ"))
			return readErr
		})
		if err != nil {
			return nil, fmt.Errorf("read environment of process %s: %w", pid, err)
		}
		if skip {
			continue
		}
		num, _ := strconv.Atoi(pid)
		rows = append(rows, ProcessEnv{PID: num, Text: string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '}))})
	}
	return rows, nil
}

// readProc runs read on a process directory. A permission error on a process
// of the caller's own user is retried, since a process mid-exec is briefly
// unreadable, and then reported so the caller fails closed. A process that
// vanished, is dead or belongs to another user is skipped.
func readProc(base string, selfUID int, read func() error) (skip bool, err error) {
	err = read()
	other := !ownedBy(base, selfUID)
	for attempt := 0; attempt < inspectRetries && !other && errors.Is(err, os.ErrPermission) && !processIsDead(base); attempt++ {
		time.Sleep(inspectRetryDelay)
		err = read()
	}
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ESRCH):
		return true, nil
	case errors.Is(err, os.ErrPermission) && (other || processIsDead(base)):
		return true, nil
	default:
		return false, err
	}
}

// ownedBy reports whether the process belongs to uid. A process whose
// directory is root-owned because it is non-dumpable still counts as ours
// when its status says so; an unknown owner counts as ours so reads fail
// closed.
func ownedBy(base string, uid int) bool {
	info, err := os.Stat(base)
	if err != nil {
		return true
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == uid {
		return true
	}
	return statusUIDMatches(base, uid)
}
