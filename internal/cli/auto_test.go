package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const autoGiB = int64(1) << 30

func oldTime() time.Time { return time.Now().Add(-48 * time.Hour) }

type autoFixture struct {
	t        *testing.T
	env      autoEnv
	loader   *config.Loader
	out      *bytes.Buffer
	sentinel string
	launchd  [][]string
	home     string
}

func newAutoFixture(t *testing.T, free int64, extraConfig string) *autoFixture {
	t.Helper()
	home := t.TempDir()
	cacheDir := filepath.Join(home, "cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o750))
	sentinel := filepath.Join(cacheDir, "old.bin")
	require.NoError(t, os.WriteFile(sentinel, []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(sentinel, oldTime(), oldTime()))

	cfgPath := filepath.Join(home, "config.yaml")
	cfg := `version: "1"
providers:
  tool:
    enabled: true
    paths:
      - ` + cacheDir + `
    max_size: 1G
    max_age: 1d
    clean_cmd: "true"
` + extraConfig
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))

	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()

	f := &autoFixture{t: t, loader: loader, out: &bytes.Buffer{}, sentinel: sentinel, home: home}
	f.env = autoEnv{
		free: func() (auto.FreeSpace, error) {
			return auto.FreeSpace{Free: free, Total: 1000 * autoGiB}, nil
		},
		newProvider: provider.NewProvider,
		exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			f.launchd = append(f.launchd, append([]string{name}, args...))
			return nil, nil
		},
		out:      f.out,
		stateDir: filepath.Join(home, "state"),
		home:     home,
		exe:      "/opt/homebrew/bin/bilgie",
		uid:      501,
		goos:     "darwin",
	}
	return f
}

func (f *autoFixture) cleaned() bool {
	_, err := os.Stat(f.sentinel)
	return os.IsNotExist(err)
}

func TestAuto_FirstRunAfterInstallDeletesNothingThenCleans(t *testing.T) {
	f := newAutoFixture(t, 1*autoGiB, "")
	require.NoError(t, auto.MarkFirstRunPending(f.env.stateDir))

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))
	assert.False(t, f.cleaned(), "first run must not delete")
	assert.Contains(t, f.out.String(), "first run after install-agent: dry-run")
	assert.False(t, auto.FirstRunPending(f.env.stateDir), "marker cleared after the dry-run")

	f.out.Reset()
	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))
	assert.True(t, f.cleaned(), "second run cleans")
}

func TestAuto_FirstRunMarkerSurvivesRestartUntilDryRunCompletes(t *testing.T) {
	f := newAutoFixture(t, 1*autoGiB, "")
	require.NoError(t, auto.MarkFirstRunPending(f.env.stateDir))
	failing := f.env
	failing.free = func() (auto.FreeSpace, error) { return auto.FreeSpace{}, os.ErrInvalid }

	require.Error(t, runAutoWithLoader(t.Context(), f.loader, failing, false))
	assert.True(t, auto.FirstRunPending(f.env.stateDir), "an aborted run keeps the marker")

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))
	assert.False(t, f.cleaned())
}

func TestAuto_FirstRunAtOKTierLeavesMarkerArmed(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "auto:\n  min_free: 10G\n  min_free_pct: 5\n")
	require.NoError(t, auto.MarkFirstRunPending(f.env.stateDir))

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.Contains(t, f.out.String(), "tier ok")
	assert.False(t, f.cleaned())
	assert.True(t, auto.FirstRunPending(f.env.stateDir), "nothing was previewed, so the next run is still a dry-run")
}

