package auto

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/smykla-skalski/bilgie/internal/appname"
	"github.com/smykla-skalski/bilgie/internal/fsx"
	"github.com/smykla-skalski/bilgie/internal/osshim"
)

const (
	stateDirName    = appname.Name
	firstRunMarker  = "first-run-pending"
	runLockFileName = "auto.lock"
)

// StateDir returns ~/.local/state/bilgie. It ignores XDG_STATE_HOME on
// purpose: install-agent runs in a shell and the agent under launchd, and both
// must find the same marker.
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", stateDirName), nil
}

// MarkFirstRunPending records that the next auto run must be a dry-run.
func MarkFirstRunPending(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	if err := fsx.WriteFileNoLink(filepath.Join(stateDir, firstRunMarker), []byte("dry-run pending\n"), 0o600); err != nil {
		return fmt.Errorf("write first-run marker: %w", err)
	}
	return nil
}

// FirstRunPending reports whether the next auto run must be a dry-run. A
// marker that cannot be read, or a link left in its place, counts as pending:
// the safe answer is no deletion.
func FirstRunPending(stateDir string) bool {
	_, err := os.Lstat(filepath.Join(stateDir, firstRunMarker))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// ClearFirstRun removes the marker once the dry-run has happened.
func ClearFirstRun(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, firstRunMarker))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove first-run marker: %w", err)
	}
	return nil
}

// RunLock is an exclusive lock that keeps two auto runs from overlapping.
type RunLock struct {
	release func()
}

// AcquireRunLock takes the lock without blocking. The bool is false when
// another run holds it.
func AcquireRunLock(stateDir string) (*RunLock, bool, error) {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, false, fmt.Errorf("create state dir: %w", err)
	}
	release, acquired, err := osshim.TryLock(filepath.Join(stateDir, runLockFileName))
	if err != nil {
		return nil, false, fmt.Errorf("lock run lock: %w", err)
	}
	if !acquired {
		return nil, false, nil
	}
	return &RunLock{release: release}, true, nil
}

// Release drops the lock.
func (l *RunLock) Release() {
	if l == nil || l.release == nil {
		return
	}
	l.release()
}

// LinkedState lists the files bilgie writes in stateDir that are symbolic
// links. Writers refuse to write through them, so each one needs the user to
// remove it.
func LinkedState(stateDir string) []string {
	return linked(filepath.Join(stateDir, firstRunMarker), filepath.Join(stateDir, RunLogName))
}

func linked(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
			out = append(out, p)
		}
	}
	return out
}
