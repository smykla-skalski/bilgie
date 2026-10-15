//go:build unix

package osshim

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeProc(t *testing.T, root, pid, cmdline, comm string) string {
	t.Helper()
	base := filepath.Join(root, pid)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "fd"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "cmdline"), []byte(cmdline), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(base, "comm"), []byte(comm+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(base, "maps"), nil, 0o600))
	return base
}

func TestCommandLinesFromProc(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "100", "/usr/bin/go\x00build\x00./...\x00", "go")
	writeProc(t, root, "101", "", "kworker/0:1")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "self"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "200"), 0o700))

	lines, err := commandLinesFromProc(t.Context(), root)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"/usr/bin/go build ./... go", "kworker/0:1"}, lines)
}

func TestCommandLinesFromProcFailsClosed(t *testing.T) {
	t.Run("missing root", func(t *testing.T) {
		_, err := commandLinesFromProc(t.Context(), filepath.Join(t.TempDir(), "nope"))
		assert.Error(t, err)
	})

	t.Run("root without processes is not an empty machine", func(t *testing.T) {
		_, err := commandLinesFromProc(t.Context(), t.TempDir())
		assert.Error(t, err)
	})

	t.Run("unreadable cmdline", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		require.NoError(t, os.Remove(filepath.Join(base, "cmdline")))
		require.NoError(t, os.Mkdir(filepath.Join(base, "cmdline"), 0o700))

		_, err := commandLinesFromProc(t.Context(), root)
		assert.Error(t, err)
	})

	t.Run("cancelled context", func(t *testing.T) {
		root := t.TempDir()
		writeProc(t, root, "100", "x", "x")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := commandLinesFromProc(ctx, root)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestProcHasOpenFiles(t *testing.T) {
	uid := os.Getuid()
	dir := filepath.Join(t.TempDir(), "cache")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	elsewhere := t.TempDir()

	link := func(t *testing.T, base, sub, name, target string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Join(base, sub), 0o700))
		require.NoError(t, os.Symlink(target, filepath.Join(base, sub, name)))
	}

	t.Run("nothing open", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		link(t, base, "fd", "3", filepath.Join(elsewhere, "f"))
		link(t, base, "fd", "4", "socket:[123]")
		require.NoError(t, os.Symlink(elsewhere, filepath.Join(base, "cwd")))

		open, err := procHasOpenFiles(t.Context(), root, dir, uid)
		require.NoError(t, err)
		assert.False(t, open)
	})

	t.Run("sibling with shared prefix is not inside", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		link(t, base, "fd", "3", dir+"-other/f")

		open, err := procHasOpenFiles(t.Context(), root, dir, uid)
		require.NoError(t, err)
		assert.False(t, open)
	})

	t.Run("open fd", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		link(t, base, "fd", "3", filepath.Join(dir, "a", "f"))

		open, err := procHasOpenFiles(t.Context(), root, dir, uid)
		require.NoError(t, err)
		assert.True(t, open)
	})

	t.Run("deleted file still counts", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		link(t, base, "fd", "3", filepath.Join(dir, "f")+" (deleted)")

		open, err := procHasOpenFiles(t.Context(), root, dir, uid)
		require.NoError(t, err)
		assert.True(t, open)
	})

	t.Run("working directory", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		require.NoError(t, os.Symlink(dir, filepath.Join(base, "cwd")))

		open, err := procHasOpenFiles(t.Context(), root, dir, uid)
		require.NoError(t, err)
		assert.True(t, open)
	})

	t.Run("memory mapping", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		maps := "7f00-7f01 r--p 00000000 08:01 42                 " + filepath.Join(dir, "lib.so") + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(base, "maps"), []byte(maps), 0o600))

		open, err := procHasOpenFiles(t.Context(), root, dir, uid)
		require.NoError(t, err)
		assert.True(t, open)
	})

	t.Run("uninspectable process of our own user fails closed", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		require.NoError(t, os.Remove(filepath.Join(base, "maps")))
		require.NoError(t, os.Mkdir(filepath.Join(base, "maps"), 0o700))

		_, err := procHasOpenFiles(t.Context(), root, dir, uid)
		assert.Error(t, err)
	})

	t.Run("uninspectable process of another user is skipped", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		status := "Uid:\t" + strconv.Itoa(uid+5) + "\t" + strconv.Itoa(uid+5) + "\t0\t0\n"
		require.NoError(t, os.WriteFile(filepath.Join(base, "status"), []byte(status), 0o600))
		require.NoError(t, os.Chmod(filepath.Join(base, "maps"), 0o000))
		if _, err := os.ReadFile(filepath.Join(base, "maps")); err == nil {
			t.Skip("running as a user that ignores file modes")
		}

		open, err := procHasOpenFiles(t.Context(), root, dir, uid+1)
		require.NoError(t, err)
		assert.False(t, open)
	})

	t.Run("other user process with our uid in status fails closed", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "x", "x")
		status := "Name:\tx\nUid:\t" + strconv.Itoa(uid+1) + "\t" + strconv.Itoa(uid) + "\t0\t0\n"
		require.NoError(t, os.WriteFile(filepath.Join(base, "status"), []byte(status), 0o600))
		require.NoError(t, os.Chmod(filepath.Join(base, "maps"), 0o000))
		if _, err := os.ReadFile(filepath.Join(base, "maps")); err == nil {
			t.Skip("running as a user that ignores file modes")
		}

		_, err := procHasOpenFiles(t.Context(), root, dir, uid+1)
		assert.Error(t, err)
	})

	t.Run("zombie with a denied fd directory holds nothing", func(t *testing.T) {
		root := t.TempDir()
		base := writeProc(t, root, "100", "", "x")
		require.NoError(t, os.WriteFile(filepath.Join(base, "status"), []byte("State:\tZ (zombie)\n"), 0o600))
		require.NoError(t, os.Chmod(filepath.Join(base, "fd"), 0o000))
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(base, "fd"), 0o700) })
		if _, err := os.ReadDir(filepath.Join(base, "fd")); err == nil {
			t.Skip("running as a user that ignores file modes")
		}

		open, err := procHasOpenFiles(t.Context(), root, dir, uid)
		require.NoError(t, err)
		assert.False(t, open)
	})

	t.Run("empty root fails closed", func(t *testing.T) {
		_, err := procHasOpenFiles(t.Context(), t.TempDir(), dir, uid)
		assert.Error(t, err)
	})
}

