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

func TestFileHolders(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil && !isLinux() {
		t.Skip("lsof not installed")
	}
	path := filepath.Join(t.TempDir(), ".lock")

	holders, err := FileHolders(t.Context(), path)
	require.NoError(t, err)
	assert.Empty(t, holders, "a missing file has no holders")

	f, err := os.Create(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	holders, err = FileHolders(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, []int{os.Getpid()}, holders)

	require.NoError(t, f.Close())
	holders, err = FileHolders(t.Context(), path)
	require.NoError(t, err)
	assert.Empty(t, holders)
}
