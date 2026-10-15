package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	envA = "AAAAAAAAAAAAAAAA"
	envB = "BBBBBBBBBBBBBBBB"
)

func TestIsUvxCommand(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"uv tool uvx", "/opt/uv/uv tool uvx freecad-mcp", true},
		{"uv tool run", "uv tool run ruff check", true},
		{"uvx binary", "/usr/local/bin/uvx mcp-server-time", true},
		{"uvx with flags", "uvx --from six python -c import time", true},
		{"uv pip", "uv pip install flask", false},
		{"uv sync", "/usr/local/bin/uv sync", false},
		{"uv tool install", "uv tool install ruff", false},
		{"bare uv", "uv", false},
		{"uvx then uv sync", "sh -c uvx foo; uv sync", false},
		{"uvx then uvx", "sh -c uvx foo; uvx bar", true},
		{"arguments that name uv", "uvx foo uv sync", true},
		{"no uv", "python -m http.server", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUvxCommand(tt.line, false))
		})
	}
	assert.True(t, isUvxCommand(`C:\Tools\uvx.exe mcp-server-time`, true))
}

func TestMentionsAnyStopsAtTheEndOfAnID(t *testing.T) {
	needle := []string{"/c/archive-v0/abc"}
	assert.True(t, mentionsAny("run /c/archive-v0/abc/bin/python", needle))
	assert.True(t, mentionsAny("PATH=/c/archive-v0/abc:/usr/bin", needle))
	assert.True(t, mentionsAny("x /c/archive-v0/abc", needle))
	assert.False(t, mentionsAny("x /c/archive-v0/abcdef/bin", needle))
	assert.False(t, mentionsAny("x /c/archive-v0/abc-def", needle))
	assert.True(t, mentionsAny("x /c/archive-v0/abcdef /c/archive-v0/abc", needle))
}

// sweepFixture is a uv cache with two cached environments, a wheel archive and
// a wheel pointer.
type sweepFixture struct {
	dir   string
	log   string
	p     *UVProvider
	keep  []string
	envs  map[string]string
	links map[string]string
	aged  osshim.Process
	// holders are the pids that hold the cache lock.
	holders []int
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	dir := filepath.Clean(t.TempDir())
	writeUVTag(t, dir)
	f := &sweepFixture{dir: dir, envs: map[string]string{}, links: map[string]string{}}
	for _, id := range []string{envA, envB} {
		payload := filepath.Join(dir, "archive-v0", id)
		require.NoError(t, os.MkdirAll(filepath.Join(payload, "lib", "site-packages"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(payload, "pyvenv.cfg"), []byte("home = /usr/bin\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(payload, "lib", "site-packages", "mod.py"), make([]byte, 4096), 0o600))
		bucket := filepath.Join(dir, "environments-v2", "bucket1")
		require.NoError(t, os.MkdirAll(bucket, 0o700))
		link := filepath.Join(bucket, "hash"+id[:2])
		require.NoError(t, os.Symlink(filepath.Join("..", "..", "archive-v0", id), link))
		f.envs[id], f.links[id] = payload, link
	}
	wheel := filepath.Join(dir, "archive-v0", "WHEELWHEELWHEEL1")
	require.NoError(t, os.MkdirAll(wheel, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(wheel, "six.py"), make([]byte, 256), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "wheels-v6"), 0o700))
	pointer := filepath.Join(dir, "wheels-v6", "pointer.rkyv")
	require.NoError(t, os.WriteFile(pointer, make([]byte, 64), 0o600))
	f.keep = []string{filepath.Join(wheel, "six.py"), pointer, filepath.Join(dir, "CACHEDIR.TAG")}

	f.log = filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: f.log}})
	f.p = newUVProvider(t, config.Provider{Paths: []string{dir}})
	f.p.processEnvs = func(context.Context) ([]osshim.ProcessEnv, error) { return nil, nil }
	f.p.openUnder = func(context.Context, []string) (map[string]bool, error) { return map[string]bool{}, nil }
	f.holders = []int{500}
	lock := filepath.Join(dir, ".lock")
	f.p.busy.lockHeld = func(path string) (bool, error) { return path == lock, nil }
	f.p.busy.lockHolders = func(context.Context, string) ([]int, error) { return f.holders, nil }
	f.aged = proc(500, 1, 3*time.Hour, "/opt/uv/uv tool uvx freecad-mcp")
	f.setProcs(f.aged)
	return f
}

func proc(pid, ppid int, age time.Duration, line string) osshim.Process {
	return osshim.Process{PID: pid, PPID: ppid, CommandLine: line, Args: line, Started: time.Now().Add(-age)}
}

