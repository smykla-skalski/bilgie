package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gib = int64(1) << 30

var fixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func testConfig(disabled ...string) *config.Config {
	cfg := &config.Config{
		Auto:      config.Auto{Interval: "30m", MinFree: "30G"},
		Providers: map[string]config.Provider{"go": {Enabled: true}, "npm": {Enabled: true}},
	}
	for _, name := range disabled {
		cfg.Providers[name] = config.Provider{Enabled: false}
	}
	return cfg
}

func healthy() Input {
	return Input{
		Now:             fixedNow,
		GOOS:            "linux",
		Cfg:             testConfig(),
		Agent:           auto.AgentState{Backend: auto.BackendSystemd, Installed: true, Loaded: true},
		Free:            auto.FreeSpace{Free: 200 * gib, Total: 500 * gib},
		NotifierProgram: "notify-send",
		Tick:            auto.TickState{Time: fixedNow.Add(-time.Minute), Tier: "ok", Reason: "healthy"},
		Pass:            auto.PassState{Time: fixedNow.Add(-20 * time.Minute), Tier: "ok"},
		Runs: []auto.RunRecord{{
			Time: fixedNow.Add(-20 * time.Minute), Tier: "ok",
			FreeBefore: 200 * gib, FreeAfter: 200 * gib, FreedBytes: gib,
		}},
	}
}

func find(t *testing.T, r Report, area string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Area == area {
			return f
		}
	}
	require.Failf(t, "finding missing", "area %q in %+v", area, r.Findings)
	return Finding{}
}

func TestDiagnose_HealthyNeedsNoAttention(t *testing.T) {
	r := Diagnose(healthy())
	assert.Zero(t, r.Attention(), "%+v", r.Findings)

	var buf bytes.Buffer
	r.Write(&buf)
	assert.Contains(t, buf.String(), "[ok  ] agent: installed and loaded (systemd)")
	assert.Contains(t, buf.String(), "last run: 20m ago (tier ok, freed 1.0 GiB, 0 provider error(s))")
	assert.Contains(t, buf.String(), "all good")
}

