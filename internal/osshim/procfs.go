//go:build unix

package osshim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A process mid-exec is briefly unreadable even to its owner, so a permission
// error is retried before it is treated as unknowable.
const (
	inspectRetries    = 3
	inspectRetryDelay = 20 * time.Millisecond
)

// pidDirs lists the numeric entries of a /proc-style root. An empty result
// means the root is not a real process table, so the caller must not read it
// as "nothing is running".
func pidDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var pids []string
	for _, e := range entries {
		if _, convErr := strconv.Atoi(e.Name()); convErr == nil && e.IsDir() {
			pids = append(pids, e.Name())
		}
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("no processes found under %s", root)
	}
	return pids, nil
}

// commandLinesFromProc returns one command line per process under a
// /proc-style root.
func commandLinesFromProc(ctx context.Context, root string) ([]string, error) {
	procs, err := processTableFromProc(ctx, root)
	if err != nil {
		return nil, err
	}
	return commandLines(procs), nil
}

// parentPID reads the parent pid from a /proc stat file. The command name
// sits in parentheses and may contain spaces, so parsing starts after the
// last ")". An unreadable file yields 0, which no exclusion rule matches.
func parentPID(base string) int {
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return 0
	}
	idx := strings.LastIndex(string(stat), ") ")
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(string(stat)[idx+2:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// clockTicksPerSecond is the unit of the start time in a /proc stat file. It is
// 100 on every Linux architecture bilgie supports.
const clockTicksPerSecond = 100

// bootTime reads the system boot time from a /proc-style root's stat file;
// zero when it is missing.
func bootTime(root string) time.Time {
	stat, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return time.Time{}
	}
	for line := range strings.SplitSeq(string(stat), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			secs, convErr := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if convErr != nil || secs <= 0 {
				return time.Time{}
			}
			return time.Unix(secs, 0)
		}
	}
	return time.Time{}
}

// startTime is when the process in a /proc stat file began, from its start
// tick (field 22) counted from boot. Zero when either is unknown.
func startTime(base string, boot time.Time) time.Time {
	if boot.IsZero() {
		return time.Time{}
	}
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return time.Time{}
	}
	idx := strings.LastIndex(string(stat), ") ")
	if idx < 0 {
		return time.Time{}
	}
	fields := strings.Fields(string(stat)[idx+2:])
	const startField = 22 - 3
	if len(fields) <= startField {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(fields[startField], 10, 64)
	if err != nil || ticks < 0 {
		return time.Time{}
	}
	return boot.Add(time.Duration(ticks) * time.Second / clockTicksPerSecond)
}

// processTableFromProc returns one entry per process under a /proc-style
// root. The kernel's short name is appended so a process that rewrote its
// argv is still matched; a kernel thread or zombie has no argv and yields
// the name alone.
func processTableFromProc(ctx context.Context, root string) ([]Process, error) {
	pids, err := pidDirs(root)
	if err != nil {
		return nil, err
	}

	boot := bootTime(root)
	lines := make([]Process, 0, len(pids))
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cmdline, err := os.ReadFile(filepath.Join(root, pid, "cmdline"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		comm, err := os.ReadFile(filepath.Join(root, pid, "comm"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}

		args := strings.TrimSpace(strings.ReplaceAll(string(bytes.TrimRight(cmdline, "\x00")), "\x00", " "))
		name := strings.TrimSpace(string(comm))
		num, _ := strconv.Atoi(pid)
		lines = append(lines, Process{
			PID:         num,
			PPID:        parentPID(filepath.Join(root, pid)),
			CommandLine: strings.TrimSpace(args + " " + name),
			Args:        args,
			Started:     startTime(filepath.Join(root, pid), boot),
		})
	}
	return lines, nil
}

// underDir reports whether path is dir or lies inside it.
func underDir(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// procHasOpenFiles reports whether any process under a /proc-style root has a
// file open, a mapping, its working directory, root or executable inside dir.
// A process owned by another user that cannot be inspected is skipped, since
// a cache owned by selfUID is not theirs to hold; an uninspectable process
// of our own user is an error so the caller fails closed.
func procHasOpenFiles(ctx context.Context, root, dir string, selfUID int) (bool, error) {
	found := false
	err := procScan(ctx, root, selfUID, func(paths []string) bool {
		found = slices.ContainsFunc(paths, func(path string) bool { return underDir(path, dir) })
		return found
	})
	return found, err
}

// procScan hands visit the paths each process holds (working directory,
// root, executable, descriptors, mappings) until visit returns true. A process
// owned by another user that cannot be inspected is skipped, since a cache
// owned by selfUID is not theirs to hold; an uninspectable process of our own
// user is an error so the caller fails closed.
func procScan(ctx context.Context, root string, selfUID int, visit func(paths []string) bool) error {
	pids, err := pidDirs(root)
	if err != nil {
		return err
	}

	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return err
		}
		base := filepath.Join(root, pid)
		info, err := os.Stat(base)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		other := false
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			other = int(st.Uid) != selfUID && !statusUIDMatches(base, selfUID)
		}

		paths, err := procPidPaths(base)
		for attempt := 0; attempt < inspectRetries && !other && errors.Is(err, os.ErrPermission) && !processIsDead(base); attempt++ {
			time.Sleep(inspectRetryDelay)
			paths, err = procPidPaths(base)
		}
		switch {
		case err == nil:
			if visit(paths) {
				return nil
			}
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ESRCH):
			continue
		case other && errors.Is(err, os.ErrPermission):
			continue
		case errors.Is(err, os.ErrPermission) && processIsDead(base):
			continue
		default:
			return fmt.Errorf("inspect process %s: %w", pid, err)
		}
	}
	return nil
}

func procPidPaths(base string) ([]string, error) {
	var paths []string
	for _, link := range []string{"cwd", "root", "exe"} {
		target, err := os.Readlink(filepath.Join(base, link))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		paths = append(paths, target)
	}

	fds, err := os.ReadDir(filepath.Join(base, "fd"))
	if err != nil {
		return nil, err
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		paths = append(paths, strings.TrimSuffix(target, " (deleted)"))
	}

	maps, err := os.ReadFile(filepath.Join(base, "maps"))
	if err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(string(maps), "\n") {
		fields := strings.SplitN(line, " ", 6)
		if len(fields) < 6 {
			continue
		}
		paths = append(paths, strings.TrimSuffix(strings.TrimSpace(fields[5]), " (deleted)"))
	}
	return paths, nil
}

// statusUIDMatches reports whether the real or effective uid in the status
// file is selfUID. A non-dumpable process of our own user has a root-owned
// /proc entry yet is still ours, so it must not be skipped when unreadable.
func statusUIDMatches(base string, selfUID int) bool {
	status, err := os.ReadFile(filepath.Join(base, "status"))
	if err != nil {
		return true
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			return true
		}
		for _, f := range fields[:2] {
			if uid, convErr := strconv.Atoi(f); convErr != nil || uid == selfUID {
				return true
			}
		}
		return false
	}
	return true
}

// processIsDead reports a zombie or exiting process: it holds no files, yet
// the kernel denies access to its fd directory.
func processIsDead(base string) bool {
	status, err := os.ReadFile(filepath.Join(base, "status"))
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if state, ok := strings.CutPrefix(line, "State:"); ok {
			state = strings.TrimSpace(state)
			return strings.HasPrefix(state, "Z") || strings.HasPrefix(state, "X")
		}
	}
	return false
}