func (f *sweepFixture) setProcs(procs ...osshim.Process) {
	f.p.busy.listProcesses = func(context.Context) ([]osshim.Process, error) { return procs, nil }
}

// serverFor adds the uvx server's python child, which runs from the id's environment.
func (f *sweepFixture) serverFor(id string) osshim.Process {
	return proc(501, 500, 3*time.Hour, filepath.Join(f.envs[id], "bin", "python")+" "+filepath.Join(f.envs[id], "bin", "freecad-mcp"))
}

func (f *sweepFixture) requireIntact(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		assert.DirExists(t, f.envs[id])
		target, err := os.Readlink(f.links[id])
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("..", "..", "archive-v0", id), target)
	}
}

func (f *sweepFixture) requireGone(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		assert.NoDirExists(t, f.envs[id])
		_, err := os.Lstat(f.links[id])
		assert.True(t, os.IsNotExist(err), "symlink of %s must be gone", id)
	}
}

func TestUVProvider_AgedUvxSweepsOnlyUnusedEnvironments(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	assert.Empty(t, res.Warnings)
	assert.Positive(t, res.BytesCleaned)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, f.envs[envB], res.Entries[0].Path)
	assert.Contains(t, res.Output, "uv cache prune not run")
	f.requireIntact(t, envA)
	f.requireGone(t, envB)
	requireAllExist(t, f.keep)
	assert.Empty(t, logLines(t, f.log), "uv must not run while a uvx server holds its lock")
}

func TestUVProvider_AgedUvxSweepsInFullModeToo(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	f.requireIntact(t, envA)
	f.requireGone(t, envB)
	assert.Empty(t, logLines(t, f.log), "full mode must not run uv cache clean either")
}

func TestUVProvider_SweepKeepsAnEnvironmentTheProcessEnvironmentNames(t *testing.T) {
	f := newSweepFixture(t)
	server := proc(501, 500, 3*time.Hour, "python -c import time")
	f.setProcs(f.aged, server)
	f.p.processEnvs = func(context.Context) ([]osshim.ProcessEnv, error) {
		return []osshim.ProcessEnv{{PID: 501, Text: "HOME=/h PATH=" + filepath.Join(f.envs[envA], "bin") + ":/usr/bin"}}, nil
	}

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	f.requireIntact(t, envA)
	f.requireGone(t, envB)
}

func TestUVProvider_SweepMatchesTheResolvedSpellingOfTheCacheDir(t *testing.T) {
	f := newSweepFixture(t)
	resolved, err := filepath.EvalSymlinks(f.dir)
	require.NoError(t, err)
	server := proc(501, 500, 3*time.Hour, filepath.Join(resolved, "archive-v0", envA, "bin", "python"))
	f.setProcs(f.aged, server)

	_, err = f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	f.requireIntact(t, envA)
	f.requireGone(t, envB)
}

func TestUVProvider_SweepKeepsAnEnvironmentWithOpenFiles(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA))
	f.p.openUnder = func(_ context.Context, dirs []string) (map[string]bool, error) {
		open := map[string]bool{}
		for _, d := range dirs {
			open[d] = d == f.envs[envB]
		}
		return open, nil
	}

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NotEmpty(t, res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepSkipsWhenAServerCannotBeAttributed(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, proc(501, 500, 3*time.Hour, "python -c import time"))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "cannot tell which cached environment pid 500 runs from")
	f.requireIntact(t, envA, envB)
	assert.Empty(t, logLines(t, f.log))
}

func TestUVProvider_SweepSkipsWhenTheServerHasNoChild(t *testing.T) {
	f := newSweepFixture(t)

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "pid 500")
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepFailsClosedWhenEvidenceIsMissing(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name   string
		break_ func(f *sweepFixture)
		want   string
	}{
		{"process environments", func(f *sweepFixture) {
			f.p.processEnvs = func(context.Context) ([]osshim.ProcessEnv, error) { return nil, boom }
		}, "read process environments"},
		{"open files", func(f *sweepFixture) {
			f.p.openUnder = func(context.Context, []string) (map[string]bool, error) { return nil, boom }
		}, "list open files"},
		{"process listing", func(f *sweepFixture) {
			f.p.busy.listProcesses = func(context.Context) ([]osshim.Process, error) { return nil, boom }
		}, "cannot list processes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSweepFixture(t)
			f.setProcs(f.aged, f.serverFor(envA))
			tt.break_(f)

			res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
			require.NoError(t, err)
			assert.Contains(t, res.SkipReason, tt.want)
			f.requireIntact(t, envA, envB)
			assert.Empty(t, logLines(t, f.log))
		})
	}
}

