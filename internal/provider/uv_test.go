package provider

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uvCache builds a uv-shaped cache: a wheels pointer that names an archive.
func uvCache(t *testing.T) (dir string, files []string) {
	t.Helper()
	dir = filepath.Clean(t.TempDir())
	archive := filepath.Join(dir, "archive-v0", "abc")
	require.NoError(t, os.MkdirAll(archive, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "wheels-v5"), 0o700))
	files = []string{
		filepath.Join(archive, "mod.py"),
		filepath.Join(dir, "wheels-v5", "pointer.rkyv"),
		filepath.Join(dir, "CACHEDIR.TAG"),
	}
	for _, f := range files {
		require.NoError(t, os.WriteFile(f, make([]byte, 128), 0o600))
	}
	return dir, files
}

func newUVProvider(t *testing.T, cfg config.Provider) *UVProvider {
	t.Helper()
	cfg.Enabled = true
	if cfg.MaxSize == "" {
		cfg.MaxSize = "1"
	}
	p, err := NewUVProvider("uv", cfg)
	require.NoError(t, err)
	p.busy.listProcesses = listOf()
	return p
}

func requireAllExist(t *testing.T, files []string) {
	t.Helper()
	for _, f := range files {
		assert.FileExists(t, f, "bilgie must never delete a file in the uv cache itself")
	}
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestUVProvider_SmartRunsPruneOnce(t *testing.T) {
	dir, files := uvCache(t)
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Exit: 9, Stderr: "unexpected"}, Replies: map[string]fakeReply{
		"cache prune": {Log: log, Stdout: "Pruning cache\n", Remove: []string{filepath.Join(dir, "archive-v0")}},
	}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.Empty(t, res.SkipReason)
	assert.Equal(t, []string{"cache prune --no-config\tUV_CACHE_DIR=" + dir + "\tUV_NO_CACHE="}, logLines(t, log))
	assert.Equal(t, int64(128), res.BytesCleaned, "freed bytes come from the size before and after")
	assert.Contains(t, res.Output, "Pruning cache")
	assert.NoFileExists(t, files[0])
	requireAllExist(t, files[1:])
}

func TestUVProvider_FullRunsCleanOnce(t *testing.T) {
	dir, files := uvCache(t)
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Exit: 9}, Replies: map[string]fakeReply{
		"cache clean": {Log: log},
	}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)

	assert.Empty(t, res.SkipReason)
	assert.Equal(t, []string{"cache clean --no-config\tUV_CACHE_DIR=" + dir + "\tUV_NO_CACHE="}, logLines(t, log))
	requireAllExist(t, files)
}

func TestUVProvider_CleanCmdOverridesFullMode(t *testing.T) {
	dir, _ := uvCache(t)
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}, CleanCmd: "uv cache prune --ci"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)
	_, err = p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	lines := logLines(t, log)
	require.Len(t, lines, 2)
	assert.True(t, strings.HasPrefix(lines[0], "cache prune --ci --no-config\t"), lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "cache prune --ci --no-config\t"), "smart keeps the configured prune flags: "+lines[1])
}

func TestUVProvider_DryRunReportsCommandAndSize(t *testing.T) {
	dir, files := uvCache(t)
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	for mode, want := range map[CleanMode]string{
		CleanModeSmart: "would run: uv cache prune",
		CleanModeFull:  "would run: uv cache clean",
	} {
		res, err := p.Clean(context.Background(), CleanOptions{Mode: mode, DryRun: true})
		require.NoError(t, err)
		assert.Contains(t, res.Output, want)
		assert.Contains(t, res.Output, "384 B")
		assert.Zero(t, res.BytesCleaned)
	}
	assert.Empty(t, logLines(t, log), "dry-run must not run uv")
	requireAllExist(t, files)
}

