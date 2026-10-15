// Package doctor checks that the unattended agent works and that the
// configuration lets it do something useful. Diagnose is pure: every fact it
// needs (clock, free space, scheduler state, run history) arrives in Input.
package doctor

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// Level ranks a finding. Warn and Fail need attention and make doctor exit
// non-zero; Note is informational.
type Level int

// Finding levels.
const (
	OK Level = iota
	Note
	Warn
	Fail
)

func (l Level) String() string {
	switch l {
	case OK:
		return "ok"
	case Note:
		return "note"
	case Warn:
		return "warn"
	default:
		return "FAIL"
	}
}

// TrendWindow is how far back the free-space trend looks.
const TrendWindow = 7 * 24 * time.Hour

// staleFactor intervals without a run mean the agent is not running.
const staleFactor = 3

// skipRuns recent runs must all skip a provider for it to count as always skipped.
const skipRuns = 5

const minSkipRuns = 3

// runwayDays warns when the trend reaches min_free sooner than this.
const runwayDays = 14

const minTrendSpan = 24 * time.Hour

const withinLimit = "within limit"

// Finding is one check result.
type Finding struct {
	Area    string
	Message string
	Hint    string
	Details []string
	Level   Level
}

// Input holds everything Diagnose looks at. Cfg is nil when the config could
// not be loaded and ConfigErr says why. Runs are oldest first. NotifierErr is
// set when the notifier program cannot be found.
type Input struct {
	Now time.Time

	Cfg       *config.Config
	ConfigErr error

	Runs    []auto.RunRecord
	RunsErr error
	Corrupt int

	Agent    auto.AgentState
	AgentErr error
	LogPath  string

	Free    auto.FreeSpace
	FreeErr error

	FirstRunPending bool
	// LinkedPaths are state files or the log directory that are symbolic
	// links; bilgie refuses to write through them.
	LinkedPaths []string

	// MemRead is set when memory was sampled; MemErr says why it could not be.
	// Families and FamiliesErr are only filled while swap is above the threshold.
	Mem         osshim.Memory
	MemRead     bool
	MemErr      error
	Families    []auto.Family
	FamiliesErr error

	Tick auto.TickState
	Pass auto.PassState
	// TickErr and PassErr are set when the state file exists but cannot be
	// used (corrupt, unreadable, not a regular file).
	TickErr error
	PassErr error

	NotifierProgram string
	NotifierErr     error

	Conflicts []auto.Conflict
	// Suggested names the providers discover offers for enabling.
	Suggested []string
	// LoadErrors are enabled providers whose config does not load; each
	// reads "provider <name>: <reason>".
	LoadErrors []error
	GOOS       string
}

// Report is the ordered list of findings.
type Report struct {
	Findings []Finding
}

// Attention counts the findings that need the user to act.
func (r Report) Attention() int {
	n := 0
	for _, f := range r.Findings {
		if f.Level >= Warn {
			n++
		}
	}
	return n
}

// Diagnose runs every check.
func Diagnose(in Input) Report {
	findings := []Finding{agentFinding(in)}
	findings = append(findings, linkFindings(in)...)
	findings = append(findings, cadenceFindings(in)...)
	findings = append(findings, lastRunFindings(in)...)
	findings = append(findings, firstRunFinding(in))
	findings = append(findings, freeSpaceFindings(in)...)
	findings = append(findings, memoryFindings(in)...)
	findings = append(findings, configFindings(in)...)
	findings = append(findings, discoverFindings(in)...)
	findings = append(findings, notifierFinding(in))
	return Report{Findings: findings}
}

func agentFinding(in Input) Finding {
	f := Finding{Area: "agent"}
	switch {
	case in.AgentErr != nil:
		f.Level = Warn
		f.Message = fmt.Sprintf("cannot query the %s scheduler: %v", in.Agent.Backend, in.AgentErr)
		f.Hint = "check that the scheduler tool is on PATH, then run doctor again"
	case !in.Agent.Installed:
		f.Level = Fail
		f.Message = "not installed: nothing runs bilgie automatically"
		f.Hint = "run: bilgie install-agent"
	case !in.Agent.Loaded:
		f.Level = Fail
		f.Message = fmt.Sprintf("installed but not loaded in %s", in.Agent.Backend)
		if in.Agent.Detail != "" {
			f.Message += " (" + in.Agent.Detail + ")"
		}
		f.Hint = "run: bilgie install-agent (it reloads the job)"
	default:
		f.Message = fmt.Sprintf("installed and loaded (%s)", in.Agent.Backend)
	}
	return f
}