func TestAuto_FirstRunWithEveryProviderFailingLeavesMarkerArmed(t *testing.T) {
	f := newAutoFixture(t, 1*autoGiB, "")
	require.NoError(t, auto.MarkFirstRunPending(f.env.stateDir))
	f.env.newProvider = func(string, config.Provider) (provider.Provider, error) {
		return nil, os.ErrInvalid
	}

	require.Error(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.True(t, auto.FirstRunPending(f.env.stateDir))
}

func TestAuto_FirstRunWithOneFailingProviderKeepsMarkerAndExitsNonZero(t *testing.T) {
	f := newAutoFixture(t, 1*autoGiB, "")
	require.NoError(t, auto.MarkFirstRunPending(f.env.stateDir))
	base := f.env.newProvider
	f.env.newProvider = func(name string, cfg config.Provider) (provider.Provider, error) {
		if name == "tool" {
			return base(name, cfg)
		}
		return nil, os.ErrInvalid
	}
	bad := filepath.Join(f.home, "bad")
	require.NoError(t, os.MkdirAll(bad, 0o750))
	cfgPath := filepath.Join(f.home, "config2.yaml")
	cfg := "version: \"1\"\nproviders:\n  tool:\n    enabled: true\n    paths: [" + filepath.Join(f.home, "cache") +
		"]\n    max_size: 1G\n    max_age: 1d\n    clean_cmd: \"true\"\n  broken:\n    enabled: true\n    paths: [" + bad + "]\n    max_size: 1G\n    max_age: 1d\n    clean_cmd: \"true\"\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))
	f.loader.SetConfigPath(cfgPath)

	require.Error(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.False(t, f.cleaned(), "first run is a dry-run")
	assert.True(t, auto.FirstRunPending(f.env.stateDir), "a run with a provider error must not consume the marker")
}

func TestAuto_FirstRunClearFailureStampsThePassAndKeepsMarkerArmed(t *testing.T) {
	f := newAutoFixture(t, 1*autoGiB, "")
	marker := filepath.Join(f.env.stateDir, "first-run-pending")
	require.NoError(t, os.MkdirAll(marker, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(marker, "keep"), []byte("x"), 0o600))

	err := runAutoWithLoader(t.Context(), f.loader, f.env, false)

	require.ErrorContains(t, err, "remove first-run marker")
	assert.False(t, f.cleaned(), "first run is a dry-run")
	assert.True(t, auto.FirstRunPending(f.env.stateDir), "an uncleared marker keeps the gate armed")
	pass, readErr := auto.ReadPassState(f.env.stateDir)
	require.NoError(t, readErr)
	assert.False(t, pass.Time.IsZero(), "the pass is stamped so the tick does not repeat it")
	assert.True(t, pass.DryRun)

	f.out.Reset()
	require.Error(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))
	assert.False(t, f.cleaned(), "the armed marker still forces a dry-run")
	assert.Contains(t, f.out.String(), "first run after install-agent: dry-run")
}

func TestAuto_ExplicitDryRunKeepsMarker(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	require.NoError(t, auto.MarkFirstRunPending(f.env.stateDir))
	dryRunOnly := true

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, dryRunOnly))

	assert.False(t, f.cleaned())
	assert.True(t, auto.FirstRunPending(f.env.stateDir), "a manual preview must not consume the marker")
}

func TestAuto_NoMarkerCleansOnLowSpace(t *testing.T) {
	f := newAutoFixture(t, 40*autoGiB, "")

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.True(t, f.cleaned())
	assert.Contains(t, f.out.String(), "tier low")
}

func TestAuto_PlentyOfSpaceAndUnderLimitDoesNothing(t *testing.T) {
	f := newAutoFixture(t, 500*autoGiB, "")

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.False(t, f.cleaned())
	assert.Contains(t, f.out.String(), "tier ok")
	assert.Contains(t, f.out.String(), "within limit")
}

