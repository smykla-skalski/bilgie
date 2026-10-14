package doctor

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withSwap(m osshim.Memory) func(*Input) {
	return func(in *Input) {
		in.Mem, in.MemRead = m, true
	}
}

func TestDiagnose_Memory(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Input)
		contains []string
		hint     string
		details  []string
		level    Level
	}{
		{
			name:     "memory unreadable is a note",
			mutate:   func(in *Input) { in.MemErr = errors.New("sysctl failed") },
			contains: []string{"cannot read swap and memory: sysctl failed"},
			level:    Note,
		},
		{
			name: "swap under the threshold with a macOS free percentage",
			mutate: withSwap(osshim.Memory{
				SwapUsed: 2 * uint64(gib), SwapTotal: 4 * uint64(gib), FreePercent: 35,
			}),
			contains: []string{"swap 2.0 GiB of 4.0 GiB used, memory free 35%"},
			level:    OK,
		},
		{
			name: "zero percent free memory is still a macOS reading",
			mutate: withSwap(osshim.Memory{
				SwapUsed: uint64(gib), SwapTotal: 2 * uint64(gib), FreePercent: 0,
			}),
			contains: []string{"swap 1.0 GiB of 2.0 GiB used, memory free 0%"},
			level:    OK,
		},
		{
			name: "swap under the threshold with Linux available memory",
			mutate: withSwap(osshim.Memory{
				SwapUsed: uint64(gib), SwapTotal: 2 * uint64(gib),
				MemAvailable: 6 * uint64(gib), MemTotal: 16 * uint64(gib), FreePercent: -1,
			}),
			contains: []string{"swap 1.0 GiB of 2.0 GiB used, 6.0 GiB of 16 GiB RAM available"},
			level:    OK,
		},
		{
			name: "swap under the threshold without a memory reading",
			mutate: withSwap(osshim.Memory{
				SwapUsed: uint64(gib), SwapTotal: 2 * uint64(gib), FreePercent: -1,
			}),
			contains: []string{"swap 1.0 GiB of 2.0 GiB used"},
			level:    OK,
		},
		{
			name: "high swap lists the largest process families",
			mutate: func(in *Input) {
				withSwap(osshim.Memory{SwapUsed: 20 * uint64(gib), SwapTotal: 24 * uint64(gib), FreePercent: 5})(in)
				in.Families = []auto.Family{{Command: "node server.js", Count: 12, Orphans: 4}}
			},
			contains: []string{"swap 20 GiB of 24 GiB used", "(warning above 8.0 GiB)"},
			hint:     "never kills processes",
			details:  []string{"largest process families:", "  12 x node server.js (4 with parent PID 1)"},
			level:    Warn,
		},
		{
			name: "high swap with no runaway family",
			mutate: func(in *Input) {
				withSwap(osshim.Memory{SwapUsed: 20 * uint64(gib), SwapTotal: 24 * uint64(gib), FreePercent: 5})(in)
			},
			contains: []string{"(warning above 8.0 GiB)"},
			hint:     "never kills processes",
			details:  []string{"no process family with many copies or orphans found; check Activity Monitor or top for the largest memory users"},
			level:    Warn,
		},
		{
			name: "high swap with the process list unavailable",
			mutate: func(in *Input) {
				withSwap(osshim.Memory{SwapUsed: 20 * uint64(gib), SwapTotal: 24 * uint64(gib), FreePercent: 5})(in)
				in.FamiliesErr = errors.New("ps failed")
			},
			contains: []string{"(warning above 8.0 GiB)"},
			hint:     "never kills processes",
			details:  []string{"cannot list processes: ps failed"},
			level:    Warn,
		},
		{
			name: "swap_warn 0 turns the warning off",
			mutate: func(in *Input) {
				withSwap(osshim.Memory{SwapUsed: 20 * uint64(gib), SwapTotal: 24 * uint64(gib), FreePercent: 5})(in)
				in.Cfg.Auto.SwapWarn = "0"
			},
			contains: []string{"swap 20 GiB of 24 GiB used"},
			level:    OK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := healthy()
			tt.mutate(&in)

			f := find(t, Diagnose(in), "memory")

			assert.Equal(t, tt.level, f.Level, f.Message)
			for _, want := range tt.contains {
				assert.Contains(t, f.Message, want)
			}
			if tt.hint == "" {
				assert.Empty(t, f.Hint)
			} else {
				assert.Contains(t, f.Hint, tt.hint)
			}
			for _, want := range tt.details {
				assert.Contains(t, f.Details, want)
			}
		})
	}
}