func TestDiagnose_Findings(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Input)
		area     string
		contains string
		hint     string
		level    Level
	}{
		{"agent missing", func(in *Input) { in.Agent = auto.AgentState{Backend: auto.BackendLaunchd} },
			"agent", "not installed", "bilgie install-agent", Fail},
		{"agent not loaded", func(in *Input) {
			in.Agent = auto.AgentState{Backend: auto.BackendLaunchd, Installed: true, Detail: "launchd has no job x"}
		}, "agent", "installed but not loaded in launchd (launchd has no job x)", "bilgie install-agent", Fail},
		{"scheduler error", func(in *Input) { in.AgentErr = errors.New("boom") },
			"agent", "cannot query", "PATH", Warn},
		{"no runs while installed", func(in *Input) { in.Runs = nil },
			"last run", "no runs recorded yet", "auto --dry-run", Warn},
		{"no runs, no agent", func(in *Input) { in.Runs = nil; in.Agent = auto.AgentState{} },
			"last run", "no runs recorded yet", "", Note},
		{"run error", func(in *Input) { in.Runs[0].Error = "read free space: eof"; in.LogPath = "/l/auto.log" },
			"last run", "run error: read free space: eof", "/l/auto.log", Fail},
		{"provider error", func(in *Input) {
			in.Runs[0].Providers = []auto.ProviderRecord{{Name: "go", Status: auto.StatusError, Error: "x"}}
		}, "last run", "; failed: go", "--verbose", Fail},
		{"stale agent", func(in *Input) { in.Runs[0].Time = fixedNow.Add(-5 * time.Hour) },
			"schedule", "5h ago but the interval is 30m0s", "install-agent", Fail},
		{"low space", func(in *Input) { in.Free.Free = 20 * gib },
			"free space", "tier low", "bilgie auto", Warn},
		{"critical space", func(in *Input) { in.Free.Free = 8 * gib },
			"free space", "tier critical", "bilgie status", Fail},
		{"emergency space", func(in *Input) { in.Free.Free = 2 * gib },
			"free space", "tier emergency", "bilgie status", Fail},
		{"no tick on an installed agent", func(in *Input) { in.Tick = auto.TickState{} },
			"last tick", "no tick recorded", "install-agent", Warn},
		{"no tick without an agent", func(in *Input) { in.Tick = auto.TickState{}; in.Agent.Installed = false },
			"last tick", "no tick recorded yet", "", Note},
		{"ticks stopped", func(in *Input) { in.Tick.Time = fixedNow.Add(-10 * time.Minute) },
			"last tick", "the agent is not ticking", "install-agent", Fail},
		{"one missed tick is fine", func(in *Input) { in.Tick.Time = fixedNow.Add(-5 * time.Minute) },
			"last tick", "5m ago", "", OK},
		{"cadence is reported", func(*Input) {},
			"cadence", "tick every 2m0s, full pass every 30m0s", "", OK},
		{"next pass counts down", func(*Input) {},
			"next full pass", "in 10m while space is healthy", "", OK},
		{"next pass overdue", func(in *Input) { in.Pass.Time = fixedNow.Add(-2 * time.Hour) },
			"next full pass", "due at the next tick", "", OK},
		{"future pass time is ignored", func(in *Input) { in.Pass.Time = fixedNow.Add(48 * time.Hour) },
			"next full pass", "at the next tick", "", OK},
		{"future tick time is not a tick", func(in *Input) { in.Tick.Time = fixedNow.Add(48 * time.Hour) },
			"last tick", "no tick recorded", "install-agent", Warn},
		{"no pass recorded", func(in *Input) { in.Pass = auto.PassState{} },
			"next full pass", "at the next tick", "", OK},
		{"free unreadable", func(in *Input) { in.FreeErr = errors.New("statfs") },
			"free space", "cannot read free space: statfs", "mounted", Warn},
		{"shrinking fast", func(in *Input) {
			in.Runs = []auto.RunRecord{{Time: fixedNow.Add(-48 * time.Hour), Tier: "ok", FreeBefore: 260 * gib}}
			in.Free.Free = 200 * gib
		}, "trend", "-60 GiB over 2d", "bilgie status", Warn},
		{"stable trend", func(in *Input) {
			in.Runs = []auto.RunRecord{{Time: fixedNow.Add(-48 * time.Hour), Tier: "ok", FreeBefore: 205 * gib}}
		}, "trend", "-5.0 GiB over 2d", "", OK},
		{"disabled providers", func(in *Input) { in.Cfg = testConfig("gradle", "yarn") },
			"config", "2 provider(s) disabled: gradle, yarn", "", Note},
		{"protected conflict", func(in *Input) {
			in.Conflicts = []auto.Conflict{{Provider: "go", Reason: "protected path /home/u/Downloads/go"}}
		}, "config", "go: protected path /home/u/Downloads/go; auto never cleans it", "providers.go.enabled: false", Warn},
		{"protection check cancelled", func(in *Input) {
			in.Conflicts = []auto.Conflict{{Provider: "go", Reason: "protection check cancelled before this provider was verified", Incomplete: true}}
		}, "config", "protection check incomplete: protection check cancelled before this provider was verified", "run doctor again", Warn},
		{"notifier missing linux", func(in *Input) { in.NotifierErr = errors.New("not found") },
			"notifier", "notify-send not found", "libnotify", Warn},
		{"notifier missing windows", func(in *Input) {
			in.GOOS, in.NotifierProgram, in.NotifierErr = "windows", "powershell.exe", errors.New("nf")
		}, "notifier", "powershell.exe not found", "PATH", Warn},
		{"notifier ok", func(*Input) {}, "notifier", "notify-send is available", "", OK},
		{"first run pending", func(in *Input) { in.FirstRunPending = true },
			"first run", "still a dry-run", "", Note},
		{"corrupt log", func(in *Input) { in.Corrupt = 3 },
			"history", "3 unreadable line(s)", "", Note},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := healthy()
			tt.mutate(&in)
			f := find(t, Diagnose(in), tt.area)
			assert.Equal(t, tt.level, f.Level, f.Message)
			assert.Contains(t, f.Message, tt.contains)
			assert.Contains(t, f.Hint, tt.hint)
		})
	}
}

