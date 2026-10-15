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
	"strings"
	"syscall"
	"time"
)

// clockTicks is the kernel's USER_HZ, which /proc reports process start in.
// It is 100 on every Linux architecture Go supports.
const clockTicks = 100

// startTimeField is the index of field 22 (starttime) among the stat fields
// that follow the command name, which begin with field 3 (state).
const startTimeField = 19

func processStart(ctx context.Context, pid int) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	return processStartFromProc("/proc", pid)
}

// statFieldsAfterName returns the fields of a stat line that follow the
// command name. The name may hold spaces and parentheses, so the fields start
// behind the last closing parenthesis.
func statFieldsAfterName(stat []byte) ([]string, bool) {
	end := bytes.LastIndex(stat, []byte(") "))
	if end < 0 {
		return nil, false
	}
	return strings.Fields(string(stat[end+2:])), true
}

// processStartFromProc reads the start of pid below a /proc-style root from
// its stat file, in ticks after boot. A zombie counts as gone.
func processStartFromProc(root string, pid int) (time.Time, bool, error) {
	stat, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}

	fields, ok := statFieldsAfterName(stat)
	if !ok || len(fields) <= startTimeField {
		return time.Time{}, false, fmt.Errorf("parse %s/%d/stat", root, pid)
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return time.Time{}, false, nil
	}
	ticks, err := strconv.ParseInt(fields[startTimeField], 10, 64)
	if err != nil || ticks < 0 {
		return time.Time{}, false, fmt.Errorf("parse start of %s/%d/stat", root, pid)
	}

	boot := bootTime(root)
	if boot.IsZero() {
		return time.Time{}, false, fmt.Errorf("btime missing from %s/stat", root)
	}
	return boot.Add(time.Duration(ticks/clockTicks) * time.Second), true, nil
}
