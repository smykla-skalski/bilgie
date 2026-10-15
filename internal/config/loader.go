package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/smykla-skalski/bilgie/internal/fsx"

	"github.com/spf13/viper"
)

// maxConfigBytes bounds the config read; a real file is a few KiB.
const maxConfigBytes = 8 << 20

// Loader handles config file operations.
type Loader struct {
	v            *viper.Viper
	configPath   string // override for testing, empty uses Path()
	skipDefaults bool   // skip merging with defaults (for test isolation)
	platform     Platform
	pathsExist   func([]string) bool
	warn         io.Writer
}

// NewLoader creates a new config loader.
func NewLoader() *Loader {
	return &Loader{v: viper.New(), platform: CurrentPlatform(), pathsExist: PathsExist, warn: os.Stderr}
}

// SetPlatform overrides the OS and environment the defaults are built for.
func (l *Loader) SetPlatform(p Platform) {
	l.platform = p
}

// SetWarnWriter redirects load warnings (default stderr).
func (l *Loader) SetWarnWriter(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	l.warn = w
}

// SetConfigPath overrides config path (for testing).
func (l *Loader) SetConfigPath(path string) {
	l.configPath = path
}

// SkipDefaults disables merging with defaults (for test isolation).
func (l *Loader) SkipDefaults() {
	l.skipDefaults = true
}

// Path returns the config file path this loader reads and writes.
func (l *Loader) Path() (string, error) {
	return l.path()
}

func (l *Loader) path() (string, error) {
	if l.configPath != "" {
		return l.configPath, nil
	}
	return Path()
}

