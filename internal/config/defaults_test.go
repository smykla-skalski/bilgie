package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultProviders_ExtraCaches(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"edge", "~/Library/Caches/Microsoft Edge"},
		{"vivaldi", "~/Library/Caches/Vivaldi"},
		{"huggingface", "~/.cache/huggingface/hub"},
		{"playwright", "~/Library/Caches/ms-playwright"},
		{"lima", "~/Library/Caches/lima"},
		{"gh", "~/.cache/gh"},
		{"chrome-devtools-mcp", "~/.cache/chrome-devtools-mcp"},
		{"vscode-shipit", "~/Library/Caches/com.microsoft.VSCode.ShipIt"},
	}
	optIn := map[string]bool{"huggingface": true, "playwright": true, "chrome-devtools-mcp": true}
	defaults := DefaultProvidersFor(macPlatform)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := defaults[tt.name]
			require.True(t, ok)
			assert.Equal(t, !optIn[tt.name], p.Enabled)
			assert.Equal(t, []string{tt.path}, p.Paths)
			assert.Empty(t, p.CleanCmd)
			assert.NotEmpty(t, p.MaxSize)
		})
	}
}

func TestDefaultProviders_Rustup(t *testing.T) {
	p, ok := DefaultProviders()["rustup"]
	require.True(t, ok)
	assert.False(t, p.Enabled, "toolchains are not a plain cache")
	assert.Equal(t, "rustup toolchain uninstall", p.CleanCmd)
	assert.Equal(t, []string{"~/.rustup/toolchains"}, p.Paths)
}

func TestDefaultProviders_SQLiteCompact(t *testing.T) {
	p, ok := DefaultProvidersFor(macPlatform)["sqlite-compact"]
	require.True(t, ok)
	assert.False(t, p.Enabled, "compaction is opt-in")
	assert.Equal(t, []string{"~/.local/share/opencode/opencode.db"}, p.Paths)
	assert.Equal(t, "1G", p.MaxSize)
	assert.NotContains(t, DefaultProvidersFor(Platform{OS: OSWindows}), "sqlite-compact")
}

func TestDefaultProviders_HomebrewScrubsCache(t *testing.T) {
	assert.Equal(t, "brew cleanup -s", DefaultProvidersFor(macPlatform)["homebrew"].CleanCmd)
}

func TestDefaultConfig_Validates(t *testing.T) {
	require.NoError(t, DefaultConfig().Validate())
}

func TestDefaultProviders_ChromeDevtoolsProtectsProfiles(t *testing.T) {
	p := DefaultProviders()["chrome-devtools-mcp"]
	assert.False(t, p.Enabled)
	assert.Contains(t, p.SkipPrefixes, "chrome-profile-")
}

func TestDefaultProviders_CodexSessions(t *testing.T) {
	p, ok := DefaultProviders()["codex-sessions"]
	require.True(t, ok)
	assert.False(t, p.Enabled, "removing a session drops it from codex resume")
	assert.Equal(t, []string{"~/.codex/sessions"}, p.Paths)
	assert.Equal(t, "90d", p.MaxAge)
	assert.Empty(t, p.CleanCmd)
}

func TestDefaultProviders_AgentAndToolCaches(t *testing.T) {
	defaults := DefaultProvidersFor(macPlatform)

	tests := map[string]string{
		"app-updaters":  "~/Library/Caches/*-updater",
		"copilot":       "~/Library/Caches/copilot",
		"golangci-lint": "~/Library/Caches/golangci-lint",
		"node-gyp":      "~/Library/Caches/node-gyp",
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			p, ok := defaults[name]
			require.True(t, ok)
			assert.True(t, p.Enabled)
			assert.Equal(t, []string{path}, p.Paths)
			assert.NotEmpty(t, p.MaxSize)
			require.NotNil(t, p.SkipIfOpen)
			assert.True(t, *p.SkipIfOpen)
		})
	}

	for _, p := range []Platform{linPlatform, winPlatform} {
		for name := range tests {
			assert.NotContains(t, DefaultProvidersFor(p), name, p.OS)
		}
		assert.NotContains(t, DefaultProvidersFor(p), "sail-e2e-dirs", p.OS)
	}
}

func TestDefaultProviders_SailE2EDirs(t *testing.T) {
	p, ok := DefaultProvidersFor(macPlatform)["sail-e2e-dirs"]
	require.True(t, ok)
	assert.Equal(t, TypeDirPattern, p.Type)
	assert.False(t, p.Enabled, "whole-directory sweeps are opt-in")
	assert.Equal(t, []string{"~/Library/Caches/dev.smykla.sail.e2e.*"}, p.Paths)
	require.NoError(t, p.DirPatternError())
}