func TestUVProvider_SkipsWhenLockHeld(t *testing.T) {
	dir, files := uvCache(t)
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})
	lockPath := filepath.Join(dir, ".lock")
	lockFile(t, lockPath)
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	for _, opts := range []CleanOptions{
		{Mode: CleanModeFull}, {Mode: CleanModeSmart}, {Mode: CleanModeFull, DryRun: true},
	} {
		res, err := p.Clean(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, "lock held: "+lockPath, res.SkipReason)
		assert.Zero(t, res.BytesCleaned)
	}
	assert.Empty(t, logLines(t, log), "a held lock must stop uv from running")
	requireAllExist(t, files)
}

func TestUVProvider_SkipsWhenUVRunning(t *testing.T) {
	dir, files := uvCache(t)
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})
	p.busy.listProcesses = listOf("/usr/local/bin/uv pip install flask")

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Equal(t, "uv is running", res.SkipReason)
	assert.Empty(t, logLines(t, log))
	requireAllExist(t, files)
}

func TestUVProvider_MissingUVSkips(t *testing.T) {
	dir, files := uvCache(t)
	t.Setenv("PATH", t.TempDir())
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	assert.True(t, p.Available(), "a missing uv is a skip line, not a vanished provider")
	for _, mode := range []CleanMode{CleanModeSmart, CleanModeFull} {
		res, err := p.Clean(context.Background(), CleanOptions{Mode: mode})
		require.NoError(t, err)
		assert.Equal(t, "uv not found on PATH", res.SkipReason)
		assert.Zero(t, res.BytesCleaned)
	}
	requireAllExist(t, files)
}

func TestUVProvider_FailingUVSkipsWithReason(t *testing.T) {
	dir, files := uvCache(t)
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Exit: 2, Stderr: "error: cache is broken\n"}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	for _, mode := range []CleanMode{CleanModeSmart, CleanModeFull} {
		res, err := p.Clean(context.Background(), CleanOptions{Mode: mode})
		require.NoError(t, err)
		assert.Contains(t, res.SkipReason, "failed")
		assert.Contains(t, res.SkipReason, "error: cache is broken")
		assert.Zero(t, res.BytesCleaned)
	}
	requireAllExist(t, files)
}

func TestUVProvider_FailureReportsPartialFreedBytes(t *testing.T) {
	dir, files := uvCache(t)
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{
		Exit: 2, Stderr: "error: late failure\n", Remove: []string{filepath.Join(dir, "archive-v0")},
	}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "late failure")
	assert.Equal(t, int64(128), res.BytesCleaned, "what uv removed before failing still counts")
	assert.Contains(t, res.Output, "freed")
	requireAllExist(t, files[1:])
}

func TestUVProvider_TimeoutReportsPartialFreedBytes(t *testing.T) {
	dir, _ := uvCache(t)
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{
		SleepMS: 30000, Remove: []string{filepath.Join(dir, "archive-v0")},
	}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}, CleanTimeout: "500ms"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "timed out")
	assert.Equal(t, int64(128), res.BytesCleaned)
}

func TestUVProvider_TimeoutSkipsWithReason(t *testing.T) {
	dir, files := uvCache(t)
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{SleepMS: 30000}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}, CleanTimeout: "300ms"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "timed out")
	requireAllExist(t, files)
}

func TestUVProvider_ChildEnvPinsCacheDir(t *testing.T) {
	dir, _ := uvCache(t)
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})
	t.Setenv("UV_CACHE_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	t.Setenv("UV_NO_CACHE", "1")
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Equal(t, []string{"cache prune --no-config\tUV_CACHE_DIR=" + dir + "\tUV_NO_CACHE="}, logLines(t, log))
}

// defaultUVPaths is the config an untouched install holds.
func defaultUVPaths() []string {
	return config.DefaultProviders()["uv"].Paths
}

func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("UV_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")
	return home
}

