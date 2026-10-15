package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/smykla-skalski/bilgie/pkg/size"
	"github.com/spf13/cobra"
)

// autoRunTimeout caps one unattended run so a stuck provider cannot pile up runs.
const autoRunTimeout = 45 * time.Minute

// AutoCmd trims caches based on free disk space.
var AutoCmd = &cobra.Command{
	Use:   "auto",
	Short: "Trim caches based on free disk space (for unattended runs)",
	Long: `Reads free space of the data volume and picks a pressure tier:

  ok         free space at or above the low threshold: smart-trim only providers over their limit
  low        below max(min_free, min(min_free_pct of the volume, min_free_cap)): smart-trim every
             enabled provider, cheapest to rebuild first, stopping once free space recovers
  critical   below critical_free (default 10G): the same trim, scheduled at the short cooldown,
             with the sail-dirs sweep first (sweep_tier: critical, even when disabled)
  emergency  below emergency_free (default 5G): also sweep every other stale directory
             (dir-pattern providers, even when disabled)

The docker-volumes and xcode-archives providers never run. Providers pointing at Downloads, opencode
or worktrees are skipped. The first run after install-agent is a dry-run.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runAuto,
}

// InstallAgentCmd installs the scheduled job that runs auto.
var InstallAgentCmd = &cobra.Command{
	Use:   "install-agent",
	Short: "Install the scheduled job that runs tick periodically",
	Long: `Registers tick (every auto.tick_interval) with the scheduler of this OS: a launchd agent on macOS, a systemd user timer
on Linux (a crontab entry when no systemd user manager is running) and a Task Scheduler task on Windows.
An agent installed by an earlier version, which ran auto every 30 minutes, is replaced.
The first run after install is a dry-run.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runInstallAgent,
}

// UninstallAgentCmd removes the scheduled job.
var UninstallAgentCmd = &cobra.Command{
	Use:          "uninstall-agent",
	Short:        "Remove the scheduled job installed by install-agent",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runUninstallAgent,
}

func init() {
	AutoCmd.Flags().Bool("dry-run", false, "Preview without deleting")
	AutoCmd.Flags().Bool("verbose", false, "List every entry instead of a per-provider summary")
	AutoCmd.Flags().String("assume-free", "", "Pretend this much space is free (e.g. 3G) to exercise the tiers; implies --dry-run")
	_ = AutoCmd.Flags().MarkHidden("assume-free")
}

// autoEnv holds everything auto touches outside the config, so tests can
// replace the disk, the state directory and the schedulers.
type autoEnv struct {
	free        auto.FreeFunc
	newProvider func(name string, cfg config.Provider) (provider.Provider, error)
	exec        auto.Executor
	notify      auto.Notifier
	now         func() time.Time
	out         io.Writer
	stateDir    string
	home        string
	exe         string
	uid         int
	goos        string
	configDir   string
	verbose     bool
	lookPath    func(string) (string, error)
	memory      auto.MemoryFunc
	processes   auto.ProcessesFunc
}

func defaultAutoEnv() (autoEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return autoEnv{}, fmt.Errorf("get home dir: %w", err)
	}
	stateDir, err := auto.StateDir()
	if err != nil {
		return autoEnv{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return autoEnv{}, fmt.Errorf("find own binary: %w", err)
	}
	return autoEnv{
		free:        auto.StatfsFree(auto.DataVolumePath()),
		newProvider: provider.NewProvider,
		exec:        auto.ExecCommand,
		notify:      auto.DefaultNotifier(auto.ExecCommand),
		memory:      osshim.ReadMemory,
		processes:   osshim.ProcessTable,
		now:         time.Now,
		lookPath:    exec.LookPath,
		out:         os.Stdout,
		stateDir:    stateDir,
		home:        home,
		exe:         exe,
		uid:         os.Getuid(),
		configDir:   userConfigDir(home),
	}, nil
}

// userConfigDir is where systemd looks for user units: XDG_CONFIG_HOME when
// it is an absolute path (the XDG rule), else ~/.config.
func userConfigDir(home string) string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(home, ".config")
}

func runAuto(cmd *cobra.Command, _ []string) error {
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
	return runAutoWithLoader(ctx, config.NewLoader(), env, dryRun)
}

func assumedFree(value string, statfs auto.FreeFunc) (auto.FreeFunc, error) {
	free, err := size.ParseSize(value)
	if err != nil {
		return nil, fmt.Errorf("--assume-free: %w", err)
	}
	return func() (auto.FreeSpace, error) {
		fs, err := statfs()
		if err != nil {
			return auto.FreeSpace{}, err
		}
		fs.Free = free
		return fs, nil
	}, nil
}

func runAutoWithLoader(ctx context.Context, loader *config.Loader, env autoEnv, dryRun bool) error {
	cfg, err := loadValidConfig(loader)
	if err != nil {
		return err
	}

	lock, ok, err := auto.AcquireRunLock(env.stateDir)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(env.out, "another auto run is in progress, exiting")
		return nil
	}
	defer lock.Release()

	return env.pass(ctx, cfg, dryRun, passOptions{preview: dryRun})
}

// passOptions carries what a tick knows about why it started a pass.
type passOptions struct {
	preview   bool
	minTier   auto.Tier
	predicted bool
}

