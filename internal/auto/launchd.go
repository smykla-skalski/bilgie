package auto

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const bootstrapAttempts = 5

// ErrIntervalTooLong reports an interval launchd's 32-bit StartInterval cannot hold.
var ErrIntervalTooLong = errors.New("interval too long for launchd")

// serviceNotFoundExit is the launchctl exit status for an unknown service.
const serviceNotFoundExit = 113

// PlistPath is where the launchd agent definition lives.
func (a Agent) PlistPath() string {
	return filepath.Join(a.Home, "Library", "LaunchAgents", a.id().label+".plist")
}

func (a Agent) domainTarget() string {
	return fmt.Sprintf("gui/%d", a.UID)
}

// unload asks launchd to drop the job. A job that was never loaded is the
// state the caller wants. When bootout fails in a form not recognised as
// "not loaded", a failing `launchctl print` confirms the job is absent; any other print failure keeps the error.
func (a Agent) unload(ctx context.Context) error {
	target := a.domainTarget() + "/" + a.id().label
	out, err := a.Exec(ctx, "launchctl", "bootout", target)
	if err == nil || jobNotLoaded(string(out)) {
		return nil
	}
	bootoutErr := fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
	if ctx.Err() != nil {
		return bootoutErr
	}
	printOut, printErr := a.Exec(ctx, "launchctl", "print", target)
	if printErr == nil || (!jobNotLoaded(string(printOut)) && exitCode(printErr) != serviceNotFoundExit) {
		return bootoutErr
	}
	return nil
}

func exitCode(err error) int {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return 0
}

func jobNotLoaded(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no such process") || strings.Contains(lower, "could not find") || strings.Contains(lower, "not found")
}

// bootstrap loads the job, retrying because bootout can return before launchd
// has finished unloading and the first bootstrap then fails with an I/O error.
func (a Agent) bootstrap(ctx context.Context) error {
	delay := a.RetryDelay
	if delay <= 0 {
		delay = time.Second
	}

	var lastErr error
	for attempt := range bootstrapAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		out, err := a.Exec(ctx, "launchctl", "bootstrap", a.domainTarget(), a.PlistPath())
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return lastErr
}

// installLaunchd writes the plist and loads it, unloading any earlier
// registration first since bootstrap fails on a job that is already loaded.
func (a Agent) installLaunchd(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(a.logPath()), 0o750); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	plist, err := RenderPlist(a.Exe, a.Home, a.logPath(), a.Interval)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.PlistPath(), plist); err != nil {
		return err
	}

	_ = a.unload(ctx)
	if err := a.bootstrap(ctx); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "installed %s (every %s)\n", a.PlistPath(), a.Interval)
	return nil
}

func (a Agent) uninstallLaunchd(ctx context.Context) (bool, error) {
	if err := a.unload(ctx); err != nil {
		return false, err
	}
	removed, err := removeIfExists(a.PlistPath())
	if err != nil {
		return false, fmt.Errorf("remove plist: %w", err)
	}
	if removed {
		fmt.Fprintf(a.Out, "removed %s\n", a.PlistPath())
	}
	return removed, nil
}

// agentPath is the PATH launchd gives the job. launchd carries none of the
// user's shell setup, yet providers shell out to go, brew, docker and friends.
func agentPath(home string) string {
	return strings.Join([]string{
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/usr/local/bin",
		path.Join(home, ".local", "share", "mise", "shims"),
		path.Join(home, "go", "bin"),
		path.Join(home, ".cargo", "bin"),
		path.Join(home, ".docker", "bin"),
		"/Applications/Docker.app/Contents/Resources/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}, ":")
}

func plistString(s string) string {
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(s))
	return "<string>" + esc.String() + "</string>"
}

// RenderPlist builds the launchd property list that runs `exe tick` every interval.
func RenderPlist(exe, home, logPath string, interval time.Duration) ([]byte, error) {
	seconds := int64(interval / time.Second)
	if seconds < 1 {
		return nil, fmt.Errorf("interval must be at least one second, got %s", interval)
	}
	if seconds > math.MaxInt32 {
		return nil, fmt.Errorf("%w: %s is %d seconds, launchd StartInterval holds at most %d",
			ErrIntervalTooLong, interval, seconds, math.MaxInt32)
	}

	entries := []struct{ key, value string }{
		{"Label", plistString(AgentLabel)},
		{"ProgramArguments", "<array>\n\t\t" + plistString(exe) + "\n\t\t" + plistString(agentCommand) + "\n\t</array>"},
		{"StartInterval", fmt.Sprintf("<integer>%d</integer>", seconds)},
		{"RunAtLoad", "<true/>"},
		{"ProcessType", plistString("Background")},
		{"LowPriorityIO", "<true/>"},
		{"Nice", "<integer>10</integer>"},
		{"StandardOutPath", plistString(logPath)},
		{"StandardErrorPath", plistString(logPath)},
		{"EnvironmentVariables", "<dict>\n\t\t<key>PATH</key>\n\t\t" + plistString(agentPath(home)) + "\n\t</dict>"},
	}

	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	for _, e := range entries {
		fmt.Fprintf(&b, "\t<key>%s</key>\n\t%s\n", e.key, e.value)
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}