func TestUVProvider_UVCacheDirHonored(t *testing.T) {
	isolateHome(t)
	custom := filepath.Join(filepath.Clean(t.TempDir()), "uvc")
	require.NoError(t, os.MkdirAll(custom, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(custom, "blob"), make([]byte, 64), 0o600))
	t.Setenv("UV_CACHE_DIR", custom)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})

	p := newUVProvider(t, config.Provider{Paths: defaultUVPaths()})
	assert.Equal(t, []string{custom}, p.Paths())

	dry, err := p.Clean(context.Background(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, dry.Output, "64 B")

	_, err = p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Equal(t, []string{"cache prune --no-config\tUV_CACHE_DIR=" + custom + "\tUV_NO_CACHE="}, logLines(t, log))
	assert.FileExists(t, filepath.Join(custom, "blob"))
}

func TestUVProvider_XDGCacheHome(t *testing.T) {
	home := isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	p := newUVProvider(t, config.Provider{Paths: defaultUVPaths()})
	if runtime.GOOS == "windows" {
		assert.NotEqual(t, []string{filepath.Join(xdg, "uv")}, p.Paths(), "uv ignores XDG_CACHE_HOME on Windows")
		return
	}
	assert.Equal(t, []string{filepath.Join(xdg, "uv")}, p.Paths())

	t.Setenv("XDG_CACHE_HOME", "relative/cache")
	p = newUVProvider(t, config.Provider{Paths: []string{"~/.cache/uv"}})
	assert.Equal(t, []string{filepath.Join(home, ".cache", "uv")}, p.Paths(), "a relative XDG_CACHE_HOME is ignored")
}

func TestUVProvider_UVCacheDirBeatsXDG(t *testing.T) {
	isolateHome(t)
	custom := filepath.Clean(t.TempDir())
	t.Setenv("UV_CACHE_DIR", custom)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	p := newUVProvider(t, config.Provider{Paths: defaultUVPaths()})
	assert.Equal(t, []string{custom}, p.Paths())
}

func TestUVProvider_OSDefaultWithoutEnv(t *testing.T) {
	isolateHome(t)
	want, err := config.ExpandPaths(defaultUVPaths())
	require.NoError(t, err)

	p := newUVProvider(t, config.Provider{Paths: defaultUVPaths()})
	assert.Equal(t, want, p.Paths())
}

func TestUVProvider_ExplicitPathBeatsEnv(t *testing.T) {
	isolateHome(t)
	t.Setenv("UV_CACHE_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	explicit := filepath.Clean(t.TempDir())

	p := newUVProvider(t, config.Provider{Paths: []string{explicit}})
	assert.Equal(t, []string{explicit}, p.Paths())
}

func TestNewUVProvider_RejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		cfg  config.Provider
		want string
	}{
		{"foreign clean_cmd", config.Provider{Paths: []string{dir}, CleanCmd: "rm -rf " + dir}, `clean_cmd must be "uv cache clean"`},
		{"extra clean flag", config.Provider{Paths: []string{dir}, CleanCmd: "uv cache clean --force"}, "clean_cmd must be"},
		{"other uv command", config.Provider{Paths: []string{dir}, CleanCmd: "uv cache dir"}, "clean_cmd must be"},
		{"two paths", config.Provider{Paths: []string{dir, t.TempDir()}}, "exactly one uv cache directory"},
		{"no paths", config.Provider{}, "exactly one uv cache directory"},
		{"blank timeout", config.Provider{Paths: []string{dir}, CleanTimeout: "  "}, "clean_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.MaxSize = "1G"
			_, err := NewUVProvider("uv", tt.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNewUVProvider_AcceptsDocumentedCleanCmds(t *testing.T) {
	for _, cmd := range []string{"", "uv cache clean", "uv cache prune", "uv cache prune --ci"} {
		_, err := NewUVProvider("uv", config.Provider{Paths: []string{t.TempDir()}, MaxSize: "1G", CleanCmd: cmd})
		assert.NoError(t, err, cmd)
	}
}

func TestUVProvider_ProtectedPathSkips(t *testing.T) {
	dir, files := uvCache(t)
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})
	p.SetProtected([]string{filepath.Dir(dir)})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "protected path")
	assert.Empty(t, logLines(t, log))
	requireAllExist(t, files)
}