func TestUVProvider_YoungUvxStillBlocks(t *testing.T) {
	f := newSweepFixture(t)
	f.p.busy.lockHeld = func(string) (bool, error) { return false, nil }
	f.setProcs(proc(500, 1, 5*time.Minute, "/opt/uv/uv tool uvx freecad-mcp"), f.serverFor(envA))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "uv is running (pid 500")
	f.requireIntact(t, envA, envB)
	assert.Empty(t, logLines(t, f.log))
}

func TestUVProvider_UvxWithUnknownStartStillBlocks(t *testing.T) {
	f := newSweepFixture(t)
	f.p.busy.lockHeld = func(string) (bool, error) { return false, nil }
	f.setProcs(osshim.Process{PID: 500, CommandLine: "uv tool uvx x", Args: "uv tool uvx x"})

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Equal(t, "uv is running (pid 500)", res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_AnyOtherUVProcessStillBlocks(t *testing.T) {
	for _, line := range []string{"uv pip install flask", "uv sync", "uv run pytest", "uv tool install ruff"} {
		t.Run(line, func(t *testing.T) {
			f := newSweepFixture(t)
			f.p.busy.lockHeld = func(string) (bool, error) { return false, nil }
			f.setProcs(f.aged, f.serverFor(envA), proc(600, 1, 5*time.Hour, "/opt/uv/"+line))

			res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
			require.NoError(t, err)
			assert.Contains(t, res.SkipReason, "uv is running (pid 600")
			f.requireIntact(t, envA, envB)
			assert.Empty(t, logLines(t, f.log))
		})
	}
}

func TestUVProvider_LockHeldByAnAgedUvxIsSwept(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	f.requireIntact(t, envA)
	f.requireGone(t, envB)
}

func TestUVProvider_LockHeldWithoutAnAgedUvxStillBlocks(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs()
	lock := filepath.Join(f.dir, ".lock")
	f.p.busy.lockHeld = func(string) (bool, error) { return true, nil }

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Equal(t, "lock held: "+lock, res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_UvxMinAgeIsConfigurable(t *testing.T) {
	f := newSweepFixture(t)
	f.p.busy.lockHeld = func(string) (bool, error) { return false, nil }
	f.setProcs(proc(500, 1, 10*time.Minute, "uv tool uvx freecad-mcp"), proc(501, 500, 10*time.Minute, filepath.Join(f.envs[envA], "bin", "python")))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "uv is running")

	p, err := NewUVProvider("uv", config.Provider{Paths: []string{f.dir}, MaxSize: "1", UvxMinAge: "5m"})
	require.NoError(t, err)
	p.lockPath = filepath.Join(t.TempDir(), "uv.lock")
	p.busy.listProcesses = f.p.busy.listProcesses
	lock := filepath.Join(f.dir, ".lock")
	p.busy.lockHeld = func(path string) (bool, error) { return path == lock, nil }
	p.busy.lockHolders = f.p.busy.lockHolders
	p.processEnvs, p.openUnder = f.p.processEnvs, f.p.openUnder
	res, err = p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	f.requireIntact(t, envA)
	f.requireGone(t, envB)
}

func TestUVProvider_SweepDryRunRemovesNothing(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, f.envs[envB], res.Entries[0].Path)
	assert.Positive(t, res.Entries[0].Size)
	assert.Equal(t, res.Entries[0].Size, res.BytesCleaned, "a dry run reports what it would free")
	assert.Contains(t, res.Output, "would remove 1 unused cached environments")
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepSkipsWhenEveryEnvironmentIsInUse(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA), proc(502, 500, 3*time.Hour, filepath.Join(f.envs[envB], "bin", "python")))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "all 2 cached environments are in use")
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepWithoutEnvironmentsSkips(t *testing.T) {
	dir, files := uvCache(t)
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})
	p.busy.lockHeld = func(string) (bool, error) { return true, nil }
	p.busy.lockHolders = func(context.Context, string) ([]int, error) { return []int{500}, nil }
	p.busy.listProcesses = func(context.Context) ([]osshim.Process, error) {
		return []osshim.Process{proc(500, 1, 3*time.Hour, "uv tool uvx x")}, nil
	}
	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "no cached environment to remove")
	requireAllExist(t, files)
}

