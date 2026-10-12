package auto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/appname"
)

// agentCommand is the subcommand the scheduler runs. Agents installed before
// the tick existed run `auto` every 30 minutes; install-agent replaces them.
const agentCommand = "tick"

const (
	agentName   = appname.Name
	labelPrefix = "dev.mskalski."
)

const (
	AgentLabel  = labelPrefix + agentName
	SystemdUnit = agentName
	CronTag     = agentName
	TaskName    = agentName
)

// identity names the scheduler entries of one project name.
type identity struct {
	label, unit, cronTag, task string
}

var (
	currentIdentity = identity{AgentLabel, SystemdUnit, CronTag, TaskName}
)

const (
	goosDarwin  = "darwin"
	goosLinux   = "linux"
	goosWindows = "windows"
)

// Executor runs an external command; tests inject a recorder so nothing
// reaches a real scheduler or notifier.
type Executor func(ctx context.Context, name string, args ...string) ([]byte, error)

// killGrace bounds how long a command may keep its output pipe open after
// its context ends, so a stuck grandchild cannot hold the caller.
const killGrace = 2 * time.Second

// ExecCommand is the Executor that runs the real command.
func ExecCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = killGrace
	return cmd.CombinedOutput()
}

func binaryMissing(err error) bool {
	return errors.Is(err, exec.ErrNotFound)
}

// Agent installs and removes the periodic `tick` job with the scheduler of
// its OS: launchd, a systemd user timer (cron as fallback) or Task Scheduler.
// Interval is the tick interval. An empty OS means the running OS, a nil Now means time.Now and an empty
// ConfigDir means Home/.config; systemd looks for user units in ConfigDir.
type Agent struct {
	Exec       Executor
	Out        io.Writer
	Home       string
	Exe        string
	StateDir   string
	UID        int
	Interval   time.Duration
	RetryDelay time.Duration
	OS         string
	ConfigDir  string
	Now        func() time.Time
}

func (a Agent) id() identity {
	return currentIdentity
}

func (a Agent) goos() string {
	if a.OS == "" {
		return runtime.GOOS
	}
	return a.OS
}

func (a Agent) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}

func (a Agent) logPath() string {
	if a.goos() == goosDarwin {
		return filepath.Join(a.Home, "Library", "Logs", agentName, "auto.log")
	}
	return filepath.Join(a.StateDir, "auto.log")
}

// Install arms the first-run dry-run marker, then registers the job with the
// scheduler of the agent's OS.
func (a Agent) Install(ctx context.Context) error {
	goos := a.goos()
	if !supportedOS(goos) {
		return fmt.Errorf("install-agent is not supported on %s", goos)
	}
	if strings.Contains(a.Exe, "go-build") {
		return fmt.Errorf("refusing to install a temporary `go run` binary (%s); build or install %s first", a.Exe, agentName)
	}
	if !isAbsPath(goos, a.Exe) {
		return fmt.Errorf("binary path must be absolute, got %q", a.Exe)
	}
	alreadyPending := FirstRunPending(a.StateDir)
	if err := MarkFirstRunPending(a.StateDir); err != nil {
		return err
	}

	var err error
	switch goos {
	case goosDarwin:
		err = a.installLaunchd(ctx)
	case goosLinux:
		err = a.installLinux(ctx)
	default:
		err = a.installTask(ctx)
	}
	if err != nil {
		if !alreadyPending {
			_ = ClearFirstRun(a.StateDir)
		}
		return err
	}
	fmt.Fprintln(a.Out, "the first run is a dry-run; later runs delete")
	return nil
}

func (a Agent) uninstallJob(ctx context.Context) (bool, error) {
	switch a.goos() {
	case goosDarwin:
		return a.uninstallLaunchd(ctx)
	case goosLinux:
		return a.uninstallLinux(ctx)
	default:
		return a.uninstallTask(ctx)
	}
}

func supportedOS(goos string) bool {
	return goos == goosDarwin || goos == goosLinux || goos == goosWindows
}

// Uninstall removes everything Install created for this OS and clears the
// first-run marker.
func (a Agent) Uninstall(ctx context.Context) error {
	if goos := a.goos(); !supportedOS(goos) {
		return fmt.Errorf("uninstall-agent is not supported on %s", goos)
	}
	removed, err := a.uninstallJob(ctx)
	if err != nil {
		return err
	}
	if err := ClearFirstRun(a.StateDir); err != nil {
		return err
	}
	for _, name := range []string{TickStateName, PassStateName} {
		if _, err := removeIfExists(filepath.Join(a.StateDir, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	if !removed {
		fmt.Fprintln(a.Out, "agent was not installed")
	}
	return nil
}

// isAbsPath judges a path by the rules of the target OS, not the running one.
func isAbsPath(goos, p string) bool {
	if goos != goosWindows {
		return strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	if len(p) < 3 || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
		return false
	}
	letter := p[0] | 0x20
	return letter >= 'a' && letter <= 'z'
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func removeIfExists(path string) (bool, error) {
	err := os.Remove(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create dir for %s: %w", filepath.Base(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("install %s: %w", filepath.Base(path), err)
	}
	return nil
}