func TestNewProvider_UVIsCommandManaged(t *testing.T) {
	assert.False(t, fileBasedProviders["uv"])
	p, err := NewProvider("uv", config.Provider{Enabled: true, Paths: []string{t.TempDir()}, MaxSize: "4G"})
	require.NoError(t, err)
	assert.IsType(t, &UVProvider{}, p)

	_, err = NewProvider("uv", config.Provider{Enabled: true, Paths: []string{t.TempDir()}, CleanCmd: "echo hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider uv: clean_cmd must be")
}

// globDirNames lists directory names whose glob reading would match a
// sibling; Windows forbids * and ? in names.
func globDirNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"cache[1]"}
	}
	return []string{"cache[1]", "cache*", "cache?"}
}

func TestUVProvider_EnvDirWithGlobCharactersStaysLiteral(t *testing.T) {
	for _, name := range globDirNames() {
		t.Run(name, func(t *testing.T) {
			isolateHome(t)
			root := filepath.Clean(t.TempDir())
			literal := filepath.Join(root, name, "uv")
			sibling := filepath.Join(root, "cache1", "uv")
			for _, d := range []string{literal, sibling} {
				require.NoError(t, os.MkdirAll(d, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(d, "blob"), make([]byte, 64), 0o600))
			}
			log := filepath.Join(t.TempDir(), "calls")
			installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})
			t.Setenv("UV_CACHE_DIR", literal)

			p := newUVProvider(t, config.Provider{Paths: defaultUVPaths()})
			assert.Equal(t, []string{literal}, p.Paths())

			_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
			require.NoError(t, err)
			assert.Equal(t, []string{"cache clean --no-config\tUV_CACHE_DIR=" + literal + "\tUV_NO_CACHE="}, logLines(t, log))
			assert.FileExists(t, filepath.Join(sibling, "blob"))
		})
	}
}

func TestUVProvider_XDGDirWithGlobCharactersStaysLiteral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uv ignores XDG_CACHE_HOME on Windows")
	}
	isolateHome(t)
	xdg := filepath.Join(filepath.Clean(t.TempDir()), "cache[1]")
	require.NoError(t, os.MkdirAll(filepath.Join(xdg, "uv"), 0o700))
	t.Setenv("XDG_CACHE_HOME", xdg)

	p := newUVProvider(t, config.Provider{Paths: defaultUVPaths()})
	assert.Equal(t, []string{filepath.Join(xdg, "uv")}, p.Paths())
}

func TestNewUVProvider_NonLiteralPathsAreErrors(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a1"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a2"), 0o700))

	for name, paths := range map[string][]string{
		"pattern, no such dir": {filepath.Join(root, "none*")},
		"pattern over two":     {filepath.Join(root, "a*")},
		"two paths":            {filepath.Join(root, "a1"), filepath.Join(root, "a2")},
		"empty list":           {},
		"blank":                {""},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := NewUVProvider("uv", config.Provider{Enabled: true, MaxSize: "1G", Paths: paths})
			require.Error(t, err)
			assert.Nil(t, p)
		})
	}
}

func TestNewUVProvider_ExplicitGlobNamedDirIsLiteral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows forbids * and ? in names")
	}
	isolateHome(t)
	root := filepath.Clean(t.TempDir())
	literal := filepath.Join(root, "uv?")
	sibling := filepath.Join(root, "uv1")
	for _, d := range []string{literal, sibling} {
		require.NoError(t, os.MkdirAll(d, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(d, "blob"), make([]byte, 8), 0o600))
	}
	log := filepath.Join(t.TempDir(), "calls")
	installFakeTool(t, "uv", fakeToolSpec{Default: fakeReply{Log: log}})

	p := newUVProvider(t, config.Provider{Paths: []string{literal}})
	assert.Equal(t, []string{literal}, p.Paths())
	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)
	assert.Equal(t, []string{"cache clean --no-config\tUV_CACHE_DIR=" + literal + "\tUV_NO_CACHE="}, logLines(t, log))
	assert.FileExists(t, filepath.Join(sibling, "blob"))
}