func TestUVProvider_SweepRestoresWhenUVStartsMidSweep(t *testing.T) {
	f := newSweepFixture(t)
	calls := 0
	f.p.busy.listProcesses = func(context.Context) ([]osshim.Process, error) {
		calls++
		procs := []osshim.Process{f.aged, f.serverFor(envA)}
		if calls > 2 {
			procs = append(procs, proc(900, 1, time.Second, "/opt/uv/uv tool uvx other"))
		}
		return procs, nil
	}

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "uv started (pid 900) while sweeping")
	f.requireIntact(t, envA, envB)
	assert.Zero(t, res.BytesCleaned)
}

func TestUVProvider_SweepRestoresAnEnvironmentUsedMidSweep(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA))
	calls := 0
	f.p.processEnvs = func(context.Context) ([]osshim.ProcessEnv, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return []osshim.ProcessEnv{{PID: 700, Text: "PATH=" + filepath.Join(f.envs[envB], "bin")}}, nil
	}

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NotEmpty(t, res.SkipReason)
	assert.Empty(t, res.Warnings)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_SweepLeavesUnrecognizedLayoutsAlone(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA))
	bucket := filepath.Join(f.dir, "environments-v2", "bucket1")

	realDir := filepath.Join(bucket, "realdir")
	require.NoError(t, os.MkdirAll(realDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "pyvenv.cfg"), nil, 0o600))

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "pyvenv.cfg"), nil, 0o600))
	escape := filepath.Join(bucket, "escape")
	require.NoError(t, os.Symlink(outside, escape))

	nested := filepath.Join(f.dir, "archive-v0", "WHEELWHEELWHEEL1")
	noVenv := filepath.Join(bucket, "novenv")
	require.NoError(t, os.Symlink(filepath.Join("..", "..", "archive-v0", filepath.Base(nested)), noVenv))

	_, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.DirExists(t, realDir)
	assert.DirExists(t, outside)
	assert.DirExists(t, nested)
	for _, link := range []string{escape, noVenv} {
		_, statErr := os.Lstat(link)
		assert.NoError(t, statErr)
	}
	requireAllExist(t, f.keep)
}

func TestUVProvider_NoUvxMeansUvRunsAsBefore(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs()
	f.p.busy.lockHeld = func(string) (bool, error) { return false, nil }
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Exit: 9}, Replies: map[string]fakeReply{
		"cache prune": {Log: f.log, Stdout: "Pruning cache\n"},
	}})

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	lines := logLines(t, f.log)
	require.Len(t, lines, 1)
	assert.True(t, strings.HasPrefix(lines[0], "cache prune --no-config"))
	f.requireIntact(t, envA, envB)
}

func TestNewUVProvider_RejectsBadUvxMinAge(t *testing.T) {
	_, err := NewUVProvider("uv", config.Provider{Paths: []string{t.TempDir()}, MaxSize: "1", UvxMinAge: "soon"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uvx_min_age")
}

func TestUVProvider_LockHeldByAnotherProcessStillBlocks(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA), proc(800, 1, time.Hour, "python app.py"))
	f.holders = []int{500, 800}

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Equal(t, "lock held: "+filepath.Join(f.dir, ".lock")+" (pid 800)", res.SkipReason)
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_UnknownLockHoldersStillBlock(t *testing.T) {
	f := newSweepFixture(t)
	f.setProcs(f.aged, f.serverFor(envA))
	f.p.busy.lockHolders = func(context.Context, string) ([]int, error) { return nil, errors.New("no lsof") }

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "cannot tell who holds it: no lsof")
	f.requireIntact(t, envA, envB)

	f.p.busy.lockHolders = func(context.Context, string) ([]int, error) { return nil, nil }
	res, err = f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "lock held:")
	f.requireIntact(t, envA, envB)
}

func TestUVProvider_AgedUvxOfAnotherCacheIsIgnored(t *testing.T) {
	f := newSweepFixture(t)
	other := proc(700, 1, 40*time.Hour, "/opt/uv/uv tool uvx ha-mcp")
	f.setProcs(f.aged, f.serverFor(envA), other, proc(701, 700, 40*time.Hour, "/elsewhere/archive-v0/ZZZZ/bin/python"))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason, "pid 700 does not hold this cache's lock, so it neither blocks nor needs attributing")
	f.requireIntact(t, envA)
	f.requireGone(t, envB)
}

func TestUVProvider_AgedUvxWithoutTheLockLeavesUvFree(t *testing.T) {
	f := newSweepFixture(t)
	f.p.busy.lockHeld = func(string) (bool, error) { return false, nil }
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Exit: 9}, Replies: map[string]fakeReply{
		"cache prune": {Log: f.log, Stdout: "Pruning cache\n"},
	}})

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	assert.Len(t, logLines(t, f.log), 1, "no uvx server uses this cache, so uv runs")
}