func linkFindings(in Input) []Finding {
	out := make([]Finding, 0, len(in.LinkedPaths))
	for _, p := range in.LinkedPaths {
		out = append(out, Finding{
			Area: "state", Level: Warn,
			Message: p + " is a symbolic link: bilgie refuses to write through it",
			Hint:    "delete the link, then run: bilgie install-agent",
		})
	}
	return out
}

// tickStaleFactor missed ticks mean the scheduler is not running the agent.
const tickStaleFactor = 3

func cadenceFindings(in Input) []Finding {
	if in.Cfg == nil {
		return nil
	}
	limits, err := in.Cfg.Auto.Limits()
	if err != nil {
		return nil
	}

	out := []Finding{{
		Area: "cadence",
		Message: fmt.Sprintf("tick every %s, full pass every %s (low %s, critical %s)",
			limits.TickInterval, limits.Interval, limits.LowCooldown, limits.CriticalCooldown),
	}}

	tick := Finding{Area: "last tick"}
	switch {
	case in.TickErr != nil:
		tick.Level = Warn
		tick.Message = in.TickErr.Error()
		tick.Hint = "the next tick moves it aside (" + auto.TickStateName + ".bad) and carries on; or delete it"
	case (in.Tick.Time.IsZero() || in.Tick.Time.After(in.Now)) && in.Agent.Installed:
		tick.Level = Warn
		tick.Message = "no tick recorded: the installed agent may still run the old 30-minute auto cadence"
		tick.Hint = "run: bilgie install-agent"
	case in.Tick.Time.IsZero() || in.Tick.Time.After(in.Now):
		tick.Level = Note
		tick.Message = "no tick recorded yet"
	default:
		age := in.Now.Sub(in.Tick.Time)
		tick.Message = fmt.Sprintf("%s ago (tier %s, %s)", Age(age), in.Tick.Tier, in.Tick.Reason)
		if in.Agent.Installed && age > tickStaleFactor*limits.TickInterval {
			tick.Level = Fail
			tick.Message += fmt.Sprintf("; ticks should come every %s: the agent is not ticking", limits.TickInterval)
			tick.Hint = hintLog(in.LogPath, "run: bilgie install-agent to reinstall it")
		}
	}
	next := Finding{Area: "next full pass", Message: nextPass(in, limits.Interval)}
	if in.PassErr != nil {
		next.Level = Warn
		next.Message = in.PassErr.Error()
		next.Hint = "the next pass moves it aside (" + auto.PassStateName + ".bad) and carries on; or delete it"
	}
	out = append(out, tick, next)
	if f, ok := capFinding(in); ok {
		out = append(out, f)
	}
	return out
}

// capFinding notes when min_free_cap keeps min_free_pct from taking effect,
// so an explicit percentage is not silently lowered.
func capFinding(in Input) (Finding, bool) {
	limits, err := in.Cfg.Auto.Limits()
	if err != nil || in.FreeErr != nil || limits.MinFreePct <= 0 || in.Free.Total <= 0 {
		return Finding{}, false
	}
	pct := int64(float64(in.Free.Total) * limits.MinFreePct / 100)
	if pct <= limits.MinFreeCap {
		return Finding{}, false
	}
	return Finding{
		Area: "min free", Level: Note,
		Message: fmt.Sprintf("min_free_pct %g%% of this volume is %s, lowered to min_free_cap %s",
			limits.MinFreePct, size.FormatSize(pct), size.FormatSize(limits.MinFreeCap)),
		Hint: "raise auto.min_free_cap to apply the full percentage",
	}, true
}

func nextPass(in Input, interval time.Duration) string {
	if in.Pass.Time.IsZero() || in.Pass.Time.After(in.Now) {
		return "at the next tick (no pass recorded yet)"
	}
	due := in.Pass.Time.Add(interval)
	if !due.After(in.Now) {
		return "due at the next tick while space is healthy"
	}
	return fmt.Sprintf("in %s while space is healthy, sooner if free space is low or falling", Age(due.Sub(in.Now)))
}

