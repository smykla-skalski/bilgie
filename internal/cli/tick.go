package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/pkg/size"
	"github.com/spf13/cobra"
)

// TickCmd is the scheduled entry point: a cheap free-space check that starts
// a full pass only when the free-space level, the forecast or the routine
// interval calls for one.
var TickCmd = &cobra.Command{
	Use:   "tick",
	Short: "Check free space and start a cleanup pass when it is needed (for the scheduler)",
	Long: `Reads free space with one statfs call and decides, without walking any directory:

  healthy    nothing to do, unless the routine pass is due (auto.interval)
  low        a full pass, at most every auto.low_cooldown
  critical   a full pass, at most every auto.critical_cooldown, with the sail-dirs and
             sail-e2e-dirs sweeps first
  emergency  the same, with all the stale-directory sweeps first
  falling    a pass when the median recent rate of change projects the low threshold
             inside auto.forecast

Thresholds ease with hysteresis so space hovering at a limit does not flap. A tick exits at once
when another pass holds the run lock, and never deletes while the first-run dry-run is pending.
A healthy tick prints nothing; --verbose explains the decision.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runTick,
}

func init() {
	TickCmd.Flags().Bool("dry-run", false, "Preview without deleting")
	TickCmd.Flags().Bool("verbose", false, "Explain the decision and list every entry")
	TickCmd.Flags().String("assume-free", "", "Pretend this much space is free (e.g. 3G) to exercise the tiers; implies --dry-run")
	_ = TickCmd.Flags().MarkHidden("assume-free")
}

func runTick(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	assume, _ := cmd.Flags().GetString("assume-free")
	verbose, _ := cmd.Flags().GetBool("verbose")

	env, err := defaultAutoEnv()
	if err != nil {
		return err
	}
	env.verbose = verbose
	if assume != "" {
		free, err := assumedFree(assume, env.free)
		if err != nil {
			return err
		}
		env.free = free
		dryRun = true
	}

	ctx, stop := interruptContext()
	defer stop()
	return runTickWithLoader(ctx, config.NewLoader(), env, dryRun)
}

func runTickWithLoader(ctx context.Context, loader *config.Loader, env autoEnv, dryRun bool) error {
	cfg, err := loadValidConfig(loader)
	if err != nil {
		return err
	}
	limits, err := cfg.Auto.Limits()
	if err != nil {
		return err
	}

	fs, err := env.free()
	if err != nil {
		return fmt.Errorf("read free space: %w", err)
	}
	now := env.clock()

	tick, err := auto.ReadTickState(env.stateDir)
	env.healState(auto.TickStateName, err, dryRun)
	pass, err := auto.ReadPassState(env.stateDir)
	env.healState(auto.PassStateName, err, dryRun)

	d := auto.Decide(auto.TickInput{Now: now, Free: fs, Limits: limits, Tick: tick, Pass: pass})
	state := auto.TickState{
		Time: now.UTC(), Tier: d.Tier.String(), Action: auto.ActionIdle, Reason: d.Reason,
		Samples: d.Samples, Free: fs.Free, Total: fs.Total,
	}
	if d.Run {
		state.Action = auto.ActionPass
	}
	state.SwapNotified = tick.SwapNotified
	env.checkSwap(ctx, &state, limits, now, dryRun)
	if !dryRun {
		env.saveTick(state)
	}

	if !d.Run {
		env.explainf("tick: free %s of %s, %s", size.FormatSize(fs.Free), size.FormatSize(fs.Total), d.Reason)
		return nil
	}

	lock, ok, err := auto.AcquireRunLock(env.stateDir)
	if err != nil {
		return err
	}
	if !ok {
		state.Action, state.Reason = auto.ActionSkip, "another pass holds the run lock"
		if !dryRun {
			env.saveTick(state)
		}
		env.explainf("tick: %s", state.Reason)
		return nil
	}
	defer lock.Release()

	now = env.clock()
	pass, err = auto.ReadPassState(env.stateDir)
	env.healState(auto.PassStateName, err, dryRun)
	d = auto.Decide(auto.TickInput{Now: now, Free: fs, Limits: limits, Tick: tick, Pass: pass})
	if !d.Run {
		state.Action, state.Reason = auto.ActionIdle, d.Reason
		if !dryRun {
			env.saveTick(state)
		}
		env.explainf("tick: %s", d.Reason)
		return nil
	}

	env.explainf("tick: free %s of %s, %s", size.FormatSize(fs.Free), size.FormatSize(fs.Total), d.Reason)
	err = env.pass(ctx, cfg, dryRun, passOptions{minTier: d.Tier, predicted: d.Predicted, preview: dryRun})
	if !dryRun {
		state.Time = env.clock().UTC()
		env.saveTick(state)
	}
	return err
}

// healState reports a state file that cannot be used and moves it aside, so
// the unattended tick carries on with the empty state instead of failing on
// the same file forever. The file is checked again first, because a pass
// holding the run lock may have replaced it since it was read. A dry-run
// only reports.
func (e autoEnv) healState(name string, readErr error, dryRun bool) {
	if readErr == nil {
		return
	}
	if dryRun {
		fmt.Fprintf(e.out, "warning: %v; left in place by the dry-run\n", readErr)
		return
	}
	if auto.CheckState(e.stateDir, name) == nil {
		return
	}
	moved, err := auto.QuarantineState(e.stateDir, name)
	if err != nil {
		fmt.Fprintf(e.out, "warning: %v; could not move it aside: %v\n", readErr, err)
		return
	}
	fmt.Fprintf(e.out, "warning: %v; moved aside to %s\n", readErr, moved)
}

func (e autoEnv) saveTick(state auto.TickState) {
	if err := auto.WriteTickState(e.stateDir, state); err != nil {
		fmt.Fprintf(e.out, "warning: record tick: %v\n", err)
	}
}

func (e autoEnv) explainf(format string, args ...any) {
	if e.verbose {
		fmt.Fprintf(e.out, format+"\n", args...)
	}
}

// checkSwap records swap in the tick and sends one notification per
// cooldown while it stays above auto.swap_warn. A swap that cannot be read
// is skipped: it must never stop the free-space check. A dry-run only reads.
func (e autoEnv) checkSwap(ctx context.Context, state *auto.TickState, limits config.Limits, now time.Time, dryRun bool) {
	if e.memory == nil {
		return
	}
	mem, err := e.memory(ctx)
	if err != nil {
		e.explainf("tick: swap not read: %v", err)
		return
	}
	state.SwapUsed = auto.SwapUsedBytes(mem)
	if dryRun {
		return
	}
	notifyCtx, cancel := context.WithTimeout(ctx, auto.NotifyTimeout)
	defer cancel()
	sent, err := auto.NotifySwap(notifyCtx, e.notify, mem, limits, state.SwapNotified, now)
	switch {
	case errors.Is(err, auto.ErrNotifierUnavailable):
		fmt.Fprintf(e.out, "notification skipped: %v\n", err)
		state.SwapNotified = now.UTC()
	case err != nil:
		fmt.Fprintf(e.out, "warning: %v\n", err)
	}
	if sent {
		state.SwapNotified = now.UTC()
	}
}
