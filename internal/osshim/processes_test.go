package osshim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	cmd.Env = append(os.Environ(), helperEnv+"=child", "OSSHIM_HEARTBEAT="+heartbeatPath(t))
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	want := marker
	if runtime.GOOS == "windows" {
		want = strings.TrimSuffix(filepath.Base(exe), ".exe")
	}
	require.Eventually(t, func() bool {
		lines, listErr := ProcessCommandLines(context.Background())
		if listErr != nil {
			return false
		}
		for _, l := range lines {
			if strings.Contains(l, want) {
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond)
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
