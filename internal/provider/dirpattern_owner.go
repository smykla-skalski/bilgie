package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ownerStartTolerance is how far the start time in a marker may differ from
// the start time the OS reports for the process, which is only known to the
// second. A reused pid starts far later than the owner it replaced.
const ownerStartTolerance = 5 * time.Second

// ownerFileLimit caps the bytes read from an owner marker.
const ownerFileLimit = 256

// ownerFormat is one owner marker a scratch root may carry: a file directly
// in the root that names the process that owns it.
type ownerFormat struct {
	file  string
	parse func(text string) (pid int, start time.Time, err error)
}

// ownerFormats lists the supported markers. Sail writes ".sail-scratch-root"
// holding the owner's pid and start time as unix seconds, "<pid> <start>".
var ownerFormats = []ownerFormat{
	{file: ".sail-scratch-root", parse: parsePidStart},
}

// parsePidStart parses "<pid> <unix seconds>", the content of a Sail marker.
func parsePidStart(text string) (int, time.Time, error) {
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return 0, time.Time{}, errors.New("want \"<pid> <start unix seconds>\"")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 || pid > math.MaxInt32 {
		return 0, time.Time{}, fmt.Errorf("invalid pid %q", fields[0])
	}
	start, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || start <= 0 {
		return 0, time.Time{}, fmt.Errorf("invalid start time %q", fields[1])
	}
	return pid, time.Unix(start, 0), nil
}

// hasOwnerMarker reports whether dir holds a file named like an owner marker.
// It does not judge the content.
func hasOwnerMarker(dir string) bool {
	for _, f := range ownerFormats {
		if _, err := os.Lstat(filepath.Join(dir, f.file)); err == nil {
			return true
		}
	}
	return false
}

// ownerReason returns why the owner markers of dir block its removal, or "".
// A marker whose owner runs, whose owner cannot be checked, or that cannot be
// read or parsed blocks: only a marker that names an exited owner lets the
// other checks decide.
func (p *DirPatternProvider) ownerReason(ctx context.Context, dir string) string {
	for _, f := range ownerFormats {
		if reason := p.markerReason(ctx, dir, f); reason != "" {
			return reason
		}
	}
	return ""
}

func (p *DirPatternProvider) markerReason(ctx context.Context, dir string, f ownerFormat) string {
	path := filepath.Join(dir, f.file)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return ""
	}
	if err != nil {
		return "owner marker unreadable: " + err.Error()
	}
	if !info.Mode().IsRegular() {
		return "owner marker " + f.file + " is not a regular file"
	}

	text, err := readOwnerMarker(path)
	if err != nil {
		return "owner marker unreadable: " + err.Error()
	}
	pid, started, err := f.parse(text)
	if err != nil {
		return "owner marker " + f.file + " malformed: " + err.Error()
	}

	start, running, err := p.ownerStart(ctx, pid)
	switch {
	case err != nil:
		return fmt.Sprintf("owner pid %d cannot be checked: %v", pid, err)
	case !running:
		return ""
	case start.IsZero() || start.Sub(started).Abs() <= ownerStartTolerance:
		return fmt.Sprintf("owner pid %d is running", pid)
	}
	return ""
}

func readOwnerMarker(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, ownerFileLimit+1))
	if err != nil {
		return "", err
	}
	if len(data) > ownerFileLimit {
		return "", errors.New("too large")
	}
	return string(data), nil
}
