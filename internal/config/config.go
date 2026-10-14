package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/pkg/size"
)

// Config holds bilgie configuration.
type Config struct {
	Providers map[string]Provider `mapstructure:"providers" yaml:"providers"`
	Version   string              `mapstructure:"version" yaml:"version"`
	Auto      Auto                `mapstructure:"auto" yaml:"auto"`
	// Protected lists paths auto never deletes and status reports for a human.
	// User entries are added to the built-in ones, never replace them.
	// Surrounding whitespace on an entry is trimmed on load; see MergeProtected.
	Protected []string `mapstructure:"protected" yaml:"protected,omitempty"`

	goos string
	// custom names user providers that reuse the name of a built-in that
	// belongs to another OS but define their own paths or command.
	custom []string
}

// Applies reports whether the named provider applies on the OS this config
// was built for. Providers stay in the file so a config shared between OSes
// keeps entries that only apply elsewhere.
func (c *Config) Applies(name string) bool {
	goos := c.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	return AppliesOn(name, goos) || slices.Contains(c.custom, name)
}

// DefaultProtected returns the built-in protected paths. Each is an exact
// location: auto never protects a directory just for sharing its name. The docker volumes
// path is the Linux location; Docker Desktop keeps volumes in a VM image
// that auto never prunes.
func DefaultProtected() []string {
	return []string{"~/Downloads", "~/.local/share/opencode", "/var/lib/docker/volumes"}
}

// MergeProtected returns the built-in protected paths followed by the extra
// ones. Entries are trimmed of surrounding whitespace before they are
// validated, so " /data/x " loads as "/data/x" and a blank entry is dropped.
// An entry that differs from an earlier one only by letter case or by the
// /System/Volumes/Data firmlink prefix is dropped: protection already
// compares case-insensitively and through the firmlink, so the twin adds
// nothing and only shows up twice in status.
func MergeProtected(extra []string) []string {
	out := DefaultProtected()
	seen := make(map[string]bool, len(out)+len(extra))
	for _, p := range out {
		seen[protectedKey(p)] = true
	}
	for _, p := range extra {
		p = strings.TrimSpace(p)
		if p == "" || seen[protectedKey(p)] {
			continue
		}
		seen[protectedKey(p)] = true
		out = append(out, p)
	}
	return out
}

