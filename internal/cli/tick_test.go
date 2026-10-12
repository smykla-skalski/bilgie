package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errUnexpectedScan = errors.New("provider constructed by a tick that needed no pass")

var tickStart = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

// tickRig drives runTickWithLoader with an injected clock and free-space
// reader, counting reads and provider constructions.
type tickRig struct {
	*autoFixture
	now       time.Time
	free      int64
	freeReads int
	providers int
}

func newTickRig(t *testing.T, extraConfig string) *tickRig {
	t.Helper()
	r := &tickRig{autoFixture: newAutoFixture(t, 0, extraConfig), now: tickStart, free: 400 * autoGiB}
	r.env.now = func() time.Time { return r.now }
	r.env.free = func() (auto.FreeSpace, error) {
		r.freeReads++
		return auto.FreeSpace{Free: r.free, Total: 1000 * autoGiB}, nil
	}
	r.env.newProvider = func(name string, cfg config.Provider) (provider.Provider, error) {
		r.providers++
		return provider.NewProvider(name, cfg)
	}
	return r
}

func (r *tickRig) tick(free int64, after time.Duration) {
	r.t.Helper()
	r.now = tickStart.Add(after)
	r.free = free
	require.NoError(r.t, runTickWithLoader(r.t.Context(), r.loader, r.env, false))
}

func (r *tickRig) passes() int { return len(readRecords(r.t, r.autoFixture)) }

func (r *tickRig) seedPass(at time.Duration, tier string) {
	r.t.Helper()
	require.NoError(r.t, auto.WritePassState(r.env.stateDir, auto.PassState{Time: tickStart.Add(at), Tier: tier}))
}

func TestTick_HealthyReadsFreeSpaceOnceAndTouchesNothingElse(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Minute, "ok")
	r.env.newProvider = func(string, config.Provider) (provider.Provider, error) {
		t.Fatal("a healthy tick must not construct a provider or scan a directory")
		return nil, errUnexpectedScan
	}

	r.tick(400*autoGiB, 0)

	assert.Equal(t, 1, r.freeReads)
	assert.Zero(t, r.passes())
	assert.Empty(t, r.out.String(), "a healthy tick is silent")
	state, err := auto.ReadTickState(r.env.stateDir)
	require.NoError(t, err)
	assert.Equal(t, auto.ActionIdle, state.Action)
	assert.Equal(t, "ok", state.Tier)
	assert.False(t, r.cleaned())
}

func TestTick_NoDirectoryWalkWhenNoPassIsDue(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Minute, "ok")
	for _, free := range []int64{400 * autoGiB, 300 * autoGiB, 200 * autoGiB} {
		r.env.newProvider = func(string, config.Provider) (provider.Provider, error) {
			t.Fatal("scan attempted by a tick that needed no pass")
			return nil, errUnexpectedScan
		}
		r.tick(free, time.Duration(0))
	}
	assert.Equal(t, 3, r.freeReads)
}

func TestTick_LowRunsOnePassThenWaitsForTheCooldown(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")

	r.tick(40*autoGiB, 0)
	assert.Equal(t, 1, r.passes())
	assert.True(t, r.cleaned())

	for _, m := range []int{2, 4, 6, 8} {
		r.tick(40*autoGiB, time.Duration(m)*time.Minute)
	}
	assert.Equal(t, 1, r.passes(), "inside the 10 minute cooldown")

	r.tick(40*autoGiB, 10*time.Minute)
	assert.Equal(t, 2, r.passes())
}

func TestTick_CriticalUsesTheShortCooldownAndEscalationBypassesTheLowOne(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")

	r.tick(40*autoGiB, 0)
	r.tick(8*autoGiB, 2*time.Minute)
	assert.Equal(t, 2, r.passes(), "low to critical escalates at once")

	r.tick(8*autoGiB, 3*time.Minute)
	assert.Equal(t, 2, r.passes())
	r.tick(8*autoGiB, 4*time.Minute)
	assert.Equal(t, 3, r.passes(), "critical every 2 minutes")
	r.tick(8*autoGiB, 6*time.Minute)
	assert.Equal(t, 4, r.passes())
}

