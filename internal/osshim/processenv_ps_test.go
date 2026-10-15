//go:build !linux && !windows

package osshim

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePSEnv(t *testing.T) {
	rows := parsePSEnv("    1 /sbin/launchd\n  812 python -c x PATH=/c/archive-v0/abc/bin:/usr/bin HOME=/h\n  900 sh A=one\ntwo=continued\n 901 ls")
	assert.Equal(t, []ProcessEnv{
		{PID: 1, Text: "/sbin/launchd"},
		{PID: 812, Text: "python -c x PATH=/c/archive-v0/abc/bin:/usr/bin HOME=/h"},
		{PID: 900, Text: "sh A=one\ntwo=continued"},
		{PID: 901, Text: "ls"},
	}, rows)
}

func TestProcessEnvironmentsSeesChildEnvironment(t *testing.T) {
	// ps hides the environment of Apple's own binaries, so the child is this test binary.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEnvSleeper$")
	cmd.Env = append(os.Environ(), "BILGIE_ENV_SLEEPER=1", "BILGIE_ENV_PROBE=/probe/archive-v0/xyz")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	rows, err := ProcessEnvironments(t.Context())
	require.NoError(t, err)
	var text string
	for _, r := range rows {
		if r.PID == cmd.Process.Pid {
			text = r.Text
		}
	}
	assert.Contains(t, text, "BILGIE_ENV_PROBE=/probe/archive-v0/xyz")
}

func TestEnvSleeper(t *testing.T) {
	if os.Getenv("BILGIE_ENV_SLEEPER") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(30 * time.Second)
}
