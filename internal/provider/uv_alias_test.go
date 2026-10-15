package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aliasOf links a new directory to the fixture's cache, a spelling the
// configured and fully resolved ones do not cover.
func (f *sweepFixture) aliasOf(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(f.dir)
	require.NoError(t, err)
	alias := filepath.Join(t.TempDir(), "via")
	require.NoError(t, os.Symlink(resolved, alias))
	return alias
}

func TestUVProvider_SweepKeepsAnEnvironmentNamedThroughAnotherAlias(t *testing.T) {
	tests := []struct {
		name  string
		spell func(alias, id string) string
	}{
		{"archive directory", func(alias, id string) string {
			return filepath.Join(alias, "archive-v0", id, "bin", "python")
		}},
		{"environment symlink", func(alias, id string) string {
			return filepath.Join(alias, "environments-v2", "bucket1", "hash"+id[:2], "bin", "python")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name+" on the command line", func(t *testing.T) {
			f := newSweepFixture(t)
			alias := f.aliasOf(t)
			f.setProcs(f.aged, proc(501, 500, 3*time.Hour, tt.spell(alias, envA)+" server.py"))

			res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
			require.NoError(t, err)
			assert.Empty(t, res.SkipReason)
			f.requireIntact(t, envA)
			f.requireGone(t, envB)
		})
		t.Run(tt.name+" in the environment", func(t *testing.T) {
			f := newSweepFixture(t)
			alias := f.aliasOf(t)
			f.setProcs(f.aged, proc(501, 500, 3*time.Hour, "python -c import time"))
			f.p.processEnvs = func(context.Context) ([]osshim.ProcessEnv, error) {
				return []osshim.ProcessEnv{{PID: 501, Text: "HOME=/h PATH=" + filepath.Dir(tt.spell(alias, envA)) + ":/usr/bin"}}, nil
			}

			res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
			require.NoError(t, err)
			assert.Empty(t, res.SkipReason)
			f.requireIntact(t, envA)
			f.requireGone(t, envB)
		})
	}
}

func TestUVProvider_SweepKeepsAnAliasedEnvironmentOfAnUnrelatedProcess(t *testing.T) {
	f := newSweepFixture(t)
	alias := f.aliasOf(t)
	f.setProcs(f.aged, f.serverFor(envA), proc(900, 1, time.Minute, filepath.Join(alias, "archive-v0", envB, "bin", "python")+" job.py"))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NotEmpty(t, res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepIgnoresAnAliasOfAnotherCache(t *testing.T) {
	f := newSweepFixture(t)
	other := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(other, "archive-v0", envB), 0o700))
	alias := filepath.Join(t.TempDir(), "via")
	require.NoError(t, os.Symlink(other, alias))
	f.setProcs(f.aged, f.serverFor(envA), proc(900, 1, time.Minute, filepath.Join(alias, "archive-v0", envB, "bin", "python")))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	f.requireIntact(t, envA)
	f.requireGone(t, envB)
}

func TestUVProvider_SweepRecognizesLinkTargetsSpelledThroughAnAlias(t *testing.T) {
	f := newSweepFixture(t)
	alias := f.aliasOf(t)
	for id, link := range f.links {
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(filepath.Join(alias, "archive-v0", id), link))
	}
	f.setProcs(f.aged, f.serverFor(envA))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, f.envs[envB], res.Entries[0].Path)
	assert.DirExists(t, f.envs[envA])
	assert.NoDirExists(t, f.envs[envB])
	target, err := os.Readlink(f.links[envA])
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(alias, "archive-v0", envA), target, "a kept link keeps its own spelling")
}

func TestUVProvider_SweepLeavesLinksThatLeaveTheArchiveAlone(t *testing.T) {
	f := newSweepFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "archive-v0")
	require.NoError(t, os.MkdirAll(filepath.Join(elsewhere, envB), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(elsewhere, envB, "pyvenv.cfg"), nil, 0o600))
	require.NoError(t, os.Remove(f.links[envB]))
	require.NoError(t, os.Symlink(filepath.Join(elsewhere, envB), f.links[envB]))
	f.setProcs(f.aged, f.serverFor(envA))

	_, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.DirExists(t, filepath.Join(elsewhere, envB))
	f.requireIntact(t, envA)
	assert.DirExists(t, f.envs[envB])
}

func TestCacheRefsResolveEveryAliasInFrontOfAnEntry(t *testing.T) {
	target := t.TempDir()
	resolvedReal, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	spaced := filepath.Join(t.TempDir(), "My Cache")
	require.NoError(t, os.Symlink(resolvedReal, spaced))
	plain := filepath.Join(t.TempDir(), "plain")
	require.NoError(t, os.Symlink(resolvedReal, plain))
	missing := filepath.Join(t.TempDir(), "missing")

	tests := []struct {
		name string
		text string
		want []cacheRef
	}{
		{"path with spaces", "PATH=" + spaced + "/archive-v0/ID1/bin:/usr/bin", []cacheRef{{normalizeText(resolvedReal), "archive-v0/id1"}}},
		{"command line", plain + "/archive-v0/ID2/bin/python script.py", []cacheRef{{normalizeText(resolvedReal), "archive-v0/id2"}}},
		{"environment link", "x " + plain + "/environments-v2/ab/cd/bin", []cacheRef{{normalizeText(resolvedReal), "environments-v2/ab/cd"}}},
		{"incomplete link", plain + "/environments-v2/ab", nil},
		{"missing directory", missing + "/archive-v0/ID3/bin", nil},
		{"no entry", plain + "/archive-v0/", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newPathResolver().cacheRefs(tt.text)
			for _, want := range tt.want {
				assert.Contains(t, got, cacheRef{root: want.root, rest: normalizeText(want.rest)})
			}
			if tt.want == nil {
				assert.Empty(t, got)
			}
		})
	}
}