func TestTick_HysteresisKeepsAnEasedTierFromRestartingPasses(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")

	r.tick(49*autoGiB, 0)
	assert.Equal(t, 1, r.passes())
	r.tick(51*autoGiB, 11*time.Minute)
	r.tick(49*autoGiB, 22*time.Minute)
	assert.Equal(t, 2, r.passes(), "51GB is in the hysteresis band, so the tier stayed low and 49GB is not a new crossing")
	state, err := auto.ReadTickState(r.env.stateDir)
	require.NoError(t, err)
	assert.Equal(t, "low", state.Tier)
}

func TestTick_EmergencySweepsStaleDirectoriesImmediately(t *testing.T) {
	stale := filepath.Join(t.TempDir(), "sailx-stale")
	require.NoError(t, os.MkdirAll(stale, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "f"), []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(filepath.Join(stale, "f"), oldTime(), oldTime()))
	require.NoError(t, os.Chtimes(stale, oldTime(), oldTime()))
	r := newTickRig(t, `  sweep:
    enabled: false
    type: dir-pattern
    paths:
      - `+filepath.Join(filepath.Dir(stale), "sail*")+`
    max_size: 1G
    min_idle: 1h
    skip_if_open: false
`)
	r.seedPass(-time.Minute, "critical")

	r.tick(2*autoGiB, 0)

	assert.NoDirExists(t, stale)
	assert.Contains(t, r.out.String(), "tier emergency")
}

func TestTick_FirstRunIsADryRunAndNeverDeletes(t *testing.T) {
	r := newTickRig(t, "")
	require.NoError(t, auto.MarkFirstRunPending(r.env.stateDir))

	r.tick(1*autoGiB, 0)

	assert.False(t, r.cleaned())
	assert.Contains(t, r.out.String(), "first run after install-agent: dry-run")
	runs := readRecords(t, r.autoFixture)
	require.Len(t, runs, 1)
	assert.True(t, runs[0].DryRun)
}

func TestTick_FirstRunStaysDryRunWhileNothingWasPreviewed(t *testing.T) {
	r := newTickRig(t, "")
	require.NoError(t, auto.MarkFirstRunPending(r.env.stateDir))

	r.tick(400*autoGiB, 0)
	assert.False(t, r.cleaned())
	assert.True(t, auto.FirstRunPending(r.env.stateDir), "a healthy routine pass previews nothing")

	r.tick(1*autoGiB, 31*time.Minute)
	assert.False(t, r.cleaned())
	assert.False(t, auto.FirstRunPending(r.env.stateDir))

	r.tick(1*autoGiB, 40*time.Minute)
	assert.True(t, r.cleaned(), "the pass after the completed dry-run deletes")
}

func TestTick_ExitsAtOnceWhenAPassHoldsTheRunLock(t *testing.T) {
	r := newTickRig(t, "")
	lock, ok, err := auto.AcquireRunLock(r.env.stateDir)
	require.NoError(t, err)
	require.True(t, ok)
	defer lock.Release()
	r.env.newProvider = func(string, config.Provider) (provider.Provider, error) {
		t.Fatal("a tick must not start work while another pass holds the lock")
		return nil, errUnexpectedScan
	}

	r.tick(1*autoGiB, 0)

	assert.Zero(t, r.passes())
	state, err := auto.ReadTickState(r.env.stateDir)
	require.NoError(t, err)
	assert.Equal(t, auto.ActionSkip, state.Action)
	assert.Equal(t, 1, r.freeReads)
}

