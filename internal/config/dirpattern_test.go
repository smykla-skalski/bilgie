package config

import (
	"os"
	"path/filepath"
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
