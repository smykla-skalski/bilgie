package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/doctor"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/spf13/cobra"
)

// DoctorCmd checks that the agent works and the config lets it act.
var DoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that the agent runs and the config lets it clean",
	Long: `Checks, without changing anything:

  agent      installed and loaded in the scheduler of this OS
  cadence    tick and full-pass intervals, the last tick and the next expected full pass
  last run   time, tier, bytes freed and errors from runs.jsonl
  free space current free space and its 7-day trend from the run history
  config     disabled providers, providers skipped on every recent run, protected-path conflicts
  memory     swap in use against auto.swap_warn, with the largest process families when it is high
  notifier   whether the low-space notification program exists
  discover   a note naming the providers bilgie discover would offer to enable

Exits non-zero when a finding needs attention; each one says what to do.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runDoctor,
}

func runDoctor(_ *cobra.Command, _ []string) error {
	env, err := defaultAutoEnv()
	if err != nil {
		return err
	}
	ctx, stop := interruptContext()
	defer stop()
	return runDoctorWithLoader(ctx, config.NewLoader(), env)
}

func runDoctorWithLoader(ctx context.Context, loader *config.Loader, env autoEnv) error {
	goos := env.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	now := env.now
	if now == nil {
		now = time.Now
	}
	lookPath := env.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	in := doctor.Input{Now: now(), GOOS: goos, NotifierProgram: auto.NotifierProgram(goos)}

	cfg, cfgErr := loadValidConfig(loader)
	in.Cfg, in.ConfigErr = cfg, cfgErr

	agent := env.agent(0)
	in.LogPath = agent.LogFile()
	in.Agent, in.AgentErr = agent.Status(ctx)

	in.LinkedPaths = agent.LinkedPaths()
	in.Runs, in.Corrupt, in.RunsErr = auto.ReadRuns(env.stateDir, 0)
	in.FirstRunPending = auto.FirstRunPending(env.stateDir)
	in.Tick, in.TickErr = auto.ReadTickState(env.stateDir)
	in.Pass, in.PassErr = auto.ReadPassState(env.stateDir)

	in.Free, in.FreeErr = env.free()

	if in.NotifierProgram != "" {
		_, in.NotifierErr = lookPath(in.NotifierProgram)
	}
	if cfg != nil {
		in.Conflicts = auto.ProtectionConflicts(ctx, cfg, env.home, env.newProvider)
		in.LoadErrors = auto.LoadErrors(cfg, env.newProvider)
		in.Suggested = suggestionNames(ctx, cfg, env.home, lookPath, env.newProvider)
	}

	fillMemory(ctx, &in, env)

	rep := doctor.Diagnose(in)
	rep.Write(env.out)
	if n := rep.Attention(); n > 0 {
		return fmt.Errorf("doctor: %d finding(s) need attention", n)
	}
	return nil
}

const goosWindows = "windows"

// fillMemory samples swap and, only while it is above auto.swap_warn, the
// process table that names the largest families.
func fillMemory(ctx context.Context, in *doctor.Input, env autoEnv) {
	if env.memory == nil {
		return
	}
	in.Mem, in.MemErr = env.memory(ctx)
	if in.MemErr != nil {
		return
	}
	in.MemRead = true
	if in.Cfg == nil || env.processes == nil {
		return
	}
	limits, err := in.Cfg.Auto.Limits()
	if err != nil || !auto.SwapHigh(in.Mem, limits.SwapWarn) {
		return
	}
	if env.goos == goosWindows || (env.goos == "" && runtime.GOOS == goosWindows) {
		in.FamiliesErr = errors.New("the process list has no full command lines on Windows")
		return
	}
	var procs []osshim.Process
	if procs, in.FamiliesErr = env.processes(ctx); in.FamiliesErr == nil {
		in.Families = auto.ProcessFamilies(procs)
	}
}

func loadValidConfig(loader *config.Loader) (*config.Config, error) {
	cfg, err := loader.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}
