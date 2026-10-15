//go:build linux

package osshim

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentsFromProc(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "100", "py\x00", "py")
	require.NoError(t, os.WriteFile(filepath.Join(root, "100", "environ"), []byte("PATH=/c/archive-v0/abc/bin\x00HOME=/h\x00"), 0o600))
	writeProc(t, root, "101", "gone\x00", "gone")

	rows, err := environmentsFromProc(t.Context(), root, os.Getuid())
	require.NoError(t, err)
	assert.Equal(t, []ProcessEnv{{PID: 100, Text: "PATH=/c/archive-v0/abc/bin HOME=/h "}}, rows)
}

func TestEnvironmentsFromProcFailsClosed(t *testing.T) {
	_, err := environmentsFromProc(t.Context(), t.TempDir(), os.Getuid())
	assert.Error(t, err, "a root without processes is not an empty machine")
}
