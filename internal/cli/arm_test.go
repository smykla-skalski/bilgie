package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArmDisarmCommands_FlipTheMarkerAndSayWhat(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")

	require.NoError(t, runDisarmWithEnv(f.env))
	assert.Contains(t, f.out.String(), "already disarmed: the next real pass deletes")
	f.out.Reset()

	require.NoError(t, runArmWithEnv(f.env))
	assert.True(t, auto.FirstRunPending(f.env.stateDir))
	assert.Contains(t, f.out.String(), "armed: the next real pass only previews and deletes nothing")
	assert.Contains(t, f.out.String(), "bilgie disarm")
	f.out.Reset()

	require.NoError(t, runArmWithEnv(f.env))
	assert.Contains(t, f.out.String(), "already armed")
	f.out.Reset()

	require.NoError(t, runDisarmWithEnv(f.env))
	assert.False(t, auto.FirstRunPending(f.env.stateDir))
	assert.Contains(t, f.out.String(), "disarmed: the next real pass deletes")
	assert.Contains(t, f.out.String(), "bilgie arm")
}

func TestArmDisarmCommands_FailWhileAPassRuns(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	lock, ok, err := auto.AcquireRunLock(f.env.stateDir)
	require.NoError(t, err)
	require.True(t, ok)
	defer lock.Release()

	require.ErrorIs(t, runArmWithEnv(f.env), auto.ErrPassRunning)
	require.ErrorContains(t, runArmWithEnv(f.env), "try again when it finishes")
	require.ErrorIs(t, runDisarmWithEnv(f.env), auto.ErrPassRunning)
	assert.False(t, auto.FirstRunPending(f.env.stateDir))
}

func TestArmDisarmCommands_ReportAStateDirThatCannotBeUsed(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	require.NoError(t, os.WriteFile(f.env.stateDir, []byte("a file, not a directory"), 0o600))

	armErr := runArmWithEnv(f.env)
	require.ErrorContains(t, armErr, "arm: ")
	require.NotErrorIs(t, armErr, auto.ErrPassRunning)
	require.ErrorContains(t, runDisarmWithEnv(f.env), "disarm: ")
}

func TestArmDisarmCommands_TakeNoArguments(t *testing.T) {
	require.Error(t, ArmCmd.Args(ArmCmd, []string{"extra"}))
	require.Error(t, DisarmCmd.Args(DisarmCmd, []string{"extra"}))
}

func TestTick_ArmedHealthyTickRunsNoPassAndKeepsTheMarker(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Minute, "ok")
	require.NoError(t, runArmWithEnv(r.env))
	r.env.newProvider = func(string, config.Provider) (provider.Provider, error) {
		t.Fatal("a healthy tick must not start a pass")
		return nil, errUnexpectedScan
	}

	r.tick(400*autoGiB, 0)

	assert.Zero(t, r.passes())
	assert.True(t, auto.FirstRunPending(r.env.stateDir), "a healthy tick does not clear the marker")
	assert.False(t, r.cleaned())
}

func TestTick_ArmedLowTickPreviewsOnceThenDeletes(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")
	require.NoError(t, runArmWithEnv(r.env))

	r.tick(40*autoGiB, 0)
	assert.False(t, r.cleaned(), "an armed agent only previews")
	assert.False(t, auto.FirstRunPending(r.env.stateDir), "the completed preview clears the marker")

	r.tick(40*autoGiB, 11*time.Minute)
	assert.True(t, r.cleaned())
}

func TestTick_DisarmedLowTickDeletesWithoutAPreview(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")
	require.NoError(t, auto.MarkFirstRunPending(r.env.stateDir))
	require.NoError(t, runDisarmWithEnv(r.env))
	r.out.Reset()

	r.tick(40*autoGiB, 0)

	assert.True(t, r.cleaned(), "a disarmed agent deletes on the first real pass")
	assert.NotContains(t, r.out.String(), "first run after install-agent")
	runs := readRecords(t, r.autoFixture)
	require.Len(t, runs, 1)
	assert.False(t, runs[0].DryRun)
}

func TestTick_ReArmingAfterDisarmPreviewsAgain(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")
	require.NoError(t, runDisarmWithEnv(r.env))
	require.NoError(t, runArmWithEnv(r.env))

	r.tick(40*autoGiB, 0)

	assert.False(t, r.cleaned())
	assert.Contains(t, r.out.String(), "first run after install-agent: dry-run")
}

func TestInstallAgent_ReArmsADisarmedAgent(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	require.NoError(t, runDisarmWithEnv(f.env))
	require.False(t, auto.FirstRunPending(f.env.stateDir))

	require.NoError(t, runInstallAgentWithLoader(t.Context(), f.loader, f.env))

	assert.True(t, auto.FirstRunPending(f.env.stateDir))
}

func TestDoctor_FirstRunFindingTracksTheMarker(t *testing.T) {
	d := scheduled("linux")
	f := doctorFixture(t, d, 100*autoGiB, recentRun())

	require.NoError(t, runDoctorWithLoader(t.Context(), f.loader, f.env), f.out.String())
	assert.Contains(t, f.out.String(), "[ok  ] first run: the next real pass deletes (run: bilgie arm to preview first)")

	f.out.Reset()
	require.NoError(t, runArmWithEnv(f.env))
	f.out.Reset()
	require.NoError(t, runDoctorWithLoader(t.Context(), f.loader, f.env), "armed is a note, not a failure")
	assert.Contains(t, f.out.String(), "[note] first run: the next real pass only previews and deletes nothing (run: bilgie disarm to let it delete)")
}

func statusConfig(t *testing.T) *config.Loader {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "data.txt"), []byte("x"), 0o600))
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`version: "1"
providers:
  modetest:
    enabled: true
    paths:
      - `+cacheDir+`
    max_size: 1GB
    clean_cmd: echo
`), 0o600))
	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()
	return loader
}