func lastRunFindings(in Input) []Finding {
	if in.RunsErr != nil {
		return []Finding{{
			Area: "history", Level: Fail,
			Message: "cannot read the run log: " + in.RunsErr.Error(),
			Hint:    "check that the state directory and runs.jsonl are readable by your user",
		}}
	}
	if len(in.Runs) == 0 {
		level := Note
		if in.Agent.Installed {
			level = Warn
		}
		out := []Finding{{
			Area: "last run", Level: level,
			Message: "no runs recorded yet",
			Hint:    "wait for the first scheduled run, or try: bilgie auto --dry-run",
		}}
		return append(out, extraRunNotes(in)...)
	}

	last, deleting := newestRun(in.Runs)
	mode := ""
	if last.DryRun {
		mode = ", dry-run"
	}
	var failed []string
	for _, p := range last.Providers {
		if p.Status == auto.StatusError {
			failed = append(failed, p.Name)
		}
	}
	main := Finding{
		Area: "last run",
		Message: fmt.Sprintf("%s ago (tier %s, freed %s, %d provider error(s)%s)",
			Age(in.Now.Sub(last.Time)), last.Tier, size.FormatSize(last.FreedBytes), len(failed), mode),
	}

	switch {
	case last.Error != "":
		main.Level = Fail
		main.Message += "; run error: " + last.Error
		main.Hint = hintLog(in.LogPath, "run: bilgie auto --dry-run --verbose to reproduce")
	case len(failed) > 0:
		main.Level = Fail
		main.Message += "; failed: " + strings.Join(failed, ", ")
		main.Hint = hintLog(in.LogPath, "run: bilgie auto --dry-run --verbose to see each error")
	}

	out := []Finding{main}
	switch {
	case deleting:
		if f, ok := staleFinding(in, last); ok {
			out = append(out, f)
		}
	case in.Agent.Installed && !in.FirstRunPending && onlyDryRunsStale(in, last):
		out = append(out, Finding{
			Area: "schedule", Level: Warn,
			Message: "only dry-runs are recorded: the agent has not completed a deleting run",
			Hint:    hintLog(in.LogPath, "run: bilgie install-agent to reinstall it"),
		})
	}
	out = append(out, freedFinding(in))
	return append(out, extraRunNotes(in)...)
}

// onlyDryRunsStale holds back the dry-runs-only warning until the newest
// dry-run is older than staleFactor intervals: right after the forced first
// run the real pass is simply not due yet.
func onlyDryRunsStale(in Input, last auto.RunRecord) bool {
	if in.Cfg == nil {
		return true
	}
	interval, err := in.Cfg.Auto.IntervalDuration()
	if err != nil || interval <= 0 {
		return true
	}
	return in.Now.Sub(last.Time) > staleFactor*interval
}

func newestRun(runs []auto.RunRecord) (auto.RunRecord, bool) {
	for _, r := range slices.Backward(runs) {
		if !r.DryRun {
			return r, true
		}
	}
	return runs[len(runs)-1], false
}

func hintLog(logPath, fallback string) string {
	if logPath == "" {
		return fallback
	}
	return "see " + logPath + "; " + fallback
}

func staleFinding(in Input, last auto.RunRecord) (Finding, bool) {
	if in.Cfg == nil || !in.Agent.Installed {
		return Finding{}, false
	}
	interval, err := in.Cfg.Auto.IntervalDuration()
	if err != nil || interval <= 0 {
		return Finding{}, false
	}
	age := in.Now.Sub(last.Time)
	if age <= staleFactor*interval {
		return Finding{}, false
	}
	return Finding{
		Area:    "schedule",
		Level:   Fail,
		Message: fmt.Sprintf("last run was %s ago but the interval is %s: the agent is not running", Age(age), interval),
		Hint:    hintLog(in.LogPath, "run: bilgie install-agent to reinstall it"),
	}, true
}

func freedFinding(in Input) Finding {
	cutoff := in.Now.Add(-TrendWindow)
	var freed int64
	runs := 0
	for _, r := range in.Runs {
		if r.DryRun || r.Time.Before(cutoff) {
			continue
		}
		if r.FreedBytes > 0 {
			freed += r.FreedBytes
			runs++
		}
	}
	return Finding{
		Area:    "freed",
		Message: fmt.Sprintf("%s freed by %d run(s) in the last 7 days", size.FormatSize(freed), runs),
	}
}

