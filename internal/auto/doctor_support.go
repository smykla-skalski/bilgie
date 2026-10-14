package auto

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
)

// Scheduler backends reported by Agent.Status.
const (
	BackendLaunchd = "launchd"
	BackendSystemd = "systemd"
	BackendCron    = "cron"
	BackendTask    = "Task Scheduler"
)

// AgentState is what the OS scheduler knows about the periodic job.
type AgentState struct {
	Backend   string
	Detail    string
	Installed bool
	Loaded    bool
}

// LogFile is where the scheduled job writes its output.
func (a Agent) LogFile() string { return a.logPath() }

// Status asks the scheduler of the agent's OS whether the job is installed
// and loaded. It only reads: nothing is installed, started or removed.
func (a Agent) Status(ctx context.Context) (AgentState, error) {
	switch a.goos() {
	case goosDarwin:
		return a.statusLaunchd(ctx), nil
	case goosLinux:
		return a.statusLinux(ctx)
	case goosWindows:
		return a.statusTask(ctx)
	default:
		return AgentState{}, fmt.Errorf("agent status is not supported on %s", a.goos())
	}
}

func (a Agent) statusLaunchd(ctx context.Context) AgentState {
	st := AgentState{Backend: BackendLaunchd, Installed: fileExists(a.PlistPath())}
	target := a.domainTarget() + "/" + a.id().label
	out, err := a.Exec(ctx, "launchctl", "print", target)
	switch {
	case err == nil:
		st.Loaded = true
	case jobNotLoaded(string(out)) || exitCode(err) == serviceNotFoundExit:
		st.Detail = "launchd has no job " + a.id().label
	default:
		st.Detail = fmt.Sprintf("launchctl print: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return st
}

func (a Agent) statusLinux(ctx context.Context) (AgentState, error) {
	if fileExists(a.TimerPath()) || fileExists(a.ServicePath()) {
		st := AgentState{Backend: BackendSystemd, Installed: true}
		out, err := a.systemctl(ctx, "is-active", a.id().unit+".timer")
		state := strings.TrimSpace(string(out))
		st.Loaded = err == nil && state == "active"
		if !st.Loaded {
			st.Detail = "timer state: " + cmpOr(state, "unknown")
		}
		return st, nil
	}

	current, err := a.currentCrontab(ctx)
	if err != nil {
		if binaryMissing(err) {
			return AgentState{Backend: BackendCron}, nil
		}
		return AgentState{Backend: BackendCron}, err
	}
	_, found := withoutCronEntry(current, a.id().cronTag)
	return AgentState{Backend: BackendCron, Installed: found, Loaded: found}, nil
}

func (a Agent) statusTask(ctx context.Context) (AgentState, error) {
	out, err := a.Exec(ctx, "schtasks", "/Query", "/FO", "CSV", "/NH")
	if err != nil {
		return AgentState{Backend: BackendTask}, fmt.Errorf("schtasks query: %w: %s", err, strings.TrimSpace(string(out)))
	}
	reader := csv.NewReader(strings.NewReader(string(out)))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	records, err := reader.ReadAll()
	if err != nil {
		return AgentState{Backend: BackendTask}, fmt.Errorf("parse schtasks query: %w", err)
	}
	for _, rec := range records {
		if len(rec) == 0 || !strings.EqualFold(strings.TrimPrefix(rec[0], `\`), a.id().task) {
			continue
		}
		st := AgentState{Backend: BackendTask, Installed: true, Loaded: true}
		if a.taskDisabled(ctx, rec) {
			st.Loaded = false
			st.Detail = "task is disabled"
		}
		return st, nil
	}
	return AgentState{Backend: BackendTask}, nil
}

// taskDisabled reads the Enabled setting from the task definition, which is
// the same in every Windows language. Only when the definition cannot be read
// does it fall back to the English status text of the listing row.
func (a Agent) taskDisabled(ctx context.Context, row []string) bool {
	out, err := a.Exec(ctx, "schtasks", "/Query", "/TN", a.id().task, "/XML")
	if err == nil {
		if enabled, ok := taskEnabled(out); ok {
			return !enabled
		}
	}
	return len(row) > 2 && strings.EqualFold(strings.TrimSpace(row[2]), "Disabled")
}

// taskEnabled extracts Settings/Enabled from a task definition. ok is false
// when the text is not a task definition.
func taskEnabled(definition []byte) (enabled, ok bool) {
	if len(definition) >= 2 && definition[0] == 0xFF && definition[1] == 0xFE {
		units := make([]uint16, 0, len(definition)/2)
		for i := 2; i+1 < len(definition); i += 2 {
			units = append(units, uint16(definition[i])|uint16(definition[i+1])<<8)
		}
		definition = []byte(string(utf16.Decode(units)))
	}
	var task struct {
		XMLName xml.Name `xml:"Task"`
		Enabled *bool    `xml:"Settings>Enabled"`
	}
	dec := xml.NewDecoder(bytes.NewReader(definition))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := dec.Decode(&task); err != nil {
		return false, false
	}
	return task.Enabled == nil || *task.Enabled, true
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// NotifierProgram names the program the notifier of goos runs, or "" where
// there is no notifier.
func NotifierProgram(goos string) string {
	switch goos {
	case goosDarwin:
		return "osascript"
	case goosLinux:
		return "notify-send"
	case goosWindows:
		return "powershell.exe"
	default:
		return ""
	}
}

// Conflict is an enabled provider that auto will never run because its path
// is protected.
type Conflict struct {
	Provider string
	Reason   string
	// Incomplete marks a finding that is not a conflict: the check was
	// cancelled at Provider, so it and every later provider were not verified.
	Incomplete bool
}

// ProtectionConflicts lists the enabled providers whose paths are protected,
// so auto skips them on every run.
func ProtectionConflicts(
	ctx context.Context,
	cfg *config.Config,
	home string,
	newProvider func(name string, cfg config.Provider) (provider.Provider, error),
) []Conflict {
	protected := ProtectedPaths(cfg, home)
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	slices.Sort(names)

	var out []Conflict
	for _, name := range names {
		pc := cfg.Providers[name]
		if !pc.Enabled || !cfg.Applies(name) || neverRun(name, pc) || !config.ProviderPathsExist(name, pc) {
			continue
		}
		p, err := newProvider(name, pc)
		if err != nil {
			continue
		}
		if _, aware := p.(provider.ProtectionAware); aware {
			continue
		}
		var (
			reason string
			ctxErr error
		)
		if g, guarded := p.(provider.PathGuarded); guarded {
			reason, ctxErr = guardPaths(ctx, p, g, home, protected)
		} else {
			reason, ctxErr = protectedReason(ctx, p, pc.Type == config.TypeDirPattern, home, protected)
		}
		if ctxErr != nil {
			out = append(out, Conflict{
				Provider:   name,
				Reason:     "protection check cancelled before this provider and the ones after it were verified",
				Incomplete: true,
			})
			break
		}
		if reason != "" {
			out = append(out, Conflict{Provider: name, Reason: reason})
		}
	}
	return out
}

// LoadErrors lists the enabled providers that fail to load from their
// config, so auto reports an error for them on every run.
func LoadErrors(
	cfg *config.Config,
	newProvider func(name string, cfg config.Provider) (provider.Provider, error),
) []error {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	slices.Sort(names)

	var out []error
	for _, name := range names {
		pc := cfg.Providers[name]
		if !pc.Enabled || !cfg.Applies(name) || excluded(name, pc) {
			continue
		}
		if !hasTargets(name, pc) {
			continue
		}
		if _, err := newProvider(name, pc); err != nil {
			out = append(out, err)
		}
	}
	return out
}