func TestStatus_JSONReportsTheAgentMode(t *testing.T) {
	loader := statusConfig(t)
	stateDir := filepath.Join(t.TempDir(), "state")

	for _, tt := range []struct {
		name   string
		arm    bool
		armed  bool
		mode   string
		detail string
	}{
		{"disarmed", false, false, "acting", auto.DisarmedDetail},
		{"armed", true, true, "preview", auto.ArmedDetail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.arm {
				_, err := auto.Arm(stateDir)
				require.NoError(t, err)
			}
			var err error
			output := captureStdout(t, func() {
				err = runStatusInDir(loader, true, nil, nil, stateDir)
			})
			require.NoError(t, err)

			var out StatusOutput
			require.NoError(t, json.Unmarshal([]byte(output), &out))
			require.NotNil(t, out.AgentMode)
			assert.Equal(t, tt.armed, out.AgentMode.Armed)
			assert.Equal(t, tt.mode, out.AgentMode.Mode)
			assert.Equal(t, tt.detail, out.AgentMode.Detail)
		})
	}
}

func TestStatus_TablePrintsTheModeLineUnderTheTotal(t *testing.T) {
	loader := statusConfig(t)
	stateDir := filepath.Join(t.TempDir(), "state")

	var err error
	output := captureStdout(t, func() { err = runStatusInDir(loader, false, nil, nil, stateDir) })
	require.NoError(t, err)
	assert.Contains(t, output, "Agent mode: acting, the next real pass deletes (run: bilgie arm to flip it)")
	assert.Less(t, strings.Index(output, "Total:"), strings.Index(output, "Agent mode:"))

	_, err = auto.Arm(stateDir)
	require.NoError(t, err)
	output = captureStdout(t, func() { err = runStatusInDir(loader, false, nil, nil, stateDir) })
	require.NoError(t, err)
	assert.Contains(t, output, "Agent mode: preview, the next real pass only previews and deletes nothing (run: bilgie disarm to flip it)")
}

func TestStatus_WithoutAStateDirOmitsTheAgentMode(t *testing.T) {
	loader := statusConfig(t)

	var err error
	output := captureStdout(t, func() { err = runStatusWithLoader(loader, true, nil, nil) })
	require.NoError(t, err)
	assert.NotContains(t, output, "agent_mode")

	output = captureStdout(t, func() { err = runStatusWithLoader(loader, false, nil, nil) })
	require.NoError(t, err)
	assert.NotContains(t, output, "Agent mode")
}

func noProviderConfig(t *testing.T) *config.Loader {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`version: "1"
providers:
  off:
    enabled: false
    paths:
      - /tmp
    max_size: 1GB
`), 0o600))
	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()
	return loader
}

func TestStatus_NoEnabledProvidersStillReportsTheAgentMode(t *testing.T) {
	loader := noProviderConfig(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	_, err := auto.Arm(stateDir)
	require.NoError(t, err)

	output := captureStdout(t, func() { err = runStatusInDir(loader, false, nil, nil, stateDir) })
	require.NoError(t, err)
	assert.Contains(t, output, "No enabled providers")
	assert.Contains(t, output, "Agent mode: preview")

	output = captureStdout(t, func() { err = runStatusInDir(loader, true, nil, nil, stateDir) })
	require.NoError(t, err)
	var out StatusOutput
	require.NoError(t, json.Unmarshal([]byte(output), &out))
	assert.Empty(t, out.Providers)
	assert.Contains(t, output, `"providers": []`)
	require.NotNil(t, out.AgentMode)
	assert.True(t, out.AgentMode.Armed)

	output = captureStdout(t, func() { err = runStatusWithLoader(loader, true, nil, nil) })
	require.NoError(t, err)
	assert.Equal(t, "No enabled providers\n", output, "without a state dir the legacy text stays")
}

// homeWithConfig points HOME at a directory whose config enables one
// provider, so the real command wiring (config path, state dir) is used.
func homeWithConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(home, ".config", "bilgie")
	require.NoError(t, os.MkdirAll(cfgDir, 0o750))
	cacheDir := filepath.Join(home, "cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(`version: "1"
providers:
  modetest:
    enabled: true
    paths:
      - `+cacheDir+`
    max_size: 1GB
    clean_cmd: echo
`), 0o600))
	return home
}

func TestArmDisarmStatus_ThroughTheRealCommandWiring(t *testing.T) {
	home := homeWithConfig(t)
	marker := filepath.Join(home, ".local", "state", "bilgie", "first-run-pending")

	require.NoError(t, runArm(ArmCmd, nil))
	assert.FileExists(t, marker)

	statusMode := func() *AgentMode {
		t.Helper()
		require.NoError(t, StatusCmd.Flags().Set("json", "true"))
		require.NoError(t, StatusCmd.Flags().Set("unmanaged", "0"))
		t.Cleanup(func() {
			for _, name := range []string{"json", "unmanaged"} {
				_ = StatusCmd.Flags().Set(name, StatusCmd.Flags().Lookup(name).DefValue)
			}
		})
		var err error
		output := captureStdout(t, func() { err = runStatus(StatusCmd, nil) })
		require.NoError(t, err)
		var out StatusOutput
		require.NoError(t, json.Unmarshal([]byte(output), &out), output)
		return out.AgentMode
	}
	mode := statusMode()
	require.NotNil(t, mode)
	assert.True(t, mode.Armed)

	require.NoError(t, runDisarm(DisarmCmd, nil))
	assert.NoFileExists(t, marker)
	mode = statusMode()
	require.NotNil(t, mode)
	assert.False(t, mode.Armed)
	assert.Equal(t, "acting", mode.Mode)
}
