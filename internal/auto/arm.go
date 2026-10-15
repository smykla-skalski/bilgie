package auto

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// What the next real pass does in each mode, worded once for doctor, status
// and the arm and disarm commands.
const (
	ArmedDetail    = "the next real pass only previews and deletes nothing"
	DisarmedDetail = "the next real pass deletes"
)

// ErrPassRunning is returned by Arm and Disarm while a pass holds the run
// lock: flipping the marker under a running pass would race its first-run
// decision.
var ErrPassRunning = errors.New("a pass is running and holds the run lock")

// Arm writes the first-run marker, so the next real pass only previews. It
// takes the run lock first, so it cannot race a forced pass, and reports
// whether the state changed. A marker that is a link is refused, as at install.
func Arm(stateDir string) (bool, error) {
	lock, ok, err := AcquireRunLock(stateDir)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrPassRunning
	}
	defer lock.Release()

	was := FirstRunPending(stateDir)
	if err := MarkFirstRunPending(stateDir); err != nil {
		return false, err
	}
	return !was, nil
}

// Disarm clears the first-run marker, so the next real pass deletes. It takes
// the run lock first and reports whether the state changed. A marker that is a
// link is left for the user to remove, the same way bilgie never writes through one.
func Disarm(stateDir string) (bool, error) {
	lock, ok, err := AcquireRunLock(stateDir)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrPassRunning
	}
	defer lock.Release()

	marker := filepath.Join(stateDir, firstRunMarker)
	if info, statErr := os.Lstat(marker); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s is a symbolic link: bilgie refuses to remove it; delete the link yourself", marker)
	}
	was := FirstRunPending(stateDir)
	if err := ClearFirstRun(stateDir); err != nil {
		return false, err
	}
	return was, nil
}
