//go:build linux

package osshim

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHoldersFromProc(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".lock")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	writeProc(t, root, "100", "uvx\x00", "uv")
	writeProc(t, root, "101", "other\x00", "other")
	require.NoError(t, os.Symlink(target, filepath.Join(root, "100", "fd", "9")))
	require.NoError(t, os.Symlink("/dev/null", filepath.Join(root, "101", "fd", "0")))

	holders, err := holdersFromProc(t.Context(), root, target, os.Getuid())
	require.NoError(t, err)
	assert.Equal(t, []int{100}, holders)
}

func TestHoldersFromProcMissingFileHasNoHolders(t *testing.T) {
	holders, err := holdersFromProc(t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "absent"), os.Getuid())
	require.NoError(t, err)
	assert.Empty(t, holders)
}