func TestTick_NotificationsAreRateLimitedPerTier(t *testing.T) {
	r := newTickRig(t, "")
	var notes noteLog
	r.env.notify = notes.notifier(nil)
	r.seedPass(-time.Hour, "ok")

	r.tick(40*autoGiB, 0)
	assert.Len(t, notes.titles, 1)

	r.tick(39*autoGiB, 10*time.Minute)
	r.tick(38*autoGiB, 20*time.Minute)
	assert.Len(t, notes.titles, 1, "still low, same tier, inside 3 hours")

	r.tick(25*autoGiB, 30*time.Minute)
	assert.Len(t, notes.titles, 2, "free space dropped by more than 10GB")

	r.tick(8*autoGiB, 32*time.Minute)
	assert.Len(t, notes.titles, 3, "a lower tier is new")

	r.tick(8*autoGiB, 34*time.Minute)
	assert.Len(t, notes.titles, 3)

	r.tick(25*autoGiB, 3*time.Hour+31*time.Minute)
	assert.Len(t, notes.titles, 4, "the 3 hour window for low ended")
}

func TestTick_FallingTrendStartsAPassBeforeTheThreshold(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-5*time.Minute, "ok")

	for i, free := range []int64{70, 68, 66, 64} {
		r.tick(free*autoGiB, time.Duration(i)*2*time.Minute)
	}
	assert.Zero(t, r.passes(), "four samples are not a trend")

	r.tick(62*autoGiB, 8*time.Minute)

	assert.Equal(t, 1, r.passes())
	runs := readRecords(t, r.autoFixture)
	require.Len(t, runs, 1)
	assert.Equal(t, "low", runs[0].Tier)
	state, err := auto.ReadTickState(r.env.stateDir)
	require.NoError(t, err)
	assert.Equal(t, "ok", state.Tier)
}

func TestTick_AnOutlierDoesNotStartAPass(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-5*time.Minute, "ok")

	for i, free := range []int64{70, 70, 70, 56, 56, 56} {
		r.tick(free*autoGiB, time.Duration(i)*2*time.Minute)
	}

	assert.Zero(t, r.passes())
}

func TestTick_AssumeFreeIsADryRun(t *testing.T) {
	r := newTickRig(t, "")
	free, err := assumedFree("3G", r.env.free)
	require.NoError(t, err)
	r.env.free = free

	require.NoError(t, runTickWithLoader(t.Context(), r.loader, r.env, true))

	assert.False(t, r.cleaned())
	require.Len(t, readRecords(t, r.autoFixture), 1)
}

func TestTick_VerboseExplainsAnIdleTick(t *testing.T) {
	r := newTickRig(t, "")
	r.env.verbose = true
	r.seedPass(-time.Minute, "ok")

	r.tick(400*autoGiB, 0)

	assert.Contains(t, r.out.String(), "healthy")
}

func TestTick_RejectsInvalidTickInterval(t *testing.T) {
	for _, bad := range []string{"30s", "90s", "soon"} {
		f := newAutoFixture(t, 100*autoGiB, "auto:\n  tick_interval: "+bad+"\n")
		err := runInstallAgentWithLoader(t.Context(), f.loader, f.env)
		require.ErrorContains(t, err, "tick_interval", bad)
		assert.Empty(t, f.launchd)
	}
}

func TestInstallAgent_ReplacesTheV010AutoAgent(t *testing.T) {
	f := newAutoFixture(t, 100*autoGiB, "")
	plist := filepath.Join(f.home, "Library", "LaunchAgents", auto.AgentLabel+".plist")
	require.NoError(t, os.MkdirAll(filepath.Dir(plist), 0o750))
	require.NoError(t, os.WriteFile(plist, []byte("<string>auto</string><integer>1800</integer>"), 0o600))

	require.NoError(t, runInstallAgentWithLoader(t.Context(), f.loader, f.env))

	data, err := os.ReadFile(plist)
	require.NoError(t, err)
	assert.Contains(t, string(data), "<string>tick</string>")
	assert.Contains(t, string(data), "<integer>120</integer>")
	assert.NotContains(t, string(data), "<integer>1800</integer>")
	assert.Equal(t, []string{"launchctl", "bootout", "gui/501/" + auto.AgentLabel}, f.launchd[0])
	assert.Equal(t, "bootstrap", f.launchd[1][1])
}

