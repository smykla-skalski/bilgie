//go:build !windows

package fsx

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func within(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked on a special file")
	}
}

func TestReadRegular_NeverBlocksOnSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	sock := filepath.Join(os.TempDir(), "fsx-"+filepath.Base(dir)+".sock")
	l, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close(); _ = os.Remove(sock) })
	link := filepath.Join(dir, "link-to-fifo")
	require.NoError(t, os.Symlink(fifo, link))

	for name, path := range map[string]string{"fifo": fifo, "socket": sock, "link to fifo": link, "device": "/dev/null"} {
		within(t, func() {
			_, err := ReadRegular(path, 10)
			assert.ErrorIs(t, err, ErrNotRegular, name)
			assert.ErrorIs(t, CheckRegular(path), ErrNotRegular, name)
		})
	}
}

func TestWriteFileNoLink_RefusesLinksAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(victim, link))
	dangling := filepath.Join(dir, "dangling")
	require.NoError(t, os.Symlink(filepath.Join(dir, "created-by-mistake"), dangling))
	fifo := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	within(t, func() {
		require.ErrorIs(t, WriteFileNoLink(link, []byte("x"), 0o600), ErrSymlink)
		require.ErrorIs(t, WriteFileNoLink(dangling, []byte("x"), 0o600), ErrSymlink)
		require.ErrorIs(t, WriteFileNoLink(fifo, []byte("x"), 0o600), ErrNotRegular)
		require.ErrorIs(t, WriteFileNoLink(dir, []byte("x"), 0o600), ErrNotRegular)
		_, err := OpenAppendNoLink(link, 0o600)
		require.ErrorIs(t, err, ErrSymlink)
		_, err = OpenAppendNoLink(fifo, 0o600)
		require.ErrorIs(t, err, ErrNotRegular)
	})

	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data))
	assert.NoFileExists(t, filepath.Join(dir, "created-by-mistake"))
}

func TestNoFollowFlag_RefusesALinkSwappedInAfterTheCheck(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(victim, link))

	_, err := os.OpenFile(link, os.O_WRONLY|noFollowFlag, 0)

	require.ErrorIs(t, err, errLoop)
}