func TestNewUVProvider_BracketDirAsExplicitPath(t *testing.T) {
	isolateHome(t)
	literal := filepath.Join(filepath.Clean(t.TempDir()), "uv[cache]")
	require.NoError(t, os.MkdirAll(literal, 0o700))

	p := newUVProvider(t, config.Provider{Paths: []string{literal}})
	assert.Equal(t, []string{literal}, p.Paths())
}

func TestUVProvider_DryRunEmptyCacheReportsZero(t *testing.T) {
	dir := t.TempDir()
	installFakeTool(t, "uv", fakeToolSpec{})
	p := newUVProvider(t, config.Provider{Paths: []string{dir}})

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "cache 0 B")
}

func TestNewUVProvider_RelativePathsAreErrors(t *testing.T) {
	home := isolateHome(t)
	t.Chdir(home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "Documents"), 0o700))

	for _, rel := range []string{".", "..", "Documents", "./Documents", "a/../Documents"} {
		t.Run(rel, func(t *testing.T) {
			p, err := NewUVProvider("uv", config.Provider{Enabled: true, MaxSize: "1G", Paths: []string{rel}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be absolute or start with ~/")
			assert.Nil(t, p)

			loaded, err := NewProvider("uv", config.Provider{Enabled: true, MaxSize: "1G", Paths: []string{rel}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "provider uv: path")
			assert.Nil(t, loaded)
		})
	}
}

func TestUVProvider_UncleanSpellingsStillHitProtectedRoots(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	protected := filepath.Join(root, "precious")
	require.NoError(t, os.MkdirAll(filepath.Join(protected, "uv"), 0o700))
	installFakeTool(t, "uv", fakeToolSpec{})

	for name, spelled := range map[string]string{
		"dotdot":   filepath.Join(root, "other", "..", "precious", "uv"),
		"trailing": filepath.Join(protected, "uv") + string(filepath.Separator) + ".",
		"double":   root + string(filepath.Separator) + string(filepath.Separator) + "precious" + string(filepath.Separator) + "uv",
	} {
		t.Run(name, func(t *testing.T) {
			p := newUVProvider(t, config.Provider{Paths: []string{spelled}})
			p.SetProtected([]string{protected})

			res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
			require.NoError(t, err)
			assert.Contains(t, res.SkipReason, "protected path")
		})
	}
}

func TestProviders_RelativePathsAreRejected(t *testing.T) {
	home := isolateHome(t)
	t.Chdir(home)

	for _, name := range []string{"mise", "rustup", "docker", "go-build", "gh", "npm", "huggingface", "jetbrains", "cargo", "yarn", "docker-volumes"} {
		t.Run(name, func(t *testing.T) {
			p, err := NewProvider(name, config.Provider{Enabled: true, MaxSize: "1G", Paths: []string{"."}, CleanCmd: defaultCmdFor(name)})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be absolute or start with ~/")
			assert.Nil(t, p)
		})
	}
}

func defaultCmdFor(name string) string {
	if name == "npm" || name == "go-build" || name == "docker" || name == "docker-volumes" {
		return name + " cache clean"
	}
	return ""
}

func TestPatternProviders_RelativePathsAreRejected(t *testing.T) {
	home := isolateHome(t)
	t.Chdir(home)

	_, err := NewProvider("sweep", config.Provider{Enabled: true, Type: config.TypeDirPattern, Paths: []string{"rel/dirs-*"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute or start with ~/")

	_, err = NewProvider("proj", config.Provider{Enabled: true, Type: config.TypeProjectArtifacts, Paths: []string{"."}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute or start with ~/")
}
