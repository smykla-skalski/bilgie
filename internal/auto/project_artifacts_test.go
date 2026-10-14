package auto

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type awareProvider struct {
	*fakeProvider
	protected []string
}

func (a *awareProvider) SetProtected(paths []string) { a.protected = paths }

func (h *harness) addAware(name string, over bool) *awareProvider {
	h.t.Helper()
	root := h.dir("code")
	require.NoError(h.t, os.MkdirAll(filepath.Join(root, "worktrees", "feature"), 0o750))
	pc := config.Provider{Enabled: true, Type: config.TypeProjectArtifacts, Paths: []string{root}, MaxSize: "1G"}
	f := &fakeProvider{calls: &h.calls, opts: &h.opts, name: name, paths: []string{root}, max: gib, freed: gib}
	if over {
		f.size = 2 * gib
	}
	aware := &awareProvider{fakeProvider: f}
	h.cfg.Providers[name] = pc
	h.fakes[name] = f
	h.wrap = map[string]provider.Provider{name: aware}
	return aware
}

func TestRun_ProtectionAwareProviderIsNotBlockedByWorktreesInItsRoots(t *testing.T) {
	h := newHarness(t)
	aware := h.addAware("project-artifacts", true)

	_, err := h.run(false, 400*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"project-artifacts"}, h.calls)
	assert.Contains(t, aware.protected, filepath.Join(h.home, "Downloads"))
	assert.Contains(t, aware.protected, filepath.Join(h.home, ".config", "opencode"))
}

func TestRun_RecoveredCallbackOnlyUnderPressure(t *testing.T) {
	h := newHarness(t)
	h.addAware("project-artifacts", true)

	_, err := h.run(false, 400*gib)
	require.NoError(t, err)
	require.Len(t, h.opts, 1)
	assert.Nil(t, h.opts[0].Recovered)

	h = newHarness(t)
	h.addAware("project-artifacts", true)
	_, err = h.run(false, 100*gib, 100*gib, 400*gib)
	require.NoError(t, err)
	require.Len(t, h.opts, 1)
	require.NotNil(t, h.opts[0].Recovered)
	assert.True(t, h.opts[0].Recovered(0))
}

func TestRecoveredFunc(t *testing.T) {
	cfg := config.DefaultConfigFor(config.Platform{OS: config.OSDarwin})
	cfg.Auto = autoCfg()
	free := func(v int64) Deps {
		return Deps{Free: func() (FreeSpace, error) { return FreeSpace{Free: v, Total: 1000 * gib}, nil }}
	}

	assert.False(t, recoveredFunc(cfg, free(100*gib), false)(50*gib), "a real run reads free space, not the estimate")
	assert.True(t, recoveredFunc(cfg, free(400*gib), false)(0))
	assert.False(t, recoveredFunc(cfg, free(100*gib), true)(10*gib))
	assert.True(t, recoveredFunc(cfg, free(100*gib), true)(60*gib), "a dry-run adds what it would free")
	assert.False(t, recoveredFunc(cfg, Deps{Free: func() (FreeSpace, error) { return FreeSpace{}, assert.AnError }}, false)(0))
}

func TestCoveredPaths_ExcludesProjectArtifactsRoots(t *testing.T) {
	cfg := config.DefaultConfigFor(config.Platform{OS: config.OSDarwin, Home: "/Users/u"})
	root := t.TempDir()
	cfg.Providers = map[string]config.Provider{
		"project-artifacts": {Enabled: true, Type: config.TypeProjectArtifacts, Paths: []string{root}, MaxSize: "1G"},
		"npm":               {Enabled: true, Paths: []string{filepath.Join(root, "npm")}, MaxSize: "1G"},
	}

	assert.Equal(t, []string{filepath.Join(root, "npm")}, CoveredPaths(cfg))
}

func TestSortByRebuildCost_ProjectArtifactsAfterCaches(t *testing.T) {
	names := []string{"project-artifacts", "go-build", "npm", "xcode-deriveddata"}
	sortByRebuildCost(names)
	assert.Equal(t, []string{"npm", "go-build", "xcode-deriveddata", "project-artifacts"}, names)
}

func TestRun_CriticalOptionFollowsTier(t *testing.T) {
	tests := []struct {
		name string
		free int64
		want bool
	}{
		{"low", 100 * gib, false},
		{"critical", 8 * gib, true},
		{"emergency", 1 * gib, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.addAware("project-artifacts", true)

			_, err := h.run(false, tt.free, tt.free, 400*gib)

			require.NoError(t, err)
			require.Len(t, h.opts, 1)
			assert.Equal(t, tt.want, h.opts[0].Critical)
		})
	}

	h := newHarness(t)
	h.addAware("project-artifacts", true)
	_, err := h.run(false, 400*gib)
	require.NoError(t, err)
	assert.False(t, h.opts[0].Critical)
}
