//go:build !windows

package osshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnyFileOpen(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	dir := t.TempDir()
	held := filepath.Join(dir, "held.db")
	require.NoError(t, os.WriteFile(held, []byte("x"), 0o600))
	missing := filepath.Join(dir, "missing.db-wal")

	open, err := AnyFileOpen(t.Context(), []string{held, missing})
	require.NoError(t, err)
	assert.False(t, open, "no process holds the file yet")

	f, err := os.Open(held)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	open, err = AnyFileOpen(t.Context(), []string{missing, held})
	require.NoError(t, err)
	assert.True(t, open)
}

func TestAnyFileOpen_NothingToCheck(t *testing.T) {
	open, err := AnyFileOpen(t.Context(), []string{filepath.Join(t.TempDir(), "absent")})
	require.NoError(t, err)
	assert.False(t, open)
}