// Load reads config from disk and merges with defaults.
func (l *Loader) Load() (*Config, error) {
	var cfg *Config
	if l.skipDefaults {
		cfg = &Config{
			Version: "1", Providers: make(map[string]Provider), Auto: DefaultAuto(),
			Protected: DefaultProtected(), goos: l.platform.OS,
		}
	} else {
		cfg = DefaultConfigFor(l.platform)
	}

	configPath, err := l.path()
	if err != nil {
		return nil, err
	}

	data, err := fsx.ReadRegular(configPath, maxConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	l.v.SetConfigType("yaml")
	if err := l.v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var userCfg Config
	if err := l.v.Unmarshal(&userCfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	l.mergeAuto(cfg, &userCfg)
	// Union, never replace: removing a built-in entry must not unprotect it.
	cfg.Protected = MergeProtected(append(slices.Clone(cfg.Protected), userCfg.Protected...))
	if err := cfg.validateProtected(); err != nil {
		return nil, err
	}

	// Merge user overrides on top of defaults, field by field.
	for name := range userCfg.Providers {
		userP := userCfg.Providers[name]
		defaultP, hasDefault := cfg.Providers[name]
		if !hasDefault {
			// New provider not in defaults: use as-is; do not auto-enable when `enabled` is omitted.
			foreign := l.otherOSDefaults(name)
			if len(userP.Paths) == 0 && len(foreign) > 0 {
				userP.Paths = foreign[0].Paths
			}
			if len(foreign) > 0 && !slices.ContainsFunc(foreign, func(b Provider) bool { return !customizesBuiltin(userP, b) }) {
				cfg.custom = append(cfg.custom, name)
			}
			cfg.Providers[name] = userP
			continue
		}
		merged := defaultP
		if l.v.IsSet("providers." + name + ".max_size") {
			merged.MaxSize = userP.MaxSize
		}
		if l.v.IsSet("providers." + name + ".max_age") {
			merged.MaxAge = userP.MaxAge
		}
		if l.v.IsSet("providers." + name + ".clean_cmd") {
			merged.CleanCmd = userP.CleanCmd
		}
		if l.v.IsSet("providers." + name + ".clean_timeout") {
			merged.CleanTimeout = userP.CleanTimeout
		}
		l.mergePaths(&merged, name, userP.Paths)
		if l.v.IsSet("providers." + name + ".enabled") {
			merged.Enabled = userP.Enabled
		}
		l.mergeDirPattern(&merged, name, &userP)
		mergeProjectArtifacts(l, name, &merged, &userP)
		// Union, never replace: an empty user list must not strip protections.
		if l.v.IsSet("providers." + name + ".skip_prefixes") {
			merged.SkipPrefixes = unionStrings(defaultP.SkipPrefixes, userP.SkipPrefixes)
		}
		cfg.Providers[name] = merged
	}

	if err := cfg.ValidateAges(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// mergeDirPattern applies the user's type and directory-pattern settings over
// the defaults, field by field.
func (l *Loader) mergeDirPattern(merged *Provider, name string, user *Provider) {
	prefix := "providers." + name + "."
	if l.v.IsSet(prefix + "type") {
		merged.Type = user.Type
	}
	if l.v.IsSet(prefix + "min_idle") {
		merged.MinIdle = user.MinIdle
	}
	if l.v.IsSet(prefix + "skip_if_open") {
		merged.SkipIfOpen = user.SkipIfOpen
	}
	if l.v.IsSet(prefix + "skip_if_git_worktree") {
		merged.SkipIfGitWorktree = user.SkipIfGitWorktree
	}
	if l.v.IsSet(prefix + "owner_marker") {
		merged.OwnerMarker = user.OwnerMarker
	}
	if l.v.IsSet(prefix + "sweep_tier") {
		merged.SweepTier = user.SweepTier
	} else if merged.Type != TypeDirPattern {
		// A built-in repurposed to another type must not keep the inherited tier.
		merged.SweepTier = ""
	}
}

// mergePaths applies the user's path list unless it is another OS's defaults.
func (l *Loader) mergePaths(merged *Provider, name string, paths []string) {
	if !l.v.IsSet("providers." + name + ".paths") {
		return
	}
	goos := l.foreignDefaultOS(name, paths)
	if goos == "" {
		merged.Paths = paths
		return
	}
	if goos != legacyOS && !slices.Equal(paths, merged.Paths) {
		fmt.Fprintf(l.warn, "warning: providers.%s.paths equals the %s defaults and none exists here; "+
			"using the %s defaults (remove paths to silence this)\n", name, goos, l.platform.OS)
	}
}

// mergeProjectArtifacts applies the user's project-artifacts settings over
// the defaults, field by field.
func mergeProjectArtifacts(l *Loader, name string, merged, user *Provider) {
	prefix := "providers." + name + "."
	if l.v.IsSet(prefix + "max_depth") {
		merged.MaxDepth = user.MaxDepth
	}
	if l.v.IsSet(prefix + "trim_idle") {
		merged.TrimIdle = user.TrimIdle
	}
	if l.v.IsSet(prefix + "scan_budget") {
		merged.ScanBudget = user.ScanBudget
	}
	if l.v.IsSet(prefix + "pass_budget") {
		merged.PassBudget = user.PassBudget
	}
	if l.v.IsSet(prefix + "skip_if_dirty") {
		merged.SkipIfDirty = user.SkipIfDirty
	}
	if l.v.IsSet(prefix + "rust") {
		merged.Rust = user.Rust
	}
	if l.v.IsSet(prefix + "node") {
		merged.Node = user.Node
	}
	if l.v.IsSet(prefix + "python") {
		merged.Python = user.Python
	}
	if l.v.IsSet(prefix + "tier_scaling") {
		merged.TierScaling = user.TierScaling
	}
	if l.v.IsSet(prefix + "critical_min_idle") {
		merged.CriticalMinIdle = user.CriticalMinIdle
	}
	if l.v.IsSet(prefix + "emergency_min_idle") {
		merged.EmergencyMinIdle = user.EmergencyMinIdle
	}
}

// unionStrings appends the items of extra that base lacks, so a list saved
// from a merged config does not grow on every round trip.
func unionStrings(base, extra []string) []string {
	out := slices.Clone(base)
	for _, s := range extra {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// mergeAuto applies the user's auto settings over the defaults, field by field.
func (l *Loader) mergeAuto(cfg, userCfg *Config) {
	for _, f := range []struct {
		key  string
		dst  *string
		user string
	}{
		{"interval", &cfg.Auto.Interval, userCfg.Auto.Interval},
		{"tick_interval", &cfg.Auto.TickInterval, userCfg.Auto.TickInterval},
		{"min_free", &cfg.Auto.MinFree, userCfg.Auto.MinFree},
		{"min_free_cap", &cfg.Auto.MinFreeCap, userCfg.Auto.MinFreeCap},
		{"critical_free", &cfg.Auto.CriticalFree, userCfg.Auto.CriticalFree},
		{"emergency_free", &cfg.Auto.EmergencyFree, userCfg.Auto.EmergencyFree},
		{"hysteresis", &cfg.Auto.Hysteresis, userCfg.Auto.Hysteresis},
		{"low_cooldown", &cfg.Auto.LowCooldown, userCfg.Auto.LowCooldown},
		{"critical_cooldown", &cfg.Auto.CriticalCooldown, userCfg.Auto.CriticalCooldown},
		{"forecast", &cfg.Auto.Forecast, userCfg.Auto.Forecast},
		{"notify_cooldown", &cfg.Auto.NotifyCooldown, userCfg.Auto.NotifyCooldown},
		{"swap_warn", &cfg.Auto.SwapWarn, userCfg.Auto.SwapWarn},
	} {
		if l.v.IsSet("auto." + f.key) {
			*f.dst = f.user
		}
	}
	if l.v.IsSet("auto.min_free_pct") {
		cfg.Auto.MinFreePct = userCfg.Auto.MinFreePct
	}
}

// LoadOrCreate loads config (always merges with defaults). Returns (config, created, error).
//
// Deprecated: Use Load() instead. The created return value is always false.
func (l *Loader) LoadOrCreate() (*Config, bool, error) {
	cfg, err := l.Load()
	return cfg, false, err
}

// Save writes config to disk.
func (l *Loader) Save(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	configPath, err := l.path()
	if err != nil {
		return err
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	for key, value := range map[string]any{
		"version":   cfg.Version,
		"providers": l.portableProviders(cfg.Providers),
	} {
		l.v.Set(key, value)
	}
	// Defaults stay out of the file so a later default change still applies.
	if cfg.Auto != DefaultAuto() {
		l.v.Set("auto", cfg.Auto)
	}

	// Built-in entries stay out of the file so a later default change still applies.
	extras := slices.DeleteFunc(slices.Clone(cfg.Protected), func(p string) bool {
		return slices.Contains(DefaultProtected(), p)
	})
	if len(extras) > 0 || l.v.IsSet("protected") {
		l.v.Set("protected", extras)
	}

	return l.v.WriteConfigAs(configPath)
}

// InitDefault creates default config if missing. Returns true if created.
func (l *Loader) InitDefault() (bool, error) {
	exists, err := l.Exists()
	if err != nil {
		return false, err
	}

	if exists {
		return false, nil
	}

	if err := l.Save(DefaultConfigFor(l.platform)); err != nil {
		return false, err
	}

	return true, nil
}

// Exists checks if config file exists.
func (l *Loader) Exists() (bool, error) {
	configPath, err := l.path()
	if err != nil {
		return false, err
	}

	_, err = os.Stat(configPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// legacySailPaths is the sail-dirs default that releases before the per-OS
// defaults wrote into every saved config.
var legacySailPaths = []string{"/private/tmp/sail*"}

// legacyOS marks the pre-per-OS sail-dirs list, which is replaced silently.
const legacyOS = "legacy"

// foreignDefaultOS returns the OS whose built-in paths for the provider equal
// paths, or "" when they are not another OS's defaults. A config saved there
// and synced here holds them. Paths that exist here are kept: the user may
// have narrowed a provider on purpose. Otherwise the current OS's own
// defaults win, so one config file works on every machine. The exact
// legacy sail-dirs list is always the default, as older releases wrote it
// into every saved config; pin it by adding a second path.
func (l *Loader) foreignDefaultOS(name string, paths []string) string {
	if name == "sail-dirs" {
		if slices.Equal(paths, legacySailPaths) {
			return legacyOS
		}
		// Every other single entry is a deliberate pin, never a default.
		if len(paths) == 1 {
			return ""
		}
	}
	if l.pathsExist(paths) {
		return ""
	}
	for _, goos := range []string{OSDarwin, OSLinux, OSWindows} {
		if goos == l.platform.OS {
			continue
		}
		bare := Platform{
			OS: goos, Home: l.platform.Home, TempDir: l.platform.TempDir,
			SystemTempDir: SystemTempDirFor(goos),
		}
		withEnv := l.platform
		withEnv.OS = goos
		withEnv.SystemTempDir = SystemTempDirFor(goos)
		candidates := []Platform{bare, withEnv}
		if sys := SystemTempDirFor(goos); sys != "" {
			sysOnly := bare
			sysOnly.TempDir = sys
			candidates = append(candidates, sysOnly)
		}
		for _, p := range candidates {
			if def, ok := DefaultProvidersFor(p)[name]; ok && slices.Equal(def.Paths, paths) {
				return goos
			}
		}
	}
	return ""
}

// portableProviders drops paths that equal this OS's built-in defaults, some
// of which are machine specific (temp dir, XDG roots), so the saved file
// resolves to the right defaults on whichever OS loads it.
func (l *Loader) portableProviders(providers map[string]Provider) map[string]Provider {
	defaults := DefaultProvidersFor(l.platform)
	out := make(map[string]Provider, len(providers))
	for name := range providers {
		p := providers[name]
		if def, ok := defaults[name]; ok && slices.Equal(p.Paths, def.Paths) {
			p.Paths = nil
		}
		out[name] = p
	}
	return out
}

// otherOSDefaults returns the built-in provider of that name on each other
// OS that has one. A config saved there lists it with its paths omitted; they
// are restored from the first to keep the entry valid here.
func (l *Loader) otherOSDefaults(name string) []Provider {
	var out []Provider
	for _, goos := range []string{OSDarwin, OSLinux, OSWindows} {
		if goos == l.platform.OS {
			continue
		}
		p := l.platform
		p.OS = goos
		if def, ok := DefaultProvidersFor(p)[name]; ok {
			out = append(out, def)
		}
	}
	return out
}

// customizesBuiltin reports whether a user provider that shares its name
// with a built-in of another OS differs from that built-in, so it is the user's own definition rather than the
// copy a synced config carries. A copy keeps the built-in's paths and clean
// command; any other path or command makes it custom, so it runs here
// instead of being dropped as an entry for a different OS.
func customizesBuiltin(user, builtin Provider) bool {
	if user.Type != "" && user.Type != builtin.Type {
		return true
	}
	if user.CleanCmd != "" && user.CleanCmd != builtin.CleanCmd {
		return true
	}
	return !slices.Equal(user.Paths, builtin.Paths)
}
