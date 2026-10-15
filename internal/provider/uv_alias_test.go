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

func TestUVProvider_SweepKeepsAnEnvironmentReachedThroughASymlinkToIt(t *testing.T) {
	f := newSweepFixture(t)
	venv := filepath.Join(t.TempDir(), "myvenv")
	require.NoError(t, os.Symlink(f.envs[envA], venv))
	f.setProcs(f.aged, proc(501, 500, 3*time.Hour, filepath.Join(f.envs[envB], "bin", "python")+" "+filepath.Join(venv, "bin", "server")))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "all 2 cached environments are in use")
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepKeepsAnEnvironmentNamedBehindAFlag(t *testing.T) {
	flags := map[string]string{"include": "-I", "library": "-L", "prefix": "--prefix=", "at": "@", "subshell": "$(", "pipe": "|"}
	for name, flag := range flags {
		t.Run(name, func(t *testing.T) {
			f := newSweepFixture(t)
			alias := f.aliasOf(t)
			f.setProcs(f.aged, f.serverFor(envA), proc(900, 1, time.Minute, "cc "+flag+filepath.Join(alias, "archive-v0", envB, "include")+" x.c"))

			res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
			require.NoError(t, err)
			assert.NotEmpty(t, res.SkipReason)
			f.requireIntact(t, envA, envB)
		})
	}
}

func TestUVProvider_SweepKeepsAnEnvironmentNamedThroughASymlinkedArchive(t *testing.T) {
	f := newSweepFixture(t)
	big := filepath.Join(t.TempDir(), "archive-v0")
	require.NoError(t, os.Rename(filepath.Join(f.dir, "archive-v0"), big))
	require.NoError(t, os.Symlink(big, filepath.Join(f.dir, "archive-v0")))
	f.setProcs(f.aged, f.serverFor(envA), proc(900, 1, time.Minute, filepath.Join(big, envB, "bin", "python")+" job.py"))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NotEmpty(t, res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepKeepsAnEnvironmentWhoseLinkWasRemovedMidSweep(t *testing.T) {
	f := newSweepFixture(t)
	alias := f.aliasOf(t)
	f.setProcs(f.aged, f.serverFor(envA))
	calls := 0
	f.p.busy.listProcesses = func(context.Context) ([]osshim.Process, error) {
		calls++
		if calls > 2 {
			return []osshim.Process{f.aged, f.serverFor(envA), proc(900, 1, time.Minute, "env PATH="+filepath.Join(alias, "environments-v2", "bucket1", "hash"+envB[:2], "bin"))}, nil
		}
		return []osshim.Process{f.aged, f.serverFor(envA)}, nil
	}

	_, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepKeepsAnEnvironmentWhoseInterpreterLinksElsewhere(t *testing.T) {
	f := newSweepFixture(t)
	alias := f.aliasOf(t)
	for _, id := range []string{envA, envB} {
		require.NoError(t, os.MkdirAll(filepath.Join(f.envs[id], "bin"), 0o700))
		require.NoError(t, os.Symlink("/bin/sh", filepath.Join(f.envs[id], "bin", "python")))
	}
	f.setProcs(f.aged, f.serverFor(envA), proc(900, 1, time.Minute, filepath.Join(alias, "archive-v0", envB, "bin", "python")+" job.py"))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NotEmpty(t, res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepKeepsAnEnvironmentUnderAnAliasWithPunctuation(t *testing.T) {
	f := newSweepFixture(t)
	resolved, err := filepath.EvalSymlinks(f.dir)
	require.NoError(t, err)
	alias := filepath.Join(t.TempDir(), "Dropbox (Personal)")
	require.NoError(t, os.Symlink(resolved, alias))
	f.setProcs(f.aged, f.serverFor(envA), proc(900, 1, time.Minute, filepath.Join(alias, "archive-v0", envB, "bin", "python")+" job.py"))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NotEmpty(t, res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepKeepsAnEnvironmentNamedThroughAnAwkwardAlias(t *testing.T) {
	resolveNames := []string{"a b c d e f g h i j", "Docs (a, b, c, d) [x]", "x;y;z;a;b;c;d;e;f"}
	for _, name := range resolveNames {
		t.Run(name, func(t *testing.T) {
			f := newSweepFixture(t)
			resolved, err := filepath.EvalSymlinks(f.dir)
			require.NoError(t, err)
			alias := filepath.Join(t.TempDir(), name)
			require.NoError(t, os.Symlink(resolved, alias))
			f.setProcs(f.aged, f.serverFor(envA), proc(900, 1, time.Minute, filepath.Join(alias, "archive-v0", envB, "bin", "python")+" job.py"))

			res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
			require.NoError(t, err)
			assert.NotEmpty(t, res.SkipReason)
			f.requireIntact(t, envA, envB)
		})
	}
}

func TestUVProvider_SweepKeepsAnEnvironmentBehindALinkChainAndAControlCharacter(t *testing.T) {
	f := newSweepFixture(t)
	dir := t.TempDir()
	require.NoError(t, os.Symlink(f.envs[envB], filepath.Join(dir, "l2")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "l2"), filepath.Join(dir, "l1")))
	f.setProcs(f.aged, f.serverFor(envA))
	f.p.processEnvs = func(context.Context) ([]osshim.ProcessEnv, error) {
		return []osshim.ProcessEnv{{PID: 501, Text: "VIRTUAL_ENV=" + filepath.Join(dir, "l1") + "\tx"}}, nil
	}

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NotEmpty(t, res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestResolvedPathsFollowEveryAlias(t *testing.T) {
	target := t.TempDir()
	resolvedTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	spaced := filepath.Join(t.TempDir(), "My Cache")
	require.NoError(t, os.Symlink(resolvedTarget, spaced))
	plain := filepath.Join(t.TempDir(), "plain")
	require.NoError(t, os.Symlink(resolvedTarget, plain))
	want := normalizeText(resolvedTarget)

	tests := []struct {
		name string
		text string
		want string
	}{
		{"path with spaces", "PATH=" + spaced + "/bin:/usr/bin", want + "/bin:/usr/bin"},
		{"command line", plain + "/x/python script.py", want + "/x/python script.py"},
		{"glued flag", "cc -I" + plain + "/inc x.c", want + "/inc x.c"},
		{"file not created yet", plain + "/new.log", want + "/new.log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, newPathResolver().resolvedPaths(tt.text), normalizeText(tt.want))
		})
	}
	t.Run("missing directory", func(t *testing.T) {
		assert.NotContains(t, newPathResolver().resolvedPaths(filepath.Join(t.TempDir(), "gone", "x")), want)
	})
}
