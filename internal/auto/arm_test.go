package auto

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArmDisarm_MarkerTransitions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	changed, err := Disarm(dir)
	require.NoError(t, err)
	assert.False(t, changed, "already disarmed")
	assert.False(t, FirstRunPending(dir))

	changed, err = Arm(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, FirstRunPending(dir))

	changed, err = Arm(dir)
	require.NoError(t, err)
	assert.False(t, changed, "already armed")
	assert.True(t, FirstRunPending(dir))

	changed, err = Disarm(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.False(t, FirstRunPending(dir))
}

func TestArmDisarm_RefuseWhileAPassHoldsTheRunLock(t *testing.T) {
	dir := t.TempDir()
	lock, ok, err := AcquireRunLock(dir)
	require.NoError(t, err)
	require.True(t, ok)

	_, err = Arm(dir)
	require.ErrorIs(t, err, ErrPassRunning)
	assert.False(t, FirstRunPending(dir), "a refused arm writes nothing")

	require.NoError(t, MarkFirstRunPending(dir))
	_, err = Disarm(dir)
	require.ErrorIs(t, err, ErrPassRunning)
	assert.True(t, FirstRunPending(dir), "a refused disarm clears nothing")

	lock.Release()
	changed, err := Disarm(dir)
	require.NoError(t, err)
	assert.True(t, changed)
}

func TestArmDisarm_ReleaseTheRunLock(t *testing.T) {
	dir := t.TempDir()
	_, err := Arm(dir)
	require.NoError(t, err)
	_, err = Disarm(dir)
	require.NoError(t, err)

	lock, ok, err := AcquireRunLock(dir)
	require.NoError(t, err)
	assert.True(t, ok, "arm and disarm must not leave the lock held")
	lock.Release()
}

func TestDisarm_ReportsAnUnremovableMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, firstRunMarker)
	require.NoError(t, os.Mkdir(marker, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(marker, "x"), []byte("x"), 0o600))

	_, err := Disarm(dir)

	require.Error(t, err)
	assert.True(t, FirstRunPending(dir), "a marker that cannot be removed keeps deletion off")
}

func TestArmDisarm_FailWhenTheStateDirIsNotADirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	_, err := Arm(path)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPassRunning)
	_, err = Disarm(path)
	require.Error(t, err)
}

func TestArm_FailsWhenTheMarkerIsADirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, firstRunMarker), 0o750))

	changed, err := Arm(dir)

	require.Error(t, err)
	assert.False(t, changed)
	assert.True(t, FirstRunPending(dir))
}
