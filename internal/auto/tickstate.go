package auto

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/smykla-skalski/bilgie/internal/fsx"
)

// State files next to runs.jsonl. They are separate so a tick (which only
// touches the tick file) never races the pass holder (which owns the pass
// file under the run lock). Both are replaced atomically.
const (
	TickStateName = "tick.json"
	PassStateName = "pass.json"
)

// ErrBadState marks a state file that cannot be used: corrupt content, a read
// error, or something that is not a regular file. The readers return the
// empty state with it so callers decide how loudly to heal.
var ErrBadState = errors.New("unusable state file")

// Sample is one free-space reading kept for the trend forecast.
type Sample struct {
	Time time.Time `json:"time"`
	Free int64     `json:"free_bytes"`
}

// TickState is what the previous tick left behind.
type TickState struct {
	Time    time.Time `json:"time"`
	Tier    string    `json:"tier"`
	Action  string    `json:"action"`
	Reason  string    `json:"reason"`
	Samples []Sample  `json:"samples"`
	Free    int64     `json:"free_bytes"`
	Total   int64     `json:"total_bytes"`
	// SwapUsed is the swap in use at this tick; SwapNotified is when the
	// last swap notification went out. The tick owns both, so they stay out
	// of pass.json, which the pass holder writes under the run lock.
	SwapUsed     int64     `json:"swap_used_bytes,omitempty"`
	SwapNotified time.Time `json:"swap_notified,omitzero"`
}

// NotifyRecord remembers the last notification of one tier.
type NotifyRecord struct {
	Time time.Time `json:"time"`
	Free int64     `json:"free_bytes"`
}

// PassState records the last full pass, whoever ran it (tick or auto), and
// the notifications sent, for the cooldowns.
type PassState struct {
	Time     time.Time               `json:"time"`
	Tier     string                  `json:"tier"`
	Notified map[string]NotifyRecord `json:"notified,omitempty"`
	DryRun   bool                    `json:"dry_run"`
}

// maxStateBytes bounds a state file; a real one is a few KiB.
const maxStateBytes = 1 << 20

func readJSON(stateDir, name string, v any) error {
	path := filepath.Join(stateDir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: read %s: %w", ErrBadState, name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrBadState, name)
	}
	data, err := fsx.ReadRegular(path, maxStateBytes)
	if err != nil {
		return fmt.Errorf("%w: read %s: %w", ErrBadState, name, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: %s is corrupt: %w", ErrBadState, name, err)
	}
	return nil
}

// CheckState reports whether the named state file is still unusable, judged
// by the shape the reader expects.
func CheckState(stateDir, name string) error {
	if name == TickStateName {
		_, err := ReadTickState(stateDir)
		return err
	}
	_, err := ReadPassState(stateDir)
	return err
}

// QuarantineState moves a bad state file (or directory) aside to
// <name>.bad so the next write can replace it, and returns the new path.
func QuarantineState(stateDir, name string) (string, error) {
	from := filepath.Join(stateDir, name)
	to := from + ".bad"
	if err := os.RemoveAll(to); err != nil {
		return "", err
	}
	return to, os.Rename(from, to)
}

func writeJSON(stateDir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	return writeFileAtomic(filepath.Join(stateDir, name), data)
}

// ReadTickState loads the tick state. A missing file is the empty state. A
// bad file also returns the empty state, with an error wrapping ErrBadState.
func ReadTickState(stateDir string) (TickState, error) {
	var s TickState
	if err := readJSON(stateDir, TickStateName, &s); err != nil {
		return TickState{}, err
	}
	return s, nil
}

// WriteTickState stores the tick state.
func WriteTickState(stateDir string, s TickState) error {
	return writeJSON(stateDir, TickStateName, s)
}

// ReadPassState loads the pass state, with the same contract as ReadTickState.
func ReadPassState(stateDir string) (PassState, error) {
	var s PassState
	if err := readJSON(stateDir, PassStateName, &s); err != nil {
		return PassState{}, err
	}
	return s, nil
}

// WritePassState stores the pass state.
func WritePassState(stateDir string, s PassState) error {
	return writeJSON(stateDir, PassStateName, s)
}
