//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_FifoAsTheConfigFailsInsteadOfBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	l := NewLoader()
	l.SetConfigPath(path)
	done := make(chan error, 1)

	go func() { _, err := l.Load(); done <- err }()

	select {
	case err := <-done:
		assert.ErrorContains(t, err, "not a regular file")
	case <-time.After(2 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}

func TestLoad_FollowsALinkedConfig(t *testing.T) {
	dir := t.TempDir()
	linked := filepath.Join(dir, "dotfiles.yaml")
	require.NoError(t, os.WriteFile(linked, []byte("version: \"1\"\nauto:\n  min_free: 12G\n"), 0o600))
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.Symlink(linked, path))
	l := NewLoader()
	l.SetConfigPath(path)

	cfg, err := l.Load()

	require.NoError(t, err)
	assert.Equal(t, "12G", cfg.Auto.MinFree)
}