func TestProcessTableFromProcParentPID(t *testing.T) {
	root := t.TempDir()
	base := writeProc(t, root, "100", "/usr/bin/go\x00build\x00", "go")
	require.NoError(t, os.WriteFile(filepath.Join(base, "stat"), []byte("100 (my (odd) name) S 42 100 100 0"), 0o600))
	writeProc(t, root, "101", "x", "x")

	procs, err := processTableFromProc(t.Context(), root)
	require.NoError(t, err)
	byPID := map[int]Process{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	assert.Equal(t, 42, byPID[100].PPID)
	assert.Equal(t, "/usr/bin/go build", byPID[100].Args)
	assert.Zero(t, byPID[101].PPID, "missing stat is an unknown parent")
}

func TestProcessTableFromProcStartTime(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "stat"), []byte("cpu  1 2 3\nbtime 1700000000\nprocesses 5\n"), 0o600))
	base := writeProc(t, root, "100", "/usr/bin/go\x00run\x00", "go")
	// field 22 (starttime) is the 20th field after the "(comm) " prefix, in ticks since boot
	require.NoError(t, os.WriteFile(filepath.Join(base, "stat"),
		[]byte("100 (go (x)) S 42 100 100 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 150050 0 0"), 0o600))
	writeProc(t, root, "101", "x", "x")

	procs, err := processTableFromProc(t.Context(), root)
	require.NoError(t, err)
	byPID := map[int]Process{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	assert.Equal(t, time.Unix(1700001500, 500_000_000), byPID[100].Started)
	assert.True(t, byPID[101].Started.IsZero(), "missing stat is an unknown start")
}

func TestProcessTableFromProcStartTimeUnknown(t *testing.T) {
	stat := "100 (go) S 42 100 100 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 150050 0 0"
	for name, rootStat := range map[string]string{
		"no boot time":     "",
		"unparsable btime": "btime soon\n",
		"non-positive":     "btime 0\n",
		"no btime line":    "cpu 1 2 3\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if rootStat != "" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "stat"), []byte(rootStat), 0o600))
			}
			base := writeProc(t, root, "100", "go\x00run\x00", "go")
			require.NoError(t, os.WriteFile(filepath.Join(base, "stat"), []byte(stat), 0o600))
			procs, err := processTableFromProc(t.Context(), root)
			require.NoError(t, err)
			require.Len(t, procs, 1)
			assert.True(t, procs[0].Started.IsZero())
		})
	}

	t.Run("short or malformed stat", func(t *testing.T) {
		for _, bad := range []string{"100 (go) S 42", "garbage", "100 (go) S 42 100 100 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 abc 0 0"} {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "stat"), []byte("btime 1700000000\n"), 0o600))
			base := writeProc(t, root, "100", "go\x00run\x00", "go")
			require.NoError(t, os.WriteFile(filepath.Join(base, "stat"), []byte(bad), 0o600))
			procs, err := processTableFromProc(t.Context(), root)
			require.NoError(t, err)
			require.Len(t, procs, 1)
			assert.True(t, procs[0].Started.IsZero(), bad)
		}
	})
}
