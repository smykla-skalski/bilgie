package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoader_DirPatternOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `version: "1"
providers:
  sail-dirs:
    enabled: true
    min_idle: 6h
    skip_if_open: false
`
	require.NoError(t, os.WriteFile(configPath, []byte(yaml), 0o600))

	loader := NewLoader()
	loader.SetConfigPath(configPath)
	tempDir := t.TempDir()
	loader.SetPlatform(Platform{OS: OSLinux, TempDir: tempDir})
	cfg, err := loader.Load()
	require.NoError(t, err)

	sail := cfg.Providers["sail-dirs"]
	assert.True(t, sail.Enabled)
	assert.Equal(t, TypeDirPattern, sail.Type, "type is inherited from defaults")
	assert.Equal(t, []string{filepath.Join(tempDir, "sail*")}, sail.Paths)
	assert.Equal(t, "6h", sail.MinIdle)
	require.NotNil(t, sail.SkipIfOpen)
	assert.False(t, *sail.SkipIfOpen)
	assert.Nil(t, sail.SkipIfGitWorktree, "unset guard keeps its safe default")
}

func TestLoader_DefaultSailProviderStaysDisabled(t *testing.T) {
	loader := NewLoader()
	loader.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))

	cfg, err := loader.Load()
	require.NoError(t, err)
	assert.False(t, cfg.Providers["sail-dirs"].Enabled)
}

func TestDefaultSailProviderSweepsFromCritical(t *testing.T) {
	defaults := DefaultProvidersFor(Platform{OS: OSLinux, TempDir: "/tmp", SystemTempDir: "/tmp"})

	sail := defaults["sail-dirs"]
	assert.False(t, sail.Enabled, "off in status and clean until the user opts in")
	assert.True(t, sail.SweepsAtCritical())
	assert.Equal(t, "2h", sail.MinIdle)
	assert.Nil(t, sail.SkipIfOpen, "unset guard keeps its safe default (on)")
	assert.Nil(t, sail.SkipIfGitWorktree, "unset guard keeps its safe default (on)")
	assert.False(t, sail.OwnerMarker)

	for name, pc := range defaults {
		if name != "sail-dirs" {
			assert.False(t, pc.SweepsAtCritical(), "%s stays an emergency-tier sweep", name)
		}
	}
}

func TestLoader_SweepTier(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		critical bool
	}{
		{"default is kept", "providers:\n  sail-dirs:\n    min_idle: 6h\n", true},
		{"user can move it to emergency", "providers:\n  sail-dirs:\n    sweep_tier: emergency\n", false},
		{"user can move another sweep to critical", "providers:\n  agent-scratch-dirs:\n    sweep_tier: critical\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte("version: \"1\"\n"+tt.yaml), 0o600))
			loader := NewLoader()
			loader.SetConfigPath(configPath)
			loader.SetPlatform(Platform{OS: OSLinux, TempDir: t.TempDir()})

			cfg, err := loader.Load()
			require.NoError(t, err)

			name := "sail-dirs"
			if strings.Contains(tt.yaml, "agent-scratch-dirs") {
				name = "agent-scratch-dirs"
			}
			assert.Equal(t, tt.critical, cfg.Providers[name].SweepsAtCritical())
			assert.False(t, cfg.Providers[name].Enabled)
		})
	}
}

func TestLoader_RepurposedSweepDropsInheritedTier(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "version: \"1\"\nproviders:\n  sail-dirs:\n    type: project-artifacts\n    enabled: true\n    paths: [~/code]\n"
	require.NoError(t, os.WriteFile(configPath, []byte(yaml), 0o600))
	loader := NewLoader()
	loader.SetConfigPath(configPath)

	cfg, err := loader.Load()
	require.NoError(t, err)

	assert.Empty(t, cfg.Providers["sail-dirs"].SweepTier)
	require.NoError(t, cfg.Validate())
}

func TestConfig_Validate_SweepTier(t *testing.T) {
	sweep := func(tier string, typ string) *Config {
		return &Config{Providers: map[string]Provider{
			"x": {Type: typ, Paths: []string{"/tmp/x*"}, MaxSize: "1G", SweepTier: tier},
		}}
	}
	require.NoError(t, sweep("critical", TypeDirPattern).Validate())
	require.NoError(t, sweep("emergency", TypeDirPattern).Validate())
	require.NoError(t, sweep("", TypeDirPattern).Validate())
	require.ErrorContains(t, sweep("low", TypeDirPattern).Validate(), `sweep_tier must be "critical" or "emergency"`)
	require.ErrorContains(t, sweep("critical", "").Validate(), "sweep_tier only applies to dir-pattern")
}