func extraRunNotes(in Input) []Finding {
	var out []Finding
	if in.Corrupt > 0 {
		out = append(out, Finding{
			Area: "history", Level: Note,
			Message: fmt.Sprintf("%d unreadable line(s) in the run log were skipped", in.Corrupt),
		})
	}
	return out
}

// firstRunFinding always says whether the next real pass deletes or only
// previews, naming the command that flips it. Write prints hints only for
// warnings, so the command sits in the message.
func firstRunFinding(in Input) Finding {
	if in.FirstRunPending {
		return Finding{
			Area: "first run", Level: Note,
			Message: auto.ArmedDetail + " (run: bilgie disarm to let it delete)",
		}
	}
	return Finding{
		Area:    "first run",
		Message: auto.DisarmedDetail + " (run: bilgie arm to preview first)",
	}
}

func freeSpaceFindings(in Input) []Finding {
	if in.FreeErr != nil {
		return []Finding{{
			Area: "free space", Level: Warn,
			Message: "cannot read free space: " + in.FreeErr.Error(),
			Hint:    "check that the data volume is mounted and readable",
		}}
	}

	line := fmt.Sprintf("%s free of %s (%.0f%%)", size.FormatSize(in.Free.Free), size.FormatSize(in.Free.Total), percent(in.Free))
	f := Finding{Area: "free space", Message: line}
	if in.Cfg != nil {
		if tier, err := auto.ChooseTier(in.Free, in.Cfg.Auto); err == nil {
			f.Message += ", tier " + tier.String()
			switch tier {
			case auto.TierLow:
				f.Level = Warn
			case auto.TierCritical, auto.TierEmergency:
				f.Level = Fail
			case auto.TierOK:
			}
			if tier != auto.TierOK {
				f.Hint = "run: bilgie auto; run: bilgie status for large unmanaged directories"
			}
		}
	}

	out := []Finding{f}
	if t, ok := trendFinding(in); ok {
		out = append(out, t)
	}
	return out
}

func memoryFindings(in Input) []Finding {
	if in.MemErr != nil {
		return []Finding{{Area: "memory", Level: Note, Message: "cannot read swap and memory: " + in.MemErr.Error()}}
	}
	if !in.MemRead {
		return nil
	}
	m := in.Mem
	var warn int64
	if in.Cfg != nil {
		if limits, err := in.Cfg.Auto.Limits(); err == nil {
			warn = limits.SwapWarn
		}
	}

	msg := fmt.Sprintf("swap %s of %s used", size.FormatSize(auto.SwapUsedBytes(m)), size.FormatSize(auto.ClampBytes(m.SwapTotal)))
	switch {
	case m.FreePercent >= 0:
		msg += fmt.Sprintf(", memory free %d%%", m.FreePercent)
	case m.MemTotal > 0:
		msg += fmt.Sprintf(", %s of %s RAM available", size.FormatSize(auto.ClampBytes(m.MemAvailable)), size.FormatSize(auto.ClampBytes(m.MemTotal)))
	}
	f := Finding{Area: "memory", Message: msg}
	if !auto.SwapHigh(m, warn) {
		return []Finding{f}
	}

	f.Level = Warn
	f.Message += fmt.Sprintf(" (warning above %s)", size.FormatSize(warn))
	f.Hint = "quit or restart the largest memory users; bilgie only reports and never kills processes. Swap grows on the same disk as free space"
	switch {
	case in.FamiliesErr != nil:
		f.Details = []string{"cannot list processes: " + in.FamiliesErr.Error()}
	case len(in.Families) == 0:
		f.Details = []string{"no process family with many copies or orphans found; check Activity Monitor or top for the largest memory users"}
	default:
		f.Details = append(f.Details, "largest process families:")
		for _, fam := range in.Families {
			f.Details = append(f.Details, "  "+fam.String())
		}
	}
	return []Finding{f}
}

func percent(fs auto.FreeSpace) float64 {
	if fs.Total <= 0 {
		return 0
	}
	return float64(fs.Free) * 100 / float64(fs.Total)
}