// pass runs one full pass. The caller holds the run lock.
func (e autoEnv) pass(ctx context.Context, cfg *config.Config, dryRun bool, opts passOptions) error {
	forced := auto.FirstRunPending(e.stateDir) && !dryRun
	if forced {
		fmt.Fprintln(e.out, "first run after install-agent: dry-run, nothing is deleted")
		dryRun = true
	}

	ctx, cancel := context.WithTimeout(ctx, autoRunTimeout)
	defer cancel()

	report, err := auto.Run(ctx, cfg, dryRun, auto.Deps{
		Free:        e.free,
		NewProvider: e.newProvider,
		Out:         e.out,
		Home:        e.home,
		Verbose:     e.verbose,
		MinTier:     opts.minTier,
		Predicted:   opts.predicted,
	})
	clearsFirstRun := forced && err == nil && report.Previewed() && report.Err() == nil
	e.recordRun(ctx, cfg.Auto, report, err, opts.preview, clearsFirstRun)
	if err != nil {
		return err
	}

	if clearsFirstRun {
		if clearErr := auto.ClearFirstRun(e.stateDir); clearErr != nil {
			return clearErr
		}
	}
	return report.Err()
}

func (e autoEnv) clock() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
}

// recordRun notifies when space is still low, appends the run record and
// stores the pass state the tick schedules from. The dry-run that completes
// the first-run preview and a run that was interrupted or failed before
// finishing are not stamped as done, so they never delay the next real pass. None of these failures
// changes the run's outcome: they are reported and the run keeps its own result.
func (e autoEnv) recordRun(ctx context.Context, cfg config.Auto, report auto.Report, runErr error, preview, clearsFirstRun bool) {
	pass, err := auto.ReadPassState(e.stateDir)
	e.healState(auto.PassStateName, err, preview)
	notified := false
	if runErr == nil {
		notifyCtx, cancel := context.WithTimeout(ctx, auto.NotifyTimeout)
		defer cancel()
		notified, err = auto.NotifyLimited(notifyCtx, e.notify, report, cfg, &pass, e.clock())
		switch {
		case errors.Is(err, auto.ErrNotifierUnavailable):
			fmt.Fprintf(e.out, "notification skipped: %v\n", err)
		case err != nil:
			fmt.Fprintf(e.out, "warning: %v\n", err)
		}
	}
	now := e.clock()
	rec := auto.NewRunRecord(report, now, runErr, notified)
	if e.memory != nil {
		if mem, memErr := e.memory(ctx); memErr == nil {
			rec.SwapUsedMiB = auto.SwapUsedMiB(mem)
		}
	}
	if err := auto.AppendRun(e.stateDir, rec); err != nil {
		fmt.Fprintf(e.out, "warning: record run: %v\n", err)
	}
	if preview || (runErr != nil && !errors.Is(runErr, context.DeadlineExceeded)) {
		return
	}
	if !clearsFirstRun {
		pass.Time, pass.Tier, pass.DryRun = now.UTC(), report.Tier.String(), report.DryRun
	}
	if err := auto.WritePassState(e.stateDir, pass); err != nil {
		fmt.Fprintf(e.out, "warning: record pass: %v\n", err)
	}
}

func runInstallAgent(_ *cobra.Command, _ []string) error {
	env, err := defaultAutoEnv()
	if err != nil {
		return err
	}
	ctx, stop := interruptContext()
	defer stop()
	return runInstallAgentWithLoader(ctx, config.NewLoader(), env)
}

func runInstallAgentWithLoader(ctx context.Context, loader *config.Loader, env autoEnv) error {
	cfg, err := loader.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	interval, err := cfg.Auto.TickIntervalDuration()
	if err != nil {
		return err
	}
	if err := env.agent(interval).Install(ctx); err != nil {
		return err
	}
	noteSuggestions(ctx, cfg, env)
	return nil
}

// suggestBudget bounds the discover lookup after an install, so a slow
// protection check never delays the command.
const suggestBudget = 10 * time.Second

// noteSuggestions prints how many providers discover offers to enable. It only
// suggests: nothing is enabled and a failure here never fails the install.
func noteSuggestions(ctx context.Context, cfg *config.Config, env autoEnv) {
	lookPath := env.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	ctx, cancel := context.WithTimeout(ctx, suggestBudget)
	defer cancel()
	names := suggestionNames(ctx, cfg, env.home, lookPath, env.newProvider)
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(env.out, "discover: %d provider(s) could be enabled (%s); nothing was changed, run: bilgie discover\n",
		len(names), strings.Join(names, ", "))
}

func runUninstallAgent(_ *cobra.Command, _ []string) error {
	env, err := defaultAutoEnv()
	if err != nil {
		return err
	}
	ctx, stop := interruptContext()
	defer stop()
	return env.agent(0).Uninstall(ctx)
}

func (e autoEnv) agent(interval time.Duration) auto.Agent {
	return auto.Agent{
		Exec:      e.exec,
		Out:       e.out,
		Home:      e.home,
		Exe:       e.exe,
		StateDir:  e.stateDir,
		UID:       e.uid,
		Interval:  interval,
		OS:        e.goos,
		ConfigDir: e.configDir,
	}
}