func TestAuto_NeverRunsDockerVolumesEvenWhenEnabled(t *testing.T) {
	volDir := t.TempDir()
	volFile := filepath.Join(volDir, "data.bin")
	require.NoError(t, os.WriteFile(volFile, []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(volFile, oldTime(), oldTime()))
	f := newAutoFixture(t, 1*autoGiB, `  docker-volumes:
    enabled: true
    paths:
      - `+volDir+`
    max_size: 1
    max_age: 1d
    clean_cmd: "true"
`)

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.True(t, f.cleaned())
	assert.FileExists(t, volFile)
}

func TestAuto_CriticalSweepsDisabledDirPatternProvider(t *testing.T) {
	stale := filepath.Join(t.TempDir(), "sailx-stale")
	require.NoError(t, os.MkdirAll(stale, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "f"), []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(filepath.Join(stale, "f"), oldTime(), oldTime()))
	require.NoError(t, os.Chtimes(stale, oldTime(), oldTime()))
	f := newAutoFixture(t, 1*autoGiB, `  sweep:
    enabled: false
    type: dir-pattern
    paths:
      - `+filepath.Join(filepath.Dir(stale), "sail*")+`
    max_size: 1G
    min_idle: 1h
    skip_if_open: false
`)

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.NoDirExists(t, stale)
	assert.Contains(t, f.out.String(), "tier emergency")
}

func TestAuto_ConcurrentRunExits(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	lock, ok, err := auto.AcquireRunLock(f.env.stateDir)
	require.NoError(t, err)
	require.True(t, ok)
	defer lock.Release()

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.False(t, f.cleaned())
	assert.Contains(t, f.out.String(), "another auto run is in progress")
}

func TestAuto_InvalidAutoConfigIsRejected(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	cfgPath := filepath.Join(f.home, "config.yaml")
	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, append(data, []byte("auto:\n  min_free_pct: 150\n")...), 0o600))

	err = runAutoWithLoader(t.Context(), f.loader, f.env, false)

	require.ErrorContains(t, err, "min_free_pct")
	assert.False(t, f.cleaned())
}

func TestAuto_ConfigOverridesChangeTier(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "auto:\n  min_free: 10G\n  min_free_pct: 5\n")

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.Contains(t, f.out.String(), "tier ok")
	assert.False(t, f.cleaned())
}

func TestInstallAgent_WritesPlistWithoutRealLaunchctl(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "auto:\n  interval: 60m\n  tick_interval: 5m\n")

	require.NoError(t, runInstallAgentWithLoader(t.Context(), f.loader, f.env))

	plist := filepath.Join(f.home, "Library", "LaunchAgents", auto.AgentLabel+".plist")
	data, err := os.ReadFile(plist)
	require.NoError(t, err)
	assert.Contains(t, string(data), "<integer>300</integer>")
	assert.True(t, auto.FirstRunPending(f.env.stateDir))
	require.Len(t, f.launchd, 2)
	assert.Equal(t, []string{"launchctl", "bootout", "gui/501/" + auto.AgentLabel}, f.launchd[0])
}

func TestInstallAgent_RejectsTooShortInterval(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "auto:\n  interval: 10s\n")

	err := runInstallAgentWithLoader(t.Context(), f.loader, f.env)

	require.ErrorContains(t, err, "interval")
	assert.Empty(t, f.launchd)
}

func TestUninstallAgent_RemovesPlist(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	require.NoError(t, runInstallAgentWithLoader(t.Context(), f.loader, f.env))
	f.launchd = nil

	require.NoError(t, f.env.agent(0).Uninstall(t.Context()))

	assert.NoFileExists(t, filepath.Join(f.home, "Library", "LaunchAgents", auto.AgentLabel+".plist"))
	assert.False(t, auto.FirstRunPending(f.env.stateDir))
	assert.Equal(t, [][]string{
		{"launchctl", "bootout", "gui/501/" + auto.AgentLabel},
	}, f.launchd)
}

func TestAssumedFree(t *testing.T) {
	base := func() (auto.FreeSpace, error) { return auto.FreeSpace{Free: 9, Total: 100}, nil }

	fn, err := assumedFree("3G", base)
	require.NoError(t, err)
	fs, err := fn()
	require.NoError(t, err)
	assert.Equal(t, 3*autoGiB, fs.Free)
	assert.Equal(t, int64(100), fs.Total)

	_, err = assumedFree("lots", base)
	require.Error(t, err)
}

