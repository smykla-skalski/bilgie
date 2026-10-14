package auto

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const uvTag = "Signature: 8a477f597d28d172789f06886806bc55\n# created by uv\n"

func uvConfig(path string) *config.Config {
	return &config.Config{
		Providers: map[string]config.Provider{
			"uv": {Enabled: true, Paths: []string{path}, MaxSize: "1B"},
		},
		Auto: autoCfg(),
	}
}

func uvDeps(home string) Deps {
	return Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 1 * gib, Total: 1000 * gib}, nil },
		NewProvider: provider.NewProvider,
		Out:         &bytes.Buffer{},
		Home:        home,
	}
}

func writeUVCache(t *testing.T, dir string) string {
	t.Helper()
	blob := filepath.Join(dir, "archive-v0", "abc", "mod.py")
	require.NoError(t, os.MkdirAll(filepath.Dir(blob), 0o750))
	require.NoError(t, os.WriteFile(blob, make([]byte, 4096), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CACHEDIR.TAG"), []byte(uvTag), 0o600))
	return blob
}

func TestRun_UVCacheInsideCheckoutIsSkipped(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("PATH", t.TempDir())
	repo := filepath.Join(home, "src", "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o750))
	cache := filepath.Join(repo, ".cache", "uv")
	blob := writeUVCache(t, cache)

	report, err := Run(t.Context(), uvConfig(cache), false, uvDeps(home))

	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, StatusSkipped, report.Results[0].Status)
	assert.Contains(t, report.Results[0].Reason, "protected path")
	assert.FileExists(t, blob)
}

func TestRun_UVPathHoldingCheckoutsIsSkipped(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("PATH", t.TempDir())
	src := filepath.Join(home, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "repo", ".git"), 0o750))
	work := filepath.Join(src, "repo", "main.go")
	require.NoError(t, os.WriteFile(work, []byte("package main\n"), 0o600))

	report, err := Run(t.Context(), uvConfig(src), false, uvDeps(home))

	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, StatusSkipped, report.Results[0].Status)
	assert.Contains(t, report.Results[0].Reason, "not a uv cache")
	assert.FileExists(t, work)
}

func TestRun_UVCacheIsNotScannedForCheckoutsBelowIt(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("PATH", t.TempDir())
	cache := filepath.Join(home, ".cache", "uv")
	writeUVCache(t, cache)
	require.NoError(t, os.MkdirAll(filepath.Join(cache, "git-v0", "checkouts", "abc", ".git"), 0o750))

	report, err := Run(t.Context(), uvConfig(cache), false, uvDeps(home))

	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.NotContains(t, report.Results[0].Reason, "checkout")
	assert.NotContains(t, report.Results[0].Reason, "protected path")
}

func TestProtectionConflicts_UVInsideCheckout(t *testing.T) {
	home := sandboxHome(t)
	repo := filepath.Join(home, "src", "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o750))
	inside := filepath.Join(repo, ".cache", "uv")
	writeUVCache(t, inside)
	outside := filepath.Join(home, ".cache", "uv")
	writeUVCache(t, outside)

	got := ProtectionConflicts(t.Context(), uvConfig(inside), home, provider.NewProvider)
	require.Len(t, got, 1)
	assert.Equal(t, "uv", got[0].Provider)
	assert.Contains(t, got[0].Reason, "protected path")

	assert.Empty(t, ProtectionConflicts(t.Context(), uvConfig(outside), home, provider.NewProvider))
}

func TestProtectionConflicts_UVWithoutTagIsReported(t *testing.T) {
	home := sandboxHome(t)
	src := filepath.Join(home, "src")
	require.NoError(t, os.MkdirAll(src, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "notes.txt"), []byte("keep"), 0o600))

	got := ProtectionConflicts(t.Context(), uvConfig(src), home, provider.NewProvider)

	require.Len(t, got, 1)
	assert.Equal(t, "uv", got[0].Provider)
	assert.Contains(t, got[0].Reason, "not a uv cache")
}

func TestProtectionConflicts_ScanningProtectionAwareProvidersAreStillSkipped(t *testing.T) {
	h := newHarness(t)
	aware := h.addAware("project-artifacts", true)
	newProvider := func(string, config.Provider) (provider.Provider, error) { return aware, nil }

	assert.Empty(t, ProtectionConflicts(t.Context(), h.cfg, h.home, newProvider))
}