func trendFinding(in Input) (Finding, bool) {
	cutoff := in.Now.Add(-TrendWindow)
	var window []auto.RunRecord
	for _, r := range in.Runs {
		if !r.DryRun && !r.Time.Before(cutoff) {
			window = append(window, r)
		}
	}
	if len(window) == 0 {
		return Finding{}, false
	}
	oldest := slices.MinFunc(window, func(a, b auto.RunRecord) int { return a.Time.Compare(b.Time) })
	delta := in.Free.Free - oldest.FreeBefore
	span := in.Now.Sub(oldest.Time)

	f := Finding{
		Area: "trend",
		Message: fmt.Sprintf("free space %s over %s (%s -> %s, %d run(s))",
			signedSize(delta), Age(span), size.FormatSize(oldest.FreeBefore), size.FormatSize(in.Free.Free), len(window)),
	}
	if delta < 0 && span >= minTrendSpan && in.Cfg != nil {
		if minFree, err := in.Cfg.Auto.MinFreeBytes(); err == nil && in.Free.Free > minFree {
			perDay := float64(-delta) / (float64(span) / float64(24*time.Hour))
			days := float64(in.Free.Free-minFree) / perDay
			if days < runwayDays {
				f.Level = Warn
				f.Message += fmt.Sprintf("; min_free is reached in about %.0f day(s) at this rate", days)
				f.Hint = "find what grows: bilgie status shows large unmanaged directories"
			}
		}
	}
	return f, true
}

func signedSize(n int64) string {
	if n < 0 {
		return "-" + size.FormatSize(-n)
	}
	return "+" + size.FormatSize(n)
}

func configFindings(in Input) []Finding {
	if in.Cfg == nil {
		msg := "config could not be loaded"
		if in.ConfigErr != nil {
			msg += ": " + in.ConfigErr.Error()
		}
		return []Finding{{
			Area: "config", Level: Fail, Message: msg,
			Hint: "fix the config file (bilgie config show prints the active one), then run doctor again",
		}}
	}

	var out []Finding
	if f, ok := disabledFinding(in.Cfg); ok {
		out = append(out, f)
	}

	for _, err := range in.LoadErrors {
		name := ""
		if le, ok := errors.AsType[*provider.LoadError](err); ok {
			name = le.Name
		}
		out = append(out, Finding{
			Area: "config", Level: Fail,
			Message: err.Error() + "; auto reports an error for it on every run",
			Hint:    fmt.Sprintf("fix providers.%s in the config, or set providers.%s.enabled: false", name, name),
		})
	}

	conflicted := make(map[string]bool)
	for _, c := range in.Conflicts {
		if c.Incomplete {
			out = append(out, Finding{
				Area: "config", Level: Warn,
				Message: "protection check incomplete: " + c.Reason,
				Hint:    "run doctor again to verify the remaining providers",
			})
			continue
		}
		conflicted[c.Provider] = true
		out = append(out, Finding{
			Area: "config", Level: Warn,
			Message: fmt.Sprintf("%s: %s; auto never cleans it", c.Provider, c.Reason),
			Hint:    skipHint(c.Provider, c.Reason),
		})
	}
	out = append(out, alwaysSkipped(in, conflicted)...)

	if !slices.ContainsFunc(out, func(f Finding) bool { return f.Level >= Warn }) {
		out = append(out, Finding{Area: "config", Message: "no provider problems found"})
	}
	return out
}

// suggestedShown caps the names a discover note lists.
const suggestedShown = 5

func discoverFindings(in Input) []Finding {
	if in.Cfg == nil || len(in.Suggested) == 0 {
		return nil
	}
	shown := in.Suggested
	more := ""
	if len(shown) > suggestedShown {
		more = fmt.Sprintf(" and %d more", len(shown)-suggestedShown)
		shown = shown[:suggestedShown]
	}
	return []Finding{{
		Area: "discover", Level: Note,
		Message: fmt.Sprintf("%d provider(s) could be enabled: %s%s; run: bilgie discover", len(in.Suggested), strings.Join(shown, ", "), more),
	}}
}