func TestTick_PreviewRunsLeaveTheRealStateAlone(t *testing.T) {
	r := newTickRig(t, "")
	free, err := assumedFree("3G", r.env.free)
	require.NoError(t, err)
	r.env.free = free

	require.NoError(t, runTickWithLoader(t.Context(), r.loader, r.env, true))
	require.NoError(t, runAutoWithLoader(t.Context(), r.loader, r.env, true))

	assert.NoFileExists(t, filepath.Join(r.env.stateDir, "tick.json"))
	assert.NoFileExists(t, filepath.Join(r.env.stateDir, "pass.json"))
	assert.Len(t, readRecords(t, r.autoFixture), 2)
}

func TestTick_RechecksTheCooldownAfterTakingTheLock(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")
	r.env.free = func() (auto.FreeSpace, error) {
		require.NoError(t, auto.WritePassState(r.env.stateDir, auto.PassState{Time: r.now, Tier: "low"}))
		return auto.FreeSpace{Free: 40 * autoGiB, Total: 1000 * autoGiB}, nil
	}

	require.NoError(t, runTickWithLoader(t.Context(), r.loader, r.env, false))

	assert.Zero(t, r.passes(), "another pass finished first, so this tick is inside its cooldown")
	state, err := auto.ReadTickState(r.env.stateDir)
	require.NoError(t, err)
	assert.Equal(t, auto.ActionIdle, state.Action)
}

func TestTick_RefreshesTheTickTimeAfterALongPass(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")
	r.env.newProvider = func(name string, cfg config.Provider) (provider.Provider, error) {
		r.now = r.now.Add(20 * time.Minute)
		return provider.NewProvider(name, cfg)
	}

	r.tick(40*autoGiB, 0)

	state, err := auto.ReadTickState(r.env.stateDir)
	require.NoError(t, err)
	assert.False(t, state.Time.Before(tickStart.Add(20*time.Minute)))
}

func TestTick_StaleClockDoesNotStartASecondPass(t *testing.T) {
	r := newTickRig(t, "")
	calls := 0
	r.env.now = func() time.Time {
		calls++
		if calls == 1 {
			require.NoError(t, auto.WritePassState(r.env.stateDir, auto.PassState{Time: tickStart.Add(2 * time.Second), Tier: "low"}))
			return tickStart
		}
		return tickStart.Add(10 * time.Second)
	}
	r.env.free = func() (auto.FreeSpace, error) {
		return auto.FreeSpace{Free: 40 * autoGiB, Total: 1000 * autoGiB}, nil
	}

	require.NoError(t, runTickWithLoader(t.Context(), r.loader, r.env, false))

	assert.Zero(t, r.passes(), "the stamp is in the past once the clock is read under the lock")
}

func TestTick_UnusableStateFilesSelfHeal(t *testing.T) {
	for _, name := range []string{auto.TickStateName, auto.PassStateName} {
		t.Run(name+" directory", func(t *testing.T) {
			r := newTickRig(t, "")
			require.NoError(t, os.MkdirAll(filepath.Join(r.env.stateDir, name, "inner"), 0o750))

			r.tick(40*autoGiB, 0)

			assert.Contains(t, r.out.String(), "warning:")
			assert.Contains(t, r.out.String(), name+" is not a regular file")
			assert.DirExists(t, filepath.Join(r.env.stateDir, name+".bad"))
			assert.Equal(t, 1, r.passes(), "the tick carries on with empty state")
			_, err := auto.ReadTickState(r.env.stateDir)
			require.NoError(t, err)
			_, err = auto.ReadPassState(r.env.stateDir)
			require.NoError(t, err)
		})
		t.Run(name+" corrupt", func(t *testing.T) {
			r := newTickRig(t, "")
			require.NoError(t, os.MkdirAll(r.env.stateDir, 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(r.env.stateDir, name), []byte("{torn"), 0o600))

			r.tick(400*autoGiB, 0)

			assert.Contains(t, r.out.String(), name+" is corrupt")
			assert.FileExists(t, filepath.Join(r.env.stateDir, name+".bad"))
			_, err := auto.ReadTickState(r.env.stateDir)
			require.NoError(t, err)
		})
	}
}

