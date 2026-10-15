package osshim

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessStartOfThisProcess(t *testing.T) {
	start, running, err := ProcessStart(t.Context(), os.Getpid())
	require.NoError(t, err)
	require.True(t, running)

	now := time.Now()
	assert.False(t, start.After(now.Add(2*time.Second)), "started in the future: %s", start)
	assert.True(t, start.After(now.Add(-24*time.Hour)), "test binary started a day ago: %s", start)
}

func TestProcessStartOfExitedProcess(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)

	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^$")
	require.NoError(t, cmd.Run())

	_, running, err := ProcessStart(t.Context(), cmd.Process.Pid)
	require.NoError(t, err)
	assert.False(t, running)
}

func TestProcessStartOfRunningChild(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)

	before := time.Now().Add(-2 * time.Second)
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestProcessStartHold$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "BILGIE_HOLD=1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	start, running, err := ProcessStart(t.Context(), cmd.Process.Pid)
	require.NoError(t, err)
	require.True(t, running)
	assert.True(t, start.After(before), "child start %s before %s", start, before)
	assert.False(t, start.After(time.Now().Add(2*time.Second)))
}

func TestProcessStartHold(t *testing.T) {
	if os.Getenv("BILGIE_HOLD") == "" {
		t.Skip("helper process")
	}
	time.Sleep(25 * time.Second)
}