func disabledFinding(cfg *config.Config) (Finding, bool) {
	var names []string
	for name := range cfg.Providers {
		if !cfg.Providers[name].Enabled && cfg.Applies(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return Finding{}, false
	}
	slices.Sort(names)
	return Finding{
		Area: "config", Level: Note,
		Message: fmt.Sprintf("%d provider(s) disabled: %s", len(names), strings.Join(names, ", ")),
	}, true
}

// alwaysSkipped reports providers skipped for the same non-routine reason on
// each of the last runs. "within limit" is the normal idle answer.
func alwaysSkipped(in Input, conflicted map[string]bool) []Finding {
	recent := in.Runs
	if len(recent) > skipRuns {
		recent = recent[len(recent)-skipRuns:]
	}
	if len(recent) < minSkipRuns {
		return nil
	}

	reasons := make(map[string]string)
	for _, p := range recent[len(recent)-1].Providers {
		if p.Status == auto.StatusSkipped && p.Reason != withinLimit {
			reasons[p.Name] = p.Reason
		}
	}
	for _, r := range recent[:len(recent)-1] {
		for name, reason := range reasons {
			if !slices.ContainsFunc(r.Providers, func(p auto.ProviderRecord) bool {
				return p.Name == name && p.Status == auto.StatusSkipped && p.Reason == reason
			}) {
				delete(reasons, name)
			}
		}
	}

	names := make([]string, 0, len(reasons))
	for name := range reasons {
		if !conflicted[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	out := make([]Finding, 0, len(names))
	for _, name := range names {
		level := Warn
		if reasons[name] == "unavailable" {
			level = Note
		}
		out = append(out, Finding{
			Area: "config", Level: level,
			Message: fmt.Sprintf("%s was skipped on each of the last %d runs: %s", name, len(recent), reasons[name]),
			Hint:    skipHint(name, reasons[name]),
		})
	}
	return out
}

func skipHint(name, reason string) string {
	switch {
	case strings.HasPrefix(reason, "protected path"):
		return fmt.Sprintf("set providers.%s.enabled: false, or point its paths away from protected data", name)
	case strings.HasPrefix(reason, "too large to verify"), strings.HasPrefix(reason, "cannot verify"):
		return fmt.Sprintf("point providers.%s.paths at narrower directories, or set providers.%s.enabled: false", name, name)
	case reason == "unavailable":
		return fmt.Sprintf("install the tool %s needs, or set providers.%s.enabled: false", name, name)
	default:
		return fmt.Sprintf("run: bilgie auto --dry-run --verbose to see why %s is skipped", name)
	}
}

func notifierFinding(in Input) Finding {
	f := Finding{Area: "notifier"}
	switch {
	case in.NotifierProgram == "":
		f.Level = Warn
		f.Message = "no notifier exists for " + in.GOOS + ": low-space alerts will not be shown"
		f.Hint = "watch the run log or run: bilgie history"
	case in.NotifierErr != nil:
		f.Level = Warn
		f.Message = fmt.Sprintf("%s not found: low-space alerts will not be shown", in.NotifierProgram)
		f.Hint = notifierHint(in.GOOS, in.NotifierProgram)
	default:
		f.Message = in.NotifierProgram + " is available"
	}
	return f
}

func notifierHint(goos, program string) string {
	switch goos {
	case "linux":
		return "install libnotify (notify-send), for example: apt install libnotify-bin"
	case "windows":
		return "make sure powershell.exe is on PATH"
	default:
		return fmt.Sprintf("make sure %s is on PATH", program)
	}
}

// Write prints one line per finding with an indented hint for the ones that
// need attention, then a verdict line.
func (r Report) Write(w io.Writer) {
	for _, f := range r.Findings {
		fmt.Fprintf(w, "[%-4s] %s: %s\n", f.Level, f.Area, f.Message)
		if f.Level >= Warn {
			for _, d := range f.Details {
				fmt.Fprintf(w, "       %s\n", d)
			}
		}
		if f.Hint != "" && f.Level >= Warn {
			fmt.Fprintf(w, "       what to do: %s\n", f.Hint)
		}
	}
	if n := r.Attention(); n > 0 {
		fmt.Fprintf(w, "\n%d finding(s) need attention\n", n)
	} else {
		fmt.Fprintln(w, "\nall good")
	}
}

// Age formats a duration as its largest whole unit: 5m, 3h, 2d.
func Age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
