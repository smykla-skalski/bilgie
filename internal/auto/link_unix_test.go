//go:build !windows

package auto

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/fsx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func finishes(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked on a special file")
	}
}

func linkTo(t *testing.T, dir, name string) (link, victim string) {
	t.Helper()
	victim = filepath.Join(dir, "victim-"+name)
	require.NoError(t, os.WriteFile(victim, []byte("keep\n"), 0o600))
	link = filepath.Join(dir, name)
	require.NoError(t, os.Symlink(victim, link))
	return link, victim
}

func requireUntouched(t *testing.T, victim string) {
	t.Helper()
	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "keep\n", string(data))
}

func TestMarkFirstRunPending_RefusesToWriteThroughALink(t *testing.T) {
	dir := t.TempDir()
	link, victim := linkTo(t, dir, firstRunMarker)

	err := MarkFirstRunPending(dir)

	require.ErrorIs(t, err, fsx.ErrSymlink)
	requireUntouched(t, victim)
	assert.True(t, FirstRunPending(dir))
	assert.Equal(t, []string{link}, LinkedState(dir))
}

func TestFirstRunPending_ADanglingLinkKeepsDeletionOff(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, firstRunMarker)))

	assert.True(t, FirstRunPending(dir))
	require.NoError(t, ClearFirstRun(dir))
	assert.False(t, FirstRunPending(dir))
}

func TestMarkFirstRunPending_FifoAtTheMarkerFailsInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, firstRunMarker), 0o600))

	finishes(t, func() {
		require.ErrorIs(t, MarkFirstRunPending(dir), fsx.ErrNotRegular)
	})
	assert.True(t, FirstRunPending(dir))
}

func TestAppendRun_RefusesToWriteThroughALink(t *testing.T) {
	dir := t.TempDir()
	link, victim := linkTo(t, dir, RunLogName)

	err := AppendRun(dir, RunRecord{Time: time.Now()})

	require.ErrorIs(t, err, fsx.ErrSymlink)
	requireUntouched(t, victim)
	assert.Equal(t, []string{link}, LinkedState(dir))
}

func TestAppendRun_DoesNotRotateALinkToABigFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "big")
	require.NoError(t, os.WriteFile(victim, make([]byte, rotateAt), 0o600))
	require.NoError(t, os.Symlink(victim, filepath.Join(dir, RunLogName)))

	require.ErrorIs(t, AppendRun(dir, RunRecord{Time: time.Now()}), fsx.ErrSymlink)

	assert.NoFileExists(t, filepath.Join(dir, RunLogName+".1"))
}

func TestAppendRun_FifoAtTheRunLogFailsInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, RunLogName), 0o600))

	finishes(t, func() {
		require.ErrorIs(t, AppendRun(dir, RunRecord{Time: time.Now()}), fsx.ErrNotRegular)
	})
}

func TestWriteState_ReplacesALinkInsteadOfWritingThroughIt(t *testing.T) {
	for name, write := range map[string]func(dir string) error{
		TickStateName: func(dir string) error { return WriteTickState(dir, TickState{Tier: "ok"}) },
		PassStateName: func(dir string) error { return WritePassState(dir, PassState{Tier: "ok"}) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			link, victim := linkTo(t, dir, name)

			require.NoError(t, write(dir))

			requireUntouched(t, victim)
			info, err := os.Lstat(link)
			require.NoError(t, err)
			assert.True(t, info.Mode().IsRegular(), "the link itself was replaced")
		})
	}
}

func TestQuarantineState_NeverBlocksOrWritesThroughLinks(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, TickStateName), 0o600))
	bad, victim := linkTo(t, dir, TickStateName+".bad")

	finishes(t, func() {
		to, err := QuarantineState(dir, TickStateName)
		require.NoError(t, err)
		assert.Equal(t, bad, to)
	})

	requireUntouched(t, victim)
	assert.NoFileExists(t, filepath.Join(dir, TickStateName))
}

func TestReadTickState_FifoIsBadStateNotAHang(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, TickStateName), 0o600))

	finishes(t, func() {
		_, err := ReadTickState(dir)
		assert.ErrorIs(t, err, ErrBadState)
	})
}

func TestInstall_RefusesALinkedLogDir(t *testing.T) {
	s := &scriptedExec{}
	a := newOSAgent(t, goosDarwin, posixExe, s)
	logs := filepath.Join(a.Home, "Library", "Logs")
	elsewhere := filepath.Join(a.Home, "elsewhere")
	require.NoError(t, os.MkdirAll(logs, 0o750))
	require.NoError(t, os.MkdirAll(elsewhere, 0o750))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(logs, agentName)))

	err := a.Install(t.Context())

	require.ErrorContains(t, err, "is a symbolic link")
	assert.False(t, scheduled(s, "bootstrap"))
	assert.False(t, FirstRunPending(a.StateDir), "a failed install leaves no marker behind")
	entries, readErr := os.ReadDir(elsewhere)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
	assert.Equal(t, []string{filepath.Join(logs, agentName)}, a.LinkedPaths())
}

func TestInstall_FollowsALinkedParentOfTheLogDir(t *testing.T) {
	s := &scriptedExec{}
	a := newOSAgent(t, goosDarwin, posixExe, s)
	elsewhere := filepath.Join(a.Home, "elsewhere")
	require.NoError(t, os.MkdirAll(elsewhere, 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(a.Home, "Library"), 0o750))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(a.Home, "Library", "Logs")))

	require.NoError(t, a.Install(t.Context()))

	assert.DirExists(t, filepath.Join(elsewhere, agentName))
	assert.Empty(t, a.LinkedPaths())
}
