package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestProjectArtifactsDefaultsOnEveryOS(t *testing.T) {
	for _, p := range []Platform{macPlatform, linPlatform, winPlatform} {
		t.Run(p.OS, func(t *testing.T) {
			pa, ok := DefaultProvidersFor(p)["project-artifacts"]

			require.True(t, ok)
			assert.True(t, pa.Enabled)
			assert.Equal(t, TypeProjectArtifacts, pa.Type)
			assert.Equal(t, []string{"~/sideprojects", "~/kong", "~/work", "~/src", "~/code", "~/projects"}, pa.Paths)
			assert.Equal(t, "60d", pa.MinIdle)
			require.NotNil(t, pa.Rust)
			require.NotNil(t, pa.Node)
			require.NotNil(t, pa.Python)
			assert.True(t, *pa.Rust)
			assert.True(t, *pa.Node)
			assert.False(t, *pa.Python)
			assert.NoError(t, DefaultConfigFor(p).Validate())
		})
	}
}

func TestProjectArtifactsValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Provider)
		errHas string
	}{
		{"defaults", func(*Provider) {}, ""},
		{"glob root", func(p *Provider) { p.Paths = []string{"~/code/*"} }, "literal roots"},
		{"relative root", func(p *Provider) { p.Paths = []string{"code"} }, "absolute"},
		{"negative depth", func(p *Provider) { p.MaxDepth = -1 }, "max_depth"},
		{"too deep", func(p *Provider) { p.MaxDepth = MaxProjectDepth + 1 }, "max_depth"},
		{"bad min_idle", func(p *Provider) { p.MinIdle = "soon" }, "min_idle"},
		{"zero scan_budget", func(p *Provider) { p.ScanBudget = "0s" }, "scan_budget"},
		{"valid tuning", func(p *Provider) { p.MaxDepth, p.ScanBudget, p.MinIdle = 6, "30s", "14d" }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfigFor(linPlatform)
			pa := cfg.Providers["project-artifacts"]
			tt.mutate(&pa)
			cfg.Providers["project-artifacts"] = pa

			err := cfg.Validate()

			if tt.errHas == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.errHas)
		})
	}
}

func TestProjectArtifactsSavedConfigIsPortableAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	loader := NewLoader()
	loader.SetConfigPath(path)
	loader.SetPlatform(macPlatform)
	cfg, err := loader.Load()
	require.NoError(t, err)
	off := false
	pa := cfg.Providers["project-artifacts"]
	pa.Node, pa.MaxDepth, pa.ScanBudget, pa.SkipIfDirty = &off, 6, "20s", &off
	cfg.Providers["project-artifacts"] = pa
	require.NoError(t, loader.Save(cfg))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var saved struct {
		Providers map[string]map[string]any `yaml:"providers"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &saved))
	assert.NotContains(t, saved.Providers["project-artifacts"], "paths", "default roots follow the machine, not the file")

	other := NewLoader()
	other.SetConfigPath(path)
	other.SetPlatform(winPlatform)
	loaded, err := other.Load()

	require.NoError(t, err)
	got := loaded.Providers["project-artifacts"]
	assert.Equal(t, projectRoots, got.Paths)
	assert.Equal(t, 6, got.MaxDepth)
	assert.Equal(t, "20s", got.ScanBudget)
	require.NotNil(t, got.Node)
	assert.False(t, *got.Node)
	require.NotNil(t, got.SkipIfDirty)
	assert.False(t, *got.SkipIfDirty)
	require.NotNil(t, got.Rust)
	assert.True(t, *got.Rust, "unset kinds keep their defaults")
}

func TestProjectArtifactsUserOverridesOnlyWhatTheyName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: "1"
providers:
  project-artifacts:
    python: true
    min_idle: 14d
    paths: [~/mycode, ~/other]
`), 0o600))
	loader := NewLoader()
	loader.SetConfigPath(path)
	loader.SetPlatform(linPlatform)

	cfg, err := loader.Load()

	require.NoError(t, err)
	got := cfg.Providers["project-artifacts"]
	assert.Equal(t, []string{"~/mycode", "~/other"}, got.Paths)
	assert.Equal(t, "14d", got.MinIdle)
	assert.True(t, got.Enabled)
	assert.Equal(t, TypeProjectArtifacts, got.Type)
	require.NotNil(t, got.Python)
	assert.True(t, *got.Python)
	assert.True(t, *got.Rust)
}

func TestBuiltinProtectedRoots(t *testing.T) {
	assert.Empty(t, BuiltinProtectedRoots(""))
	roots := BuiltinProtectedRoots("/h")
	assert.Contains(t, roots, filepath.Join("/h", "Downloads"))
	assert.Contains(t, roots, filepath.Join("/h", ".config", "opencode"))
}

func TestProjectArtifactsPassBudgetAndDepthMessages(t *testing.T) {
	cfg := DefaultConfigFor(Platform{OS: "linux"})
	pa := cfg.Providers["project-artifacts"]
	pa.PassBudget = "0s"
	assert.ErrorContains(t, pa.validateProjectArtifacts(), "pass_budget must be positive")
	pa.PassBudget = "45s"
	pa.MaxDepth = -2
	assert.ErrorContains(t, pa.validateProjectArtifacts(), "max_depth must be at least 1")
	pa.MaxDepth = MaxProjectDepth + 1
	assert.ErrorContains(t, pa.validateProjectArtifacts(), "max_depth must be at most")
	pa.MaxDepth = 3
	assert.NoError(t, pa.validateProjectArtifacts())
}

func TestProjectArtifactsTierIdleSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`version: "1"
providers:
  project-artifacts:
    tier_scaling: false
    critical_min_idle: 3d
    emergency_min_idle: 12h
`), 0o600))
	loader := NewLoader()
	loader.SetConfigPath(path)
	loader.SetPlatform(linPlatform)

	cfg, err := loader.Load()

	require.NoError(t, err)
	got := cfg.Providers["project-artifacts"]
	require.NotNil(t, got.TierScaling)
	assert.False(t, *got.TierScaling)
	assert.Equal(t, "3d", got.CriticalMinIdle)
	assert.Equal(t, "12h", got.EmergencyMinIdle)
	assert.Equal(t, "60d", got.MinIdle, "unset fields keep their defaults")
}

func TestProjectArtifactsTierIdleValidation(t *testing.T) {
	for _, body := range []string{
		"    critical_min_idle: 10s\n",
		"    emergency_min_idle: 10s\n",
	} {
		cfg, err := loadAgeConfig(t, "  x:\n    type: project-artifacts\n    paths: [/a]\n    max_size: 1G\n"+body)
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "under the 1m minimum")
	}
}