func TestDiagnose_AlwaysSkippedProvider(t *testing.T) {
	skip := func(reason string) []auto.ProviderRecord {
		return []auto.ProviderRecord{
			{Name: "gradle", Status: auto.StatusSkipped, Reason: reason},
			{Name: "go", Status: auto.StatusSkipped, Reason: "within limit"},
		}
	}
	run := func(age time.Duration, providers []auto.ProviderRecord) auto.RunRecord {
		return auto.RunRecord{Time: fixedNow.Add(-age), Tier: "low", FreeBefore: 200 * gib, Providers: providers}
	}

	t.Run("unavailable tool is only a note", func(t *testing.T) {
		in := healthy()
		in.Runs = []auto.RunRecord{run(2*time.Hour, skip("unavailable")), run(time.Hour, skip("unavailable")), run(20*time.Minute, skip("unavailable"))}
		r := Diagnose(in)
		assert.Zero(t, r.Attention())
		assert.True(t, slices.ContainsFunc(r.Findings, func(f Finding) bool {
			return f.Level == Note && strings.Contains(f.Message, "gradle was skipped on each of the last 3 runs: unavailable")
		}))
	})

	t.Run("two runs are too few", func(t *testing.T) {
		in := healthy()
		in.Runs = []auto.RunRecord{run(time.Hour, skip("cannot measure size: x")), run(20*time.Minute, skip("cannot measure size: x"))}
		assert.Zero(t, Diagnose(in).Attention())
	})

	t.Run("same odd reason on every run", func(t *testing.T) {
		in := healthy()
		in.Runs = []auto.RunRecord{run(2*time.Hour, skip("cannot measure size: x")), run(time.Hour, skip("cannot measure size: x")), run(20*time.Minute, skip("cannot measure size: x"))}
		r := Diagnose(in)
		require.Equal(t, 1, r.Attention())
		var f Finding
		for _, c := range r.Findings {
			if c.Level == Warn {
				f = c
			}
		}
		assert.Equal(t, "gradle was skipped on each of the last 3 runs: cannot measure size: x", f.Message)
		assert.Contains(t, f.Hint, "bilgie auto --dry-run --verbose")
	})

	t.Run("recovered since", func(t *testing.T) {
		in := healthy()
		in.Runs = []auto.RunRecord{run(2*time.Hour, skip("unavailable")), run(20*time.Minute, []auto.ProviderRecord{{Name: "gradle", Status: auto.StatusCleaned}})}
		assert.Zero(t, Diagnose(in).Attention())
	})

	t.Run("within limit is routine", func(t *testing.T) {
		in := healthy()
		in.Runs = []auto.RunRecord{run(time.Hour, skip("within limit")), run(20*time.Minute, skip("within limit"))}
		assert.Zero(t, Diagnose(in).Attention())
	})

	t.Run("a protected conflict is not reported twice", func(t *testing.T) {
		in := healthy()
		reason := "protected path /x/Downloads"
		in.Runs = []auto.RunRecord{run(2*time.Hour, skip(reason)), run(time.Hour, skip(reason)), run(20*time.Minute, skip(reason))}
		in.Conflicts = []auto.Conflict{{Provider: "gradle", Reason: reason}}
		assert.Equal(t, 1, Diagnose(in).Attention())
	})
}

func TestDiagnose_ConfigLoadFailure(t *testing.T) {
	in := healthy()
	in.Cfg, in.ConfigErr = nil, errors.New("validate config: bad")
	r := Diagnose(in)
	f := find(t, r, "config")
	assert.Equal(t, Fail, f.Level)
	assert.Contains(t, f.Message, "validate config: bad")
	assert.Positive(t, r.Attention())
}

func TestFreedCountsOnlyDeletingRunsInTheWindow(t *testing.T) {
	in := healthy()
	in.Runs = []auto.RunRecord{
		{Time: fixedNow.Add(-10 * 24 * time.Hour), FreedBytes: 50 * gib},
		{Time: fixedNow.Add(-3 * time.Hour), FreedBytes: 7 * gib, DryRun: true},
		{Time: fixedNow.Add(-2 * time.Hour), FreedBytes: 2 * gib},
		{Time: fixedNow.Add(-20 * time.Minute), FreedBytes: gib},
	}
	assert.Contains(t, find(t, Diagnose(in), "freed").Message, "3.0 GiB freed by 2 run(s)")
}

func TestStalenessIgnoresManualDryRuns(t *testing.T) {
	in := healthy()
	in.Runs = []auto.RunRecord{
		{Time: fixedNow.Add(-48 * time.Hour), Tier: "ok"},
		{Time: fixedNow.Add(-time.Hour), Tier: "ok", DryRun: true},
	}
	assert.Equal(t, Fail, find(t, Diagnose(in), "schedule").Level)
}

func TestTrendIgnoresDryRunsWithAssumedFreeSpace(t *testing.T) {
	in := healthy()
	in.Runs = []auto.RunRecord{{Time: fixedNow.Add(-time.Minute), Tier: "critical", DryRun: true, FreeBefore: 3 * gib}}
	for _, f := range Diagnose(in).Findings {
		assert.NotEqual(t, "trend", f.Area)
	}
}

func TestZeroByteRunsAreNotCounted(t *testing.T) {
	in := healthy()
	in.Runs = []auto.RunRecord{
		{Time: fixedNow.Add(-3 * time.Hour), FreedBytes: gib},
		{Time: fixedNow.Add(-2 * time.Hour)},
		{Time: fixedNow.Add(-time.Hour), Error: "x"},
	}
	assert.Contains(t, find(t, Diagnose(in), "freed").Message, "1.0 GiB freed by 1 run(s)")
}

