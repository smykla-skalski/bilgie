//go:build windows

package osshim

import (
	"context"
	"errors"
	"math"
	"time"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a live process.
const stillActive = 259

func processStart(ctx context.Context, pid int) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	if pid <= 0 || pid > math.MaxUint32 {
		return time.Time{}, false, errors.New("process id out of range")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	defer func() { _ = windows.CloseHandle(h) }()

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return time.Time{}, false, err
	}
	if code != stillActive {
		return time.Time{}, false, nil
	}
	created, err := handleCreationTime(h)
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(0, created), true, nil
}