func protectedKey(p string) string {
	return strings.ToLower(strings.TrimRight(stripDataAlias(strings.ReplaceAll(p, `\`, "/")), "/"))
}

// Auto configures the unattended tick and auto commands and the scheduled agent.
type Auto struct {
	// Interval is the full-pass interval: how often the agent runs a routine
	// trim when space is healthy (default 30m).
	Interval string `mapstructure:"interval" yaml:"interval"`
	// TickInterval is how often the agent wakes to read free space (default 2m, minimum 1m).
	TickInterval string `mapstructure:"tick_interval" yaml:"tick_interval"`
	// MinFree is the absolute free-space floor (default 30G).
	MinFree string `mapstructure:"min_free" yaml:"min_free"`
	// MinFreePct is the floor as a percentage of the volume (default 5, 0 disables).
	MinFreePct float64 `mapstructure:"min_free_pct" yaml:"min_free_pct"`
	// MinFreeCap caps the low threshold so a big disk is not permanently low (default 100G).
	MinFreeCap string `mapstructure:"min_free_cap" yaml:"min_free_cap"`
	// CriticalFree is the free space under which cleanup runs at the short cooldown (default 10G).
	CriticalFree string `mapstructure:"critical_free" yaml:"critical_free"`
	// EmergencyFree is the free space under which the stale-directory sweeps run (default 5G).
	EmergencyFree string `mapstructure:"emergency_free" yaml:"emergency_free"`
	// Hysteresis is how far above a threshold free space must climb before the tier eases (default 2G).
	Hysteresis string `mapstructure:"hysteresis" yaml:"hysteresis"`
	// LowCooldown is the minimum gap between passes at the low tier (default 10m).
	LowCooldown string `mapstructure:"low_cooldown" yaml:"low_cooldown"`
	// CriticalCooldown is the minimum gap between passes at the critical and emergency tiers (default 2m).
	CriticalCooldown string `mapstructure:"critical_cooldown" yaml:"critical_cooldown"`
	// Forecast is how far ahead a falling trend is projected; a pass runs when the
	// projection crosses the low threshold inside it (default 15m, 0 disables).
	Forecast string `mapstructure:"forecast" yaml:"forecast"`
	// NotifyCooldown is the minimum gap between notifications of one tier (default 3h).
	NotifyCooldown string `mapstructure:"notify_cooldown" yaml:"notify_cooldown"`
	// SwapWarn is the swap in use above which doctor warns and the agent
	// notifies, at most once per notify_cooldown (default 8G, 0 disables).
	SwapWarn string `mapstructure:"swap_warn" yaml:"swap_warn"`
}

// Auto defaults.
const (
	DefaultAutoInterval         = "30m"
	DefaultAutoTickInterval     = "2m"
	DefaultAutoMinFree          = "30G"
	DefaultAutoMinFreePct       = 5.0
	DefaultAutoMinFreeCap       = "100G"
	DefaultAutoCriticalFree     = "10G"
	DefaultAutoEmergencyFree    = "5G"
	DefaultAutoHysteresis       = "2G"
	DefaultAutoLowCooldown      = "10m"
	DefaultAutoCriticalCooldown = "2m"
	DefaultAutoForecast         = "15m"
	DefaultAutoNotifyCooldown   = "3h"
	DefaultAutoSwapWarn         = "8G"
)

// MinAutoInterval is the shortest accepted agent interval.
const MinAutoInterval = time.Minute

// DefaultAuto returns the auto settings used when the config omits them.
func DefaultAuto() Auto {
	return Auto{
		Interval:         DefaultAutoInterval,
		TickInterval:     DefaultAutoTickInterval,
		MinFree:          DefaultAutoMinFree,
		MinFreePct:       DefaultAutoMinFreePct,
		MinFreeCap:       DefaultAutoMinFreeCap,
		CriticalFree:     DefaultAutoCriticalFree,
		EmergencyFree:    DefaultAutoEmergencyFree,
		Hysteresis:       DefaultAutoHysteresis,
		LowCooldown:      DefaultAutoLowCooldown,
		CriticalCooldown: DefaultAutoCriticalCooldown,
		Forecast:         DefaultAutoForecast,
		NotifyCooldown:   DefaultAutoNotifyCooldown,
		SwapWarn:         DefaultAutoSwapWarn,
	}
}

// Resolved fills blank string settings with their defaults. MinFreePct is
// left alone: zero is a valid value that disables the percentage floor.
func (a Auto) Resolved() Auto {
	d := DefaultAuto()
	for _, f := range []struct {
		field *string
		def   string
	}{
		{&a.Interval, d.Interval}, {&a.TickInterval, d.TickInterval}, {&a.MinFree, d.MinFree},
		{&a.MinFreeCap, d.MinFreeCap}, {&a.CriticalFree, d.CriticalFree}, {&a.EmergencyFree, d.EmergencyFree},
		{&a.Hysteresis, d.Hysteresis}, {&a.LowCooldown, d.LowCooldown},
		{&a.CriticalCooldown, d.CriticalCooldown}, {&a.Forecast, d.Forecast}, {&a.NotifyCooldown, d.NotifyCooldown},
		{&a.SwapWarn, d.SwapWarn},
	} {
		if strings.TrimSpace(*f.field) == "" {
			*f.field = f.def
		}
	}
	return a
}

// IntervalDuration parses Interval.
func (a Auto) IntervalDuration() (time.Duration, error) {
	a = a.Resolved()
	d, err := ParseDuration(a.Interval)
	if err != nil {
		return 0, fmt.Errorf("interval: %w", err)
	}
	if d < MinAutoInterval {
		return 0, fmt.Errorf("interval must be at least %s, got %q", MinAutoInterval, a.Interval)
	}
	return d, nil
}

// TickIntervalDuration parses TickInterval. The schedulers repeat in whole minutes.
func (a Auto) TickIntervalDuration() (time.Duration, error) {
	a = a.Resolved()
	d, err := ParseDuration(a.TickInterval)
	if err != nil {
		return 0, fmt.Errorf("tick_interval: %w", err)
	}
	if d < MinAutoInterval || d%time.Minute != 0 {
		return 0, fmt.Errorf("tick_interval must be a whole number of minutes, at least %s, got %q", MinAutoInterval, a.TickInterval)
	}
	return d, nil
}

// MinFreeBytes parses MinFree.
func (a Auto) MinFreeBytes() (int64, error) {
	return a.Resolved().bytes("min_free", a.Resolved().MinFree)
}

func (a Auto) bytes(name, value string) (int64, error) {
	b, err := size.ParseSize(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if b < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %q", name, value)
	}
	return b, nil
}

func (a Auto) span(name, value string) (time.Duration, error) {
	d, err := ParseBudget(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

// Limits is Auto with every value parsed.
type Limits struct {
	MinFree          int64
	MinFreeCap       int64
	CriticalFree     int64
	EmergencyFree    int64
	Hysteresis       int64
	MinFreePct       float64
	Interval         time.Duration
	TickInterval     time.Duration
	LowCooldown      time.Duration
	CriticalCooldown time.Duration
	Forecast         time.Duration
	NotifyCooldown   time.Duration
	SwapWarn         int64
}

// Limits parses and checks every setting.
func (a Auto) Limits() (Limits, error) {
	a = a.Resolved()
	var (
		l    = Limits{MinFreePct: a.MinFreePct}
		errs []error
		err  error
	)
	for _, f := range []struct {
		out   *int64
		name  string
		value string
	}{
		{&l.MinFree, "min_free", a.MinFree}, {&l.MinFreeCap, "min_free_cap", a.MinFreeCap},
		{&l.CriticalFree, "critical_free", a.CriticalFree}, {&l.EmergencyFree, "emergency_free", a.EmergencyFree},
		{&l.Hysteresis, "hysteresis", a.Hysteresis}, {&l.SwapWarn, "swap_warn", a.SwapWarn},
	} {
		if *f.out, err = a.bytes(f.name, f.value); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range []struct {
		out   *time.Duration
		name  string
		value string
	}{
		{&l.LowCooldown, "low_cooldown", a.LowCooldown}, {&l.CriticalCooldown, "critical_cooldown", a.CriticalCooldown},
		{&l.Forecast, "forecast", a.Forecast}, {&l.NotifyCooldown, "notify_cooldown", a.NotifyCooldown},
	} {
		if *f.out, err = a.span(f.name, f.value); err != nil {
			errs = append(errs, err)
		}
	}
	if l.Interval, err = a.IntervalDuration(); err != nil {
		errs = append(errs, err)
	}
	if l.TickInterval, err = a.TickIntervalDuration(); err != nil {
		errs = append(errs, err)
	}
	if a.MinFreePct < 0 || a.MinFreePct > 100 {
		errs = append(errs, fmt.Errorf("min_free_pct must be between 0 and 100, got %v", a.MinFreePct))
	}
	if err := errors.Join(errs...); err != nil {
		return Limits{}, err
	}
	return l, nil
}

// Validate checks the auto settings.
func (a Auto) Validate() error {
	_, err := a.Limits()
	return err
}

// Provider defines a cache provider's settings.
type Provider struct {
	MaxSize  string `mapstructure:"max_size" yaml:"max_size"`
	MaxAge   string `mapstructure:"max_age" yaml:"max_age,omitempty"`
	CleanCmd string `mapstructure:"clean_cmd" yaml:"clean_cmd,omitempty"`
	// CleanTimeout bounds the clean command (default 2m); a hung command is cancelled.
	CleanTimeout string   `mapstructure:"clean_timeout" yaml:"clean_timeout,omitempty"`
	Paths        []string `mapstructure:"paths" yaml:"paths,omitempty"`
	Enabled      bool     `mapstructure:"enabled" yaml:"enabled"`

	// Type selects a special provider implementation (see TypeDirPattern).
	Type string `mapstructure:"type" yaml:"type,omitempty"`
	// MinIdle is the minimum idle time before a directory-pattern match may be removed.
	MinIdle string `mapstructure:"min_idle" yaml:"min_idle,omitempty"`
	// SkipIfOpen guards directory-pattern matches that have open files (default true).
	SkipIfOpen *bool `mapstructure:"skip_if_open" yaml:"skip_if_open,omitempty"`
	// SkipIfGitWorktree guards directory-pattern matches containing .git (default true).
	SkipIfGitWorktree *bool `mapstructure:"skip_if_git_worktree" yaml:"skip_if_git_worktree,omitempty"`
	// SkipPrefixes lists entry-name prefixes that whole-entry providers never evict.
	SkipPrefixes []string `mapstructure:"skip_prefixes" yaml:"skip_prefixes,omitempty"`

	// MaxDepth bounds how deep project-artifacts looks below each root (default 4).
	MaxDepth int `mapstructure:"max_depth" yaml:"max_depth,omitempty"`
	// TrimIdle is how long a Rust build unit must go unused before project-artifacts
	// trims it from the target of an active project (default 30d).
	TrimIdle string `mapstructure:"trim_idle" yaml:"trim_idle,omitempty"`
	// ScanBudget bounds project discovery per pass for project-artifacts (default 10s).
	ScanBudget string `mapstructure:"scan_budget" yaml:"scan_budget,omitempty"`
	// PassBudget bounds one whole clean pass of project-artifacts; candidates
	// left when it runs out are skipped (default 30s).
	PassBudget string `mapstructure:"pass_budget" yaml:"pass_budget,omitempty"`
	// SkipIfDirty skips projects with uncommitted changes for project-artifacts (default true).
	SkipIfDirty *bool `mapstructure:"skip_if_dirty" yaml:"skip_if_dirty,omitempty"`
	// Rust, Node and Python switch the per-kind project-artifacts detectors
	// (rust and node default to true, python to false).
	Rust   *bool `mapstructure:"rust" yaml:"rust,omitempty"`
	Node   *bool `mapstructure:"node" yaml:"node,omitempty"`
	Python *bool `mapstructure:"python" yaml:"python,omitempty"`
}

// TypeDirPattern is the provider type that removes whole stale directories matching a glob.
const TypeDirPattern = "dir-pattern"

// DirPatternError reports why a dir-pattern provider's own settings are
// unusable: a path without a glob, a relative path or a bad min_idle. It
// returns nil for other provider types.
func (p Provider) DirPatternError() error {
	if p.Type != TypeDirPattern {
		return nil
	}
	for _, path := range p.Paths {
		if !strings.ContainsAny(path, "*?[") {
			return fmt.Errorf("path %q must contain a glob (*, ? or [)", path)
		}
		if !IsAbsPortable(path) && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
			return fmt.Errorf("path %q must be absolute or start with ~/", path)
		}
	}
	if p.MinIdle != "" {
		if _, err := ParseDuration(p.MinIdle); err != nil {
			return fmt.Errorf("parse min_idle: %w", err)
		}
	}
	return nil
}

// TypeProjectArtifacts is the provider type that removes whole build artifact
// directories (Rust target, node_modules, Python venvs) of idle projects found
// below the roots in paths.
const TypeProjectArtifacts = "project-artifacts"

// MaxProjectDepth is the deepest max_depth accepted.
const MaxProjectDepth = 16

// Validate checks config for required fields.
func (c *Config) Validate() error {
	if err := c.Auto.Validate(); err != nil {
		return fmt.Errorf("auto: %w", err)
	}
	if err := c.validateProtected(); err != nil {
		return err
	}
	if err := c.ValidateAges(); err != nil {
		return err
	}
	for name := range c.Providers {
		p := c.Providers[name]
		if strings.Contains(name, ".") {
			return fmt.Errorf("provider %q: must not contain '.' (reserved as Viper key delimiter)", name)
		}
		if p.Type != "" && p.Type != TypeDirPattern && p.Type != TypeProjectArtifacts {
			return fmt.Errorf("provider %q: unknown type %q", name, p.Type)
		}
		if p.Type == TypeDirPattern {
			for _, path := range p.Paths {
				if !strings.ContainsAny(path, "*?[") {
					return fmt.Errorf("provider %q: %s paths must contain a glob (*, ? or [), got %q",
						name, TypeDirPattern, path)
				}
				if !IsAbsPortable(path) && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
					return fmt.Errorf("provider %q: %s paths must be absolute or start with ~/, got %q",
						name, TypeDirPattern, path)
				}
			}
		}
		if p.Type == TypeProjectArtifacts {
			if err := p.validateProjectArtifacts(); err != nil {
				return fmt.Errorf("provider %q: %w", name, err)
			}
		}
		if p.CleanTimeout != "" {
			if strings.TrimSpace(p.CleanTimeout) == "" {
				return fmt.Errorf("provider %q: clean_timeout must not be blank", name)
			}
			if p.CleanCmd == "" {
				return fmt.Errorf("provider %q: clean_timeout requires clean_cmd", name)
			}
			d, err := ParseBudget(p.CleanTimeout)
			if err != nil {
				return fmt.Errorf("provider %q: clean_timeout: %w", name, err)
			}
			if d <= 0 {
				return fmt.Errorf("provider %q: clean_timeout must be positive, got %q", name, p.CleanTimeout)
			}
		}
		if len(p.Paths) == 0 {
			return fmt.Errorf("provider %q: at least one path is required", name)
		}
		if slices.ContainsFunc(p.Paths, func(path string) bool { return strings.TrimSpace(path) == "" }) {
			return fmt.Errorf("provider %q: paths must not be blank", name)
		}
		if p.MaxSize == "" {
			return fmt.Errorf("provider %q: max_size is required", name)
		}
	}
	return nil
}

// validateProjectArtifacts checks the project-artifacts settings.
func (p Provider) validateProjectArtifacts() error {
	for _, path := range p.Paths {
		if !IsAbsPortable(path) && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
			return fmt.Errorf("%s paths must be absolute or start with ~/, got %q", TypeProjectArtifacts, path)
		}
		if strings.ContainsAny(path, "*?[") {
			return fmt.Errorf("%s paths are literal roots, globs are not allowed: %q", TypeProjectArtifacts, path)
		}
	}
	if p.MaxDepth < 0 {
		return fmt.Errorf("max_depth must be at least 1, got %d", p.MaxDepth)
	}
	if p.MaxDepth > MaxProjectDepth {
		return fmt.Errorf("max_depth must be at most %d, got %d", MaxProjectDepth, p.MaxDepth)
	}
	for field, value := range map[string]string{"min_idle": p.MinIdle, "trim_idle": p.TrimIdle, "scan_budget": p.ScanBudget, "pass_budget": p.PassBudget} {
		if value == "" {
			continue
		}
		parse := ParseBudget
		if field == "min_idle" || field == "trim_idle" {
			parse = ParseDuration
		}
		d, err := parse(value)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s must be positive, got %q", field, value)
		}
	}
	return nil
}

// GetProvider returns provider by name.
func (c *Config) GetProvider(name string) (Provider, bool) {
	p, ok := c.Providers[name]
	return p, ok
}

// EnabledProviders returns sorted list of enabled provider names with existing paths.
func (c *Config) EnabledProviders() []string {
	var enabled []string
	for name := range c.Providers {
		p := c.Providers[name]
		// A misconfigured dir-pattern provider stays listed so its load
		// error is reported instead of the provider silently vanishing.
		if p.Enabled && c.Applies(name) && (ProviderPathsExist(name, p) || p.DirPatternError() != nil) {
			enabled = append(enabled, name)
		}
	}
	sort.Strings(enabled)
	return enabled
}

// AllEnabledProviders returns all enabled providers regardless of path existence.
func (c *Config) AllEnabledProviders() []string {
	var enabled []string
	for name := range c.Providers {
		if c.Providers[name].Enabled && c.Applies(name) {
			enabled = append(enabled, name)
		}
	}
	sort.Strings(enabled)
	return enabled
}

// validateProtected rejects entries that are malformed or so broad they would
// protect everything: relative paths, whitespace, empty, "." or ".." elements,
// the filesystem root, home and any parent of home.
func (c *Config) validateProtected() error {
	home, _ := os.UserHomeDir()
	for _, path := range c.Protected {
		if err := validateProtectedEntry(path, home); err != nil {
			return fmt.Errorf("protected: %w", err)
		}
	}
	return nil
}

func validateProtectedEntry(path, home string) error {
	if path != strings.TrimSpace(path) {
		return fmt.Errorf("%q has leading or trailing whitespace", path)
	}
	norm := strings.ReplaceAll(path, `\`, "/")
	rest, tilde := strings.CutPrefix(norm, "~/")
	drive := ""
	if !tilde {
		var abs bool
		switch {
		case hasDrive(norm) && len(norm) > 2 && norm[2] == '/':
			drive, rest, abs = norm[:2], norm[3:], true
		default:
			rest, abs = strings.CutPrefix(norm, "/")
		}
		if !abs {
			return fmt.Errorf("paths must be absolute or start with ~/, got %q", path)
		}
	}
	if rest == "" {
		return fmt.Errorf("%q is too broad, name a specific directory", path)
	}
	for elem := range strings.SplitSeq(rest, "/") {
		if elem == "" || elem == "." || elem == ".." {
			return fmt.Errorf("%q has an empty, '.' or '..' path element", path)
		}
	}

	if strings.ContainsAny(path, "*?[") {
		return fmt.Errorf("%q contains glob characters, protected entries are literal paths", path)
	}
	topLevel := rest
	if drive == "" {
		topLevel = strings.TrimPrefix(stripDataAlias(norm), "/")
	}
	if !tilde && !strings.Contains(topLevel, "/") {
		return fmt.Errorf("%q is a top-level system directory, name a deeper path", path)
	}

	expanded := "/" + rest
	switch {
	case tilde:
		if home == "" {
			return nil
		}
		expanded = filepath.Join(home, rest)
	case drive != "":
		expanded = drive + "/" + rest
	}
	homes := []string{filepath.Clean(home)}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		homes = append(homes, resolved)
	}
	candidates := []string{filepath.Clean(expanded)}
	if resolved, err := filepath.EvalSymlinks(expanded); err == nil {
		candidates = append(candidates, resolved)
	}
	for _, c := range slices.Clone(candidates) {
		candidates = append(candidates, stripDataAlias(c))
	}
	for _, h := range slices.Clone(homes) {
		homes = append(homes, stripDataAlias(h))
	}
	for _, cand := range candidates {
		cand = filepath.ToSlash(cand)
		if cand == "/" {
			return fmt.Errorf("%q is the filesystem root, name a specific directory", path)
		}
		for _, h := range homes {
			h = filepath.ToSlash(h)
			if h == "." || h == "" {
				continue
			}
			if strings.EqualFold(cand, h) || strings.HasPrefix(strings.ToLower(h), strings.ToLower(cand)+"/") {
				return fmt.Errorf("%q is home or a parent of home, name a specific directory", path)
			}
		}
	}
	return nil
}

// dataVolumeAlias is the macOS firmlink that spells the root filesystem
// again under the user data volume.
const dataVolumeAlias = "/System/Volumes/Data"

// stripDataAlias returns p without the data volume firmlink prefix, using
// slash separators when it strips.
func stripDataAlias(p string) string {
	slashed := filepath.ToSlash(p)
	if len(slashed) < len(dataVolumeAlias) || !strings.EqualFold(slashed[:len(dataVolumeAlias)], dataVolumeAlias) {
		return p
	}
	rest := slashed[len(dataVolumeAlias):]
	if rest == "" {
		return "/"
	}
	if strings.HasPrefix(rest, "/") {
		return rest
	}
	return p
}
