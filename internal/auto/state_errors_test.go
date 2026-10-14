package auto

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateDir_IsUnderTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "elsewhere"))

	dir, err := StateDir()

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".local", "state", stateDirName), dir, "XDG_STATE_HOME is ignored on purpose")
}

func TestMarkFirstRunPending_FailsWhenTheStateDirIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	err := MarkFirstRunPending(blocker)

	require.ErrorContains(t, err, "create state dir")
	assert.True(t, FirstRunPending(blocker), "an unreadable marker location counts as pending")
}

func TestMarkFirstRunPending_FailsWhenTheMarkerPathIsADirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, firstRunMarker), 0o750))

	err := MarkFirstRunPending(dir)

	require.ErrorContains(t, err, "write first-run marker")
}

func TestClearFirstRun_FailsWhenTheMarkerCannotBeRemoved(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, firstRunMarker)
	require.NoError(t, os.MkdirAll(filepath.Join(marker, "inner"), 0o750))

	err := ClearFirstRun(dir)

	require.ErrorContains(t, err, "remove first-run marker")
	assert.True(t, FirstRunPending(dir), "a marker that stays keeps deletion off")
}

func TestAcquireRunLock_FailsWhenTheStateDirIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	lock, ok, err := AcquireRunLock(blocker)

	require.ErrorContains(t, err, "create state dir")
	assert.False(t, ok)
	assert.Nil(t, lock)
}

func TestAcquireRunLock_FailsWhenTheLockPathIsADirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, runLockFileName), 0o750))

	lock, ok, err := AcquireRunLock(dir)

	require.ErrorContains(t, err, "lock run lock")
	assert.False(t, ok)
	assert.Nil(t, lock)
}

func TestRunLock_ReleaseIsSafeOnNilAndEmptyLocks(t *testing.T) {
	var nilLock *RunLock
	assert.NotPanics(t, func() { nilLock.Release() })
	assert.NotPanics(t, func() { (&RunLock{}).Release() })
}
