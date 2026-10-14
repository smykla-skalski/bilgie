package auto

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
)

type guardedStub struct {
	paths []string
	guard func(string) string
}

func (g *guardedStub) Name() string                                { return "stub" }
func (g *guardedStub) Paths() []string                             { return g.paths }
func (g *guardedStub) CurrentSize(context.Context) (int64, error)  { return 0, nil }
func (g *guardedStub) MaxSize() int64                              { return 0 }
func (g *guardedStub) MaxAge() time.Duration                       { return 0 }
func (g *guardedStub) Available() bool                             { return true }
func (g *guardedStub) SetPathGuard(guard func(path string) string) { g.guard = guard }
func (g *guardedStub) Clean(context.Context, provider.CleanOptions) (provider.CleanResult, error) {
	return provider.CleanResult{}, nil
}

func TestGuardPathsChecksLargeSweepsLazily(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o750))

	stub := &guardedStub{}
	for i := range eagerGuardLimit + 1 {
		dir := filepath.Join(root, fmt.Sprintf("sweep-%03d", i))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub", ".git"), 0o750))
		stub.paths = append(stub.paths, dir)
	}

	reason, err := guardPaths(t.Context(), stub, stub, home, nil)
	require.NoError(t, err)
	assert.Empty(t, reason, "a large sweep is never skipped whole")
	require.NotNil(t, stub.guard)

	assert.Contains(t, stub.guard(stub.paths[0]), "git checkout", "each match is verified when the sweep reaches it")
}

func TestGuardPathsSkipsSmallSweepWhenEveryPathIsBlocked(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "sweep-a")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o750))

	stub := &guardedStub{paths: []string{dir}}
	reason, err := guardPaths(t.Context(), stub, stub, home, nil)
	require.NoError(t, err)
	assert.Contains(t, reason, "protected path")
}

func TestGuardPathsLeavesGitInsideMatchesToTheDirPatternProvider(t *testing.T) {
	tests := []struct {
		name       string
		skipGit    bool
		wantReason string
	}{
		{name: "git check on", skipGit: true},
		{name: "git check off", skipGit: false, wantReason: "git checkout at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			require.NoError(t, os.MkdirAll(home, 0o750))
			dir := filepath.Join(root, "scratch-a")
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub", ".git"), 0o750))

			p, err := provider.NewDirPatternProvider("sweep", config.Provider{
				Type:              config.TypeDirPattern,
				Paths:             []string{filepath.Join(root, "scratch-*")},
				MaxSize:           "1G",
				SkipIfGitWorktree: &tt.skipGit,
			})
			require.NoError(t, err)

			reason, err := guardPaths(t.Context(), p, p, home, nil)
			require.NoError(t, err)
			if tt.wantReason == "" {
				assert.Empty(t, reason)
				return
			}
			assert.Contains(t, reason, tt.wantReason)
		})
	}
}
