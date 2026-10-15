package osshim

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessCommandLinesSeesChild(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)

	marker := "-osshim.marker=" + strconv.Itoa(os.Getpid())
	cmd := exec.Command(exe, marker)
	cmd.Env = append(os.Environ(), helperEnv+"=child", readyEnv+"=1", "OSSHIM_HEARTBEAT="+heartbeatPath(t))
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// A started child can still be mid-exec and show its parent's arguments, so
	// listing waits until the child itself reports it is running.
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err, "child exited before it was ready")
	require.Equal(t, readyLine, strings.TrimSpace(line))

	want := marker
	if runtime.GOOS == "windows" {
		want = strings.TrimSuffix(filepath.Base(exe), ".exe")
	}
	lines, err := ProcessCommandLines(context.Background())
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, want) }),
		"a running child must appear in the listing")
}

func TestProcessCommandLinesHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lines, err := ProcessCommandLines(ctx)
	if err == nil {
		assert.NotEmpty(t, lines, "a listing that succeeds must not be empty")
	}
}

func TestProcessTableReportsSelfAndParent(t *testing.T) {
	procs, err := ProcessTable(context.Background())
	require.NoError(t, err)

	for i := range procs {
		if procs[i].PID == os.Getpid() {
			assert.Equal(t, os.Getppid(), procs[i].PPID)
			return
		}
	}
	t.Fatal("own process missing from the process table")
}

func TestProcessTableReportsStartTime(t *testing.T) {
	procs, err := ProcessTable(context.Background())
	require.NoError(t, err)

	for i := range procs {
		if procs[i].PID != os.Getpid() {
			continue
		}
		require.False(t, procs[i].Started.IsZero(), "own start time must be known")
		// the test binary started moments ago; allow a wide margin for ps rounding
		assert.WithinDuration(t, time.Now(), procs[i].Started, 10*time.Minute)
		return
	}
	t.Fatal("own process missing from the process table")
}
