//go:build !linux && !windows

package osshim

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// processStart asks ps for the elapsed time of pid. Elapsed time, unlike
// lstart, does not depend on the time zone, so a daylight saving change never
// moves the start.
func processStart(ctx context.Context, pid int) (time.Time, bool, error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "etime=", "-p", strconv.Itoa(pid)).Output()
	text := strings.TrimSpace(string(out))
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && text == "" {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	elapsed, err := parseEtime(text)
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Now().Add(-elapsed).Truncate(time.Second), true, nil
}

// parseEtime parses ps elapsed time: [[dd-]hh:]mm:ss.
func parseEtime(text string) (time.Duration, error) {
	bad := fmt.Errorf("parse etime %q", text)

	var days int64
	if d, rest, ok := strings.Cut(text, "-"); ok {
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil || n < 0 {
			return 0, bad
		}
		days, text = n, rest
	}

	parts := strings.Split(text, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, bad
	}
	hms := [3]int64{}
	for i, part := range parts {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n < 0 {
			return 0, bad
		}
		hms[3-len(parts)+i] = n
	}
	secs := ((days*24+hms[0])*60+hms[1])*60 + hms[2]
	return time.Duration(secs) * time.Second, nil
}
