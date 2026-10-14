//go:build linux

package osshim

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeProcStat(t *testing.T, root, pid, ticks, state string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	line := pid + " (my (odd) cmd) " + state + " 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 1 0 " + ticks + " 1000 100 18446744073709551615\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o600))
}

func TestProcessStartFromProc(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3\nbtime 1000000\n"), 0o600))
	writeProcStat(t, root, "42", "250000", "S")
	writeProcStat(t, root, "43", "250000", "Z")

	start, running, err := processStartFromProc(root, 42)
	require.NoError(t, err)
	assert.True(t, running)
	assert.Equal(t, time.Unix(1000000+2500, 0), start)

	_, running, err = processStartFromProc(root, 43)
	require.NoError(t, err)
	assert.False(t, running, "a zombie has exited")

	_, running, err = processStartFromProc(root, 44)
	require.NoError(t, err)
	assert.False(t, running)
}

func TestProcessStartFromProcRejectsBrokenStat(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "stat"), []byte("btime 1000000\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "7"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "7", "stat"), []byte("7 (x) S 1 2\n"), 0o600))

	_, _, err := processStartFromProc(root, 7)
	require.Error(t, err)
}