func TestDiagnose_NoMemoryFindingWhenNotSampled(t *testing.T) {
	for _, f := range Diagnose(healthy()).Findings {
		assert.NotEqual(t, "memory", f.Area)
	}
}

func TestDiagnose_OnlyDryRunsRecorded(t *testing.T) {
	dryRun := func(age time.Duration) []auto.RunRecord {
		return []auto.RunRecord{{
			Time: fixedNow.Add(-age), Tier: "low", DryRun: true,
			FreeBefore: 200 * gib, FreeAfter: 200 * gib,
		}}
	}
	tests := []struct {
		name   string
		mutate func(*Input)
		warns  bool
	}{
		{"stale dry-run warns", func(in *Input) { in.Runs = dryRun(2 * time.Hour) }, true},
		{"fresh dry-run waits for the real pass", func(in *Input) { in.Runs = dryRun(20 * time.Minute) }, false},
		{"first run still pending is not a failure", func(in *Input) {
			in.Runs, in.FirstRunPending = dryRun(2*time.Hour), true
		}, false},
		{"no config cannot judge the age, so it warns", func(in *Input) {
			in.Runs, in.Cfg = dryRun(20*time.Minute), nil
		}, true},
		{"unparsable interval cannot judge the age, so it warns", func(in *Input) {
			in.Runs = dryRun(20 * time.Minute)
			in.Cfg = &config.Config{Auto: config.Auto{Interval: "never"}}
		}, true},
		{"agent not installed has nothing to reinstall", func(in *Input) {
			in.Runs, in.Agent = dryRun(2*time.Hour), auto.AgentState{}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := healthy()
			tt.mutate(&in)

			var got bool
			for _, f := range Diagnose(in).Findings {
				if f.Level == Warn && f.Area == "schedule" {
					got = true
					assert.Contains(t, f.Message, "only dry-runs are recorded")
				}
			}
			assert.Equal(t, tt.warns, got)
		})
	}
}

func TestDiagnose_NotifierGaps(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Input)
		message string
		hint    string
		level   Level
	}{
		{"no notifier exists on the OS", func(in *Input) { in.GOOS, in.NotifierProgram = "plan9", "" },
			"no notifier exists for plan9", "bilgie history", Warn},
		{"macOS notifier missing", func(in *Input) {
			in.GOOS, in.NotifierProgram, in.NotifierErr = "darwin", "osascript", errors.New("nf")
		}, "osascript not found", "make sure osascript is on PATH", Warn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := healthy()
			tt.mutate(&in)

			f := find(t, Diagnose(in), "notifier")

			assert.Equal(t, tt.level, f.Level)
			assert.Contains(t, f.Message, tt.message)
			assert.Contains(t, f.Hint, tt.hint)
		})
	}
}

func TestLevel_String(t *testing.T) {
	assert.Equal(t, "ok", OK.String())
	assert.Equal(t, "note", Note.String())
	assert.Equal(t, "warn", Warn.String())
	assert.Equal(t, "FAIL", Fail.String())
}

func TestReport_WriteShowsDetailsOnlyForFindingsThatNeedAttention(t *testing.T) {
	r := Report{Findings: []Finding{
		{Area: "quiet", Level: Note, Message: "fine", Details: []string{"hidden detail"}, Hint: "hidden hint"},
		{Area: "loud", Level: Warn, Message: "bad", Details: []string{"shown detail"}, Hint: "shown hint"},
	}}

	var buf bytes.Buffer
	r.Write(&buf)

	out := buf.String()
	assert.NotContains(t, out, "hidden")
	assert.Contains(t, out, "       shown detail\n")
	assert.Contains(t, out, "       what to do: shown hint\n")
	assert.Contains(t, out, "1 finding(s) need attention")
}

func TestDiagnose_FreeSpacePercentIsZeroWithoutATotal(t *testing.T) {
	in := healthy()
	in.Free = auto.FreeSpace{Free: 20 * gib}

	f := find(t, Diagnose(in), "free space")

	require.NotEmpty(t, f.Message)
	assert.Contains(t, f.Message, "(0%)")
}