func TestTick_UnreadableStateFileSelfHeals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not make a file unreadable on Windows")
	}
	r := newTickRig(t, "")
	path := filepath.Join(r.env.stateDir, auto.TickStateName)
	require.NoError(t, os.MkdirAll(r.env.stateDir, 0o750))
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o000))
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("running as a user that ignores file modes")
	}

	r.tick(400*autoGiB, 0)

	assert.Contains(t, r.out.String(), "moved aside")
	assert.FileExists(t, path+".bad")
	_, err := auto.ReadTickState(r.env.stateDir)
	require.NoError(t, err)
}

func TestAuto_UnusablePassStateDoesNotFailTheRun(t *testing.T) {
	f := newAutoFixture(t, 40*autoGiB, "")
	require.NoError(t, os.MkdirAll(filepath.Join(f.env.stateDir, auto.PassStateName, "inner"), 0o750))

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.Contains(t, f.out.String(), "pass.json is not a regular file")
	_, err := auto.ReadPassState(f.env.stateDir)
	require.NoError(t, err)
}

func TestTick_ForcedFirstRunDoesNotStartTheCooldown(t *testing.T) {
	r := newTickRig(t, "")
	require.NoError(t, auto.MarkFirstRunPending(r.env.stateDir))

	r.tick(40*autoGiB, 0)
	require.False(t, r.cleaned())
	require.False(t, auto.FirstRunPending(r.env.stateDir))

	r.tick(40*autoGiB, 2*time.Minute)
	assert.True(t, r.cleaned(), "the first real pass is not held back by the dry-run")
}

func TestTick_CancelledPassIsNotRecordedAsDone(t *testing.T) {
	r := newTickRig(t, "")
	r.seedPass(-time.Hour, "ok")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.now = tickStart
	r.free = 40 * autoGiB

	err := runTickWithLoader(ctx, r.loader, r.env, false)

	require.ErrorIs(t, err, context.Canceled)
	pass, err := auto.ReadPassState(r.env.stateDir)
	require.NoError(t, err)
	assert.Equal(t, tickStart.Add(-time.Hour), pass.Time, "the next tick may retry at once")
}

func TestTick_ConfigAndFreeSpaceErrorsFailTheTick(t *testing.T) {
	t.Run("config cannot load", func(t *testing.T) {
		r := newTickRig(t, "")
		path := filepath.Join(t.TempDir(), "broken.yaml")
		require.NoError(t, os.WriteFile(path, []byte("version: [unclosed"), 0o600))
		r.loader.SetConfigPath(path)
		require.ErrorContains(t, runTickWithLoader(t.Context(), r.loader, r.env, false), "load config")
	})

	t.Run("invalid limits", func(t *testing.T) {
		r := newTickRig(t, "auto:\n  tick_interval: soon\n")
		require.ErrorContains(t, runTickWithLoader(t.Context(), r.loader, r.env, false), "tick_interval")
	})

	t.Run("free space unreadable", func(t *testing.T) {
		r := newTickRig(t, "")
		r.env.free = func() (auto.FreeSpace, error) { return auto.FreeSpace{}, errors.New("statfs failed") }
		err := runTickWithLoader(t.Context(), r.loader, r.env, false)
		require.ErrorContains(t, err, "read free space")
		require.ErrorContains(t, err, "statfs failed")
	})
}