func TestConfig_Validate_RejectsUnknownType(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"x": {Type: "bogus", Paths: []string{"/a"}, MaxSize: "1G"},
	}}
	require.ErrorContains(t, cfg.Validate(), "unknown type")
}

func TestConfig_Validate_DirPatternRequiresGlob(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"x": {Type: TypeDirPattern, Paths: []string{"~/Documents"}, MaxSize: "1G"},
	}}
	require.ErrorContains(t, cfg.Validate(), "must contain a glob")

	cfg.Providers["x"] = Provider{Type: TypeDirPattern, Paths: []string{"~/Documents/x*"}, MaxSize: "1G"}
	require.NoError(t, cfg.Validate())
}

func TestConfig_Validate_DirPatternRequiresAbsolute(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"x": {Type: TypeDirPattern, Paths: []string{"Documents/*"}, MaxSize: "1G"},
	}}
	require.ErrorContains(t, cfg.Validate(), "absolute")
}

func TestDirPatternError(t *testing.T) {
	tests := []struct {
		name string
		p    Provider
		want string
	}{
		{"valid", Provider{Type: TypeDirPattern, Paths: []string{"~/x/sail*"}, MinIdle: "2h"}, ""},
		{"not a dir-pattern", Provider{Paths: []string{"relative"}, MinIdle: "bad"}, ""},
		{"no glob", Provider{Type: TypeDirPattern, Paths: []string{"~/x/sail"}}, "must contain a glob"},
		{"relative", Provider{Type: TypeDirPattern, Paths: []string{"rel/sail*"}}, "must be absolute or start with ~/"},
		{"bad min_idle", Provider{Type: TypeDirPattern, Paths: []string{"~/x*"}, MinIdle: "soon"}, "parse min_idle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.DirPatternError()
			if tt.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestEnabledProvidersKeepsMisconfiguredDirPattern(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"broken": {Enabled: true, Type: TypeDirPattern, Paths: []string{"rel/none-*"}, MaxSize: "1G"},
		"quiet":  {Enabled: true, Type: TypeDirPattern, Paths: []string{filepath.Join(t.TempDir(), "none-*")}, MaxSize: "1G"},
	}}

	assert.Equal(t, []string{"broken"}, cfg.EnabledProviders(), "valid providers with no matches stay hidden")
}

func TestAgentScratchDirsDefaultsToMarkerOnlyAndDisabled(t *testing.T) {
	loader := NewLoader()
	loader.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
	tempDir := t.TempDir()
	loader.SetPlatform(Platform{OS: OSLinux, TempDir: tempDir})

	cfg, err := loader.Load()
	require.NoError(t, err)

	scratch := cfg.Providers["agent-scratch-dirs"]
	assert.False(t, scratch.Enabled)
	assert.Equal(t, TypeDirPattern, scratch.Type)
	assert.True(t, scratch.OwnerMarker)
	assert.Contains(t, scratch.Paths, filepath.Join(tempDir, "*"))
	require.NoError(t, scratch.DirPatternError())
	assert.False(t, cfg.Providers["sail-dirs"].OwnerMarker)
}

func TestLoader_OwnerMarkerOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `version: "1"
providers:
  agent-scratch-dirs:
    enabled: true
    owner_marker: false
  reef-dirs:
    type: dir-pattern
    enabled: true
    paths: ["/tmp/reef-*"]
    max_size: 20G
`
	require.NoError(t, os.WriteFile(configPath, []byte(yaml), 0o600))

	loader := NewLoader()
	loader.SetConfigPath(configPath)
	cfg, err := loader.Load()
	require.NoError(t, err)

	assert.False(t, cfg.Providers["agent-scratch-dirs"].OwnerMarker, "a user value wins over the default")
	assert.False(t, cfg.Providers["reef-dirs"].OwnerMarker)
	assert.Equal(t, []string{"/tmp/reef-*"}, cfg.Providers["reef-dirs"].Paths)
}