func TestLastRunFailureIsNotMaskedByManualDryRun(t *testing.T) {
	in := healthy()
	in.Runs = []auto.RunRecord{
		{Time: fixedNow.Add(-40 * time.Minute), Tier: "low", Error: "boom"},
		{Time: fixedNow.Add(-time.Minute), Tier: "low", DryRun: true},
	}
	f := find(t, Diagnose(in), "last run")
	assert.Equal(t, Fail, f.Level)
	assert.Contains(t, f.Message, "run error: boom")
}

func TestOnlyDryRunsWarnsOnceFirstRunIsDone(t *testing.T) {
	in := healthy()
	in.Runs = []auto.RunRecord{{Time: fixedNow.Add(-time.Minute), Tier: "low", DryRun: true}}
	for _, f := range Diagnose(in).Findings {
		assert.NotEqual(t, "schedule", f.Area, "a fresh forced first run is not a problem yet")
	}

	in.Runs[0].Time = fixedNow.Add(-2 * time.Hour)
	assert.Equal(t, Warn, find(t, Diagnose(in), "schedule").Level)

	in.FirstRunPending = true
	for _, f := range Diagnose(in).Findings {
		assert.NotEqual(t, "schedule", f.Area)
	}
}

func TestUnusableStateFilesAreFlagged(t *testing.T) {
	in := healthy()
	in.Tick = auto.TickState{}
	in.TickErr = fmt.Errorf("%w: tick.json is corrupt", auto.ErrBadState)
	in.PassErr = fmt.Errorf("%w: pass.json is not a regular file", auto.ErrBadState)
	r := Diagnose(in)

	tick := find(t, r, "last tick")
	assert.Equal(t, Warn, tick.Level)
	assert.Contains(t, tick.Message, "tick.json is corrupt")
	assert.NotContains(t, tick.Message, "no tick recorded")
	assert.Contains(t, find(t, r, "next full pass").Message, "pass.json is not a regular file")
	assert.Equal(t, Warn, find(t, r, "next full pass").Level)
}

func TestCapLoweringMinFreePctIsNoted(t *testing.T) {
	in := healthy()
	in.Cfg.Auto.MinFreePct = 15
	in.Cfg.Auto.MinFreeCap = "100G"
	in.Free = auto.FreeSpace{Free: 900 * gib, Total: 2000 * gib}
	f := find(t, Diagnose(in), "min free")
	assert.Equal(t, Note, f.Level)
	assert.Contains(t, f.Message, "min_free_pct 15%")
	assert.Contains(t, f.Message, "min_free_cap 100 GiB")

	in.Free = auto.FreeSpace{Free: 200 * gib, Total: 500 * gib}
	for _, f := range Diagnose(in).Findings {
		assert.NotEqual(t, "min free", f.Area, "75G is under the cap")
	}
}

func TestShortTrendSpanDoesNotProjectARunway(t *testing.T) {
	in := healthy()
	in.Runs = []auto.RunRecord{{Time: fixedNow.Add(-90 * time.Minute), Tier: "ok", FreeBefore: 260 * gib}}
	assert.Equal(t, OK, find(t, Diagnose(in), "trend").Level)
}

func TestUnreadableRunLogIsAFinding(t *testing.T) {
	in := healthy()
	in.Runs, in.RunsErr = nil, errors.New("read run log: permission denied")
	f := find(t, Diagnose(in), "history")
	assert.Equal(t, Fail, f.Level)
	assert.Contains(t, f.Hint, "readable")
}

func TestAge(t *testing.T) {
	assert.Equal(t, "under a minute", Age(10*time.Second))
	assert.Equal(t, "5m", Age(5*time.Minute))
	assert.Equal(t, "47h", Age(47*time.Hour))
	assert.Equal(t, "3d", Age(72*time.Hour))
	assert.Equal(t, "under a minute", Age(-time.Hour))
}

func TestDiagnose_UnverifiableConflictHintsAtNarrowerPaths(t *testing.T) {
	for _, reason := range []string{"too large to verify: /x/cache", "cannot verify: /x/cache/locked"} {
		in := healthy()
		in.Conflicts = []auto.Conflict{{Provider: "go-mod", Reason: reason}}

		f := find(t, Diagnose(in), "config")

		assert.Contains(t, f.Message, reason)
		assert.Contains(t, f.Hint, "narrower directories")
	}
}

func TestDiagnose_ProviderLoadErrorIsNamed(t *testing.T) {
	in := healthy()
	in.LoadErrors = []error{&provider.LoadError{Name: "sail-dirs", Err: errors.New(`path "sail/*" must be absolute or start with ~/`)}}
	r := Diagnose(in)
	f := find(t, r, "config")
	assert.Equal(t, Fail, f.Level)
	assert.Contains(t, f.Message, `provider sail-dirs: path "sail/*" must be absolute`)
	assert.Contains(t, f.Hint, "providers.sail-dirs")
	assert.Positive(t, r.Attention())
}