func TestTick_UnwritableStateDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	t.Run("an idle tick only warns", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("directory modes do not block writes on Windows")
		}
		r := newTickRig(t, "")
		r.seedPass(-time.Minute, "ok")
		require.NoError(t, os.Chmod(r.env.stateDir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(r.env.stateDir, 0o700) })
		if probe, err := os.Create(filepath.Join(r.env.stateDir, "probe")); err == nil {
			_ = probe.Close()
			t.Skip("running as a user that ignores directory modes")
		}

		r.tick(400*autoGiB, 0)

		assert.Contains(t, r.out.String(), "warning: record tick")
	})

	t.Run("a due pass cannot take the lock", func(t *testing.T) {
		r := newTickRig(t, "")
		r.env.stateDir = blocker

		err := runTickWithLoader(t.Context(), r.loader, r.env, false)

		require.Error(t, err)
		assert.Zero(t, r.providers)
	})
}

func TestAssumedFree_RejectsAnUnparsableSize(t *testing.T) {
	_, err := assumedFree("lots", func() (auto.FreeSpace, error) { return auto.FreeSpace{}, nil })
	require.ErrorContains(t, err, "--assume-free")

	free, err := assumedFree("3G", func() (auto.FreeSpace, error) { return auto.FreeSpace{}, errors.New("boom") })
	require.NoError(t, err)
	_, err = free()
	require.ErrorContains(t, err, "boom")
}

func TestTick_PendingFirstRunOnAHealthyDiskWaitsForTheInterval(t *testing.T) {
	r := newTickRig(t, "")
	require.NoError(t, auto.MarkFirstRunPending(r.env.stateDir))

	for _, minutes := range []int{0, 2, 4, 6} {
		r.tick(400*autoGiB, time.Duration(minutes)*time.Minute)
	}

	assert.Equal(t, 1, r.passes(), "the stamped dry-run holds the routine pass back for the interval")
	assert.True(t, auto.FirstRunPending(r.env.stateDir))
}

func TestTick_DryRunLeavesBadStateFilesInPlace(t *testing.T) {
	r := newTickRig(t, "")
	path := filepath.Join(r.env.stateDir, auto.TickStateName)
	require.NoError(t, os.MkdirAll(path, 0o750))

	require.NoError(t, runTickWithLoader(t.Context(), r.loader, r.env, true))

	assert.DirExists(t, path)
	assert.NoDirExists(t, path+".bad")
	assert.Contains(t, r.out.String(), "left in place")
}

func TestHealState_KeepsAFileThatIsUsableAgain(t *testing.T) {
	r := newTickRig(t, "")
	require.NoError(t, auto.WritePassState(r.env.stateDir, auto.PassState{Time: tickStart, Tier: "low"}))

	r.env.healState(auto.PassStateName, errors.New("stale read error"), false)

	assert.FileExists(t, filepath.Join(r.env.stateDir, auto.PassStateName))
	assert.NoFileExists(t, filepath.Join(r.env.stateDir, auto.PassStateName+".bad"))
	assert.Empty(t, r.out.String())
}

func TestTick_WrongShapeStateFilesAreMovedAside(t *testing.T) {
	for _, name := range []string{auto.TickStateName, auto.PassStateName} {
		for _, content := range []string{"[]", `"x"`, `{"time":123}`} {
			t.Run(name+" "+content, func(t *testing.T) {
				r := newTickRig(t, "")
				require.NoError(t, os.MkdirAll(r.env.stateDir, 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(r.env.stateDir, name), []byte(content), 0o600))

				r.tick(400*autoGiB, 0)

				assert.Contains(t, r.out.String(), "moved aside")
				assert.FileExists(t, filepath.Join(r.env.stateDir, name+".bad"))
			})
		}
	}
}