func TestInstallAgent_PicksTheBackendOfTheOS(t *testing.T) {
	tests := []struct {
		goos     string
		exe      string
		wantCmds [][]string
		wantFile func(home, state string) string
	}{
		{"linux", "/usr/local/bin/bilgie", [][]string{
			{"crontab", "-l"},
			{"systemctl", "--user", "show-environment"},
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", auto.SystemdUnit + ".timer"},
			{"systemctl", "--user", "restart", auto.SystemdUnit + ".timer"},
			{"loginctl", "show-user", "501", "--property=Linger", "--value"},
			{"crontab", "-l"},
		}, func(home, _ string) string {
			return filepath.Join(home, ".config", "systemd", "user", auto.SystemdUnit+".timer")
		}},
		{"windows", `C:\bin\bilgie.exe`, nil, func(_, state string) string {
			return filepath.Join(state, auto.TaskName+"-task.xml")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			f := newAutoFixture(t, 100*autoGiB, "auto:\n  interval: 60m\n")
			f.env.goos = tt.goos
			f.env.exe = tt.exe

			require.NoError(t, runInstallAgentWithLoader(t.Context(), f.loader, f.env))

			assert.FileExists(t, tt.wantFile(f.home, f.env.stateDir))
			assert.True(t, auto.FirstRunPending(f.env.stateDir))
			if tt.wantCmds != nil {
				assert.Equal(t, tt.wantCmds[1:], f.launchd)
			} else {
				require.Len(t, f.launchd, 1)
				assert.Equal(t, "schtasks", f.launchd[0][0])
			}

			f.launchd = nil
			require.NoError(t, f.env.agent(0).Uninstall(t.Context()))
			assert.NoFileExists(t, tt.wantFile(f.home, f.env.stateDir))
			assert.False(t, auto.FirstRunPending(f.env.stateDir))
		})
	}
}

func TestAuto_MissingNotifierIsALoggedSkip(t *testing.T) {
	f := newAutoFixture(t, 1*autoGiB, "")
	require.NoError(t, auto.ClearFirstRun(f.env.stateDir))
	f.env.notify = auto.NotifierFor("linux", func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	})

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.Contains(t, f.out.String(), "notification skipped")
	assert.NotContains(t, f.out.String(), "warning:")
}

func TestUserConfigDir_HonoursAbsoluteXDGConfigHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	custom := filepath.Join(t.TempDir(), "cfg")

	t.Setenv("XDG_CONFIG_HOME", custom)
	assert.Equal(t, custom, userConfigDir(home))

	t.Setenv("XDG_CONFIG_HOME", "relative/cfg")
	assert.Equal(t, filepath.Join(home, ".config"), userConfigDir(home))

	t.Setenv("XDG_CONFIG_HOME", "")
	assert.Equal(t, filepath.Join(home, ".config"), userConfigDir(home))
}

func TestInstallAgent_WritesSystemdUnitsUnderTheConfigDir(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	f.env.goos = "linux"
	f.env.exe = "/usr/local/bin/bilgie"
	f.env.configDir = filepath.Join(f.home, "xdg")

	require.NoError(t, runInstallAgentWithLoader(t.Context(), f.loader, f.env))

	assert.FileExists(t, filepath.Join(f.home, "xdg", "systemd", "user", auto.SystemdUnit+".timer"))
	assert.NoDirExists(t, filepath.Join(f.home, ".config"))
}

func TestAuto_LoadErrorNamedInOutputAndRunLogWhileOthersRun(t *testing.T) {
	f := newAutoFixture(t, 1*autoGiB, `  rel:
    enabled: true
    type: dir-pattern
    min_idle: banana
    paths:
      - `+filepath.Join(t.TempDir(), "dirs-*")+`
    max_size: 1G
`)

	err := runAutoWithLoader(t.Context(), f.loader, f.env, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider rel: parse min_idle")
	assert.NotContains(t, err.Error(), "rel: provider rel")
	assert.Contains(t, f.out.String(), "provider rel: parse min_idle")
	assert.True(t, f.cleaned(), "the healthy provider still ran")

	runs, _, readErr := auto.ReadRuns(f.env.stateDir, 0)
	require.NoError(t, readErr)
	require.NotEmpty(t, runs)
	var logged string
	for _, p := range runs[len(runs)-1].Providers {
		if p.Name == "rel" {
			logged = p.Error
		}
	}
	assert.Contains(t, logged, "provider rel: parse min_idle")
}
