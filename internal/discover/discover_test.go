package discover

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errNotFound = errors.New("not found")

func noTools(string) (string, error) { return "", errNotFound }

type sandbox struct {
	t        *testing.T
	home     string
	platform config.Platform
	cfg      *config.Config
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA"} {
		t.Setenv(key, home)
	}
	t.Setenv("LOCALAPPDATA", "")
	tmp := filepath.Join(home, "tmp")
	require.NoError(t, os.MkdirAll(tmp, 0o750))
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, tmp)
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "UV_CACHE_DIR"} {
		t.Setenv(key, "")
	}
	platform := config.CurrentPlatform()
	platform.SystemTempDir = ""
	return &sandbox{t: t, home: home, platform: platform, cfg: config.DefaultConfigFor(platform)}
}

func (s *sandbox) cacheOf(name string) string {
	s.t.Helper()
	paths, err := config.ExpandPaths(s.cfg.Providers[name].Paths)
	require.NoError(s.t, err)
	require.NotEmpty(s.t, paths)
	return paths[0]
}

func (s *sandbox) fill(dir string, bytes int) {
	s.t.Helper()
	require.NoError(s.t, os.MkdirAll(dir, 0o750))
	require.NoError(s.t, os.WriteFile(filepath.Join(dir, "blob"), make([]byte, bytes), 0o600))
}

func (s *sandbox) disable(names ...string) {
	for _, name := range names {
		pc := s.cfg.Providers[name]
		pc.Enabled = false
		s.cfg.Providers[name] = pc
	}
}

func (s *sandbox) options() Options {
	return Options{
		Cfg:      s.cfg,
		Platform: s.platform,
		Home:     s.home,
		Now:      time.Now(),
		LookPath: noTools,
		Sizes:    true,
	}
}

func find(t *testing.T, report Report, name string) Candidate {
	t.Helper()
	for i := range report.Candidates {
		if report.Candidates[i].Name == name {
			return report.Candidates[i]
		}
	}
	require.Failf(t, "candidate missing", "%s not in %v", name, names(report))
	return Candidate{}
}

func names(report Report) []string {
	out := make([]string, 0, len(report.Candidates))
	for i := range report.Candidates {
		out = append(out, report.Candidates[i].Name)
	}
	return out
}

func TestRun_ListsDetectedProvidersWithSizeAndTier(t *testing.T) {
	s := newSandbox(t)
	s.disable("pip")
	s.fill(s.cacheOf("pip"), 4096)
	s.fill(s.cacheOf("huggingface"), 8192)
	s.fill(s.cacheOf("go-build"), 1024)

	report := Run(context.Background(), s.options())

	pip := find(t, report, "pip")
	assert.Equal(t, TierRegenerable, pip.Tier)
	assert.Equal(t, ActionEnable, pip.Action)
	assert.GreaterOrEqual(t, pip.Bytes, int64(4096))
	assert.True(t, pip.Present)
	assert.False(t, pip.Enabled)
	assert.NotNil(t, pip.Modified)

	hf := find(t, report, "huggingface")
	assert.Equal(t, TierOptIn, hf.Tier)
	assert.Equal(t, ActionOptIn, hf.Action)
	assert.NotEmpty(t, hf.Reason)

	goBuild := find(t, report, "go-build")
	assert.Equal(t, ActionNone, goBuild.Action)
	assert.True(t, goBuild.Enabled)

	assert.ElementsMatch(t, []string{"pip", "huggingface"}, suggestedNames(report))
}

func suggestedNames(report Report) []string {
	var out []string
	suggested := report.Suggestions()
	for i := range suggested {
		out = append(out, suggested[i].Name)
	}
	return out
}

func TestRun_HidesUndetectedProvidersUnlessAll(t *testing.T) {
	s := newSandbox(t)
	s.fill(s.cacheOf("pip"), 16)

	report := Run(context.Background(), s.options())
	assert.Equal(t, []string{"pip"}, names(report))

	opts := s.options()
	opts.All = true
	all := Run(context.Background(), opts)
	assert.Greater(t, len(all.Candidates), 1)
	assert.False(t, find(t, all, "go-build").Detected())
}

func TestRun_DetectsAProviderByItsTool(t *testing.T) {
	s := newSandbox(t)
	opts := s.options()
	opts.LookPath = func(bin string) (string, error) {
		if bin == "go" {
			return "/usr/bin/go", nil
		}
		return "", errNotFound
	}

	report := Run(context.Background(), opts)

	goBuild := find(t, report, "go-build")
	assert.True(t, goBuild.ToolFound)
	assert.False(t, goBuild.Present)
	assert.Equal(t, "go", goBuild.Tool)
}

func TestRun_NeverSuggestsProtectedProviders(t *testing.T) {
	s := newSandbox(t)
	s.disable("pip")
	downloads := filepath.Join(s.home, "Downloads", "pip-cache")
	s.fill(downloads, 64)
	pip := s.cfg.Providers["pip"]
	pip.Paths = []string{downloads}
	s.cfg.Providers["pip"] = pip

	report := Run(context.Background(), s.options())

	got := find(t, report, "pip")
	assert.Equal(t, TierProtected, got.Tier)
	assert.Equal(t, ActionNever, got.Action)
	assert.False(t, got.Suggested())
	assert.Contains(t, got.Reason, "protected")
}

func TestRun_NeverSuggestsProvidersAutoNeverRuns(t *testing.T) {
	s := newSandbox(t)
	darwin := config.Platform{OS: config.OSDarwin, TempDir: os.TempDir()}
	s.platform = darwin
	s.cfg = config.DefaultConfigFor(darwin)
	s.disable("xcode-archives")
	s.fill(s.cacheOf("xcode-archives"), 64)

	report := Run(context.Background(), s.options())

	got := find(t, report, "xcode-archives")
	assert.Equal(t, ActionNever, got.Action)
	assert.Equal(t, TierProtected, got.Tier)
}

func TestRun_FailsClosedWhenTheProtectionCheckCannotFinish(t *testing.T) {
	s := newSandbox(t)
	s.disable("go-build", "pip")
	s.fill(s.cacheOf("go-build"), 64)
	s.fill(s.cacheOf("pip"), 64)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report := Run(ctx, s.options())

	assert.True(t, report.Incomplete)
	assert.Empty(t, report.Suggestions())
	for _, c := range report.Candidates {
		assert.NotEqual(t, ActionEnable, c.Action, c.Name)
		assert.NotEqual(t, ActionOptIn, c.Action, c.Name)
	}
}

func TestRun_ReadsOnly(t *testing.T) {
	s := newSandbox(t)
	s.disable("pip")
	s.fill(s.cacheOf("pip"), 2048)
	s.fill(s.cacheOf("huggingface"), 2048)
	cacheRoot := filepath.Join(s.home, ".cache")
	s.fill(filepath.Join(cacheRoot, "bigtool"), 1<<16)
	before := snapshot(t, s.home)

	opts := s.options()
	opts.Roots = []string{cacheRoot}
	opts.Unmanaged = 5
	opts.UnmanagedMin = 1
	Run(context.Background(), opts)

	assert.Equal(t, before, snapshot(t, s.home))
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := ""
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sum = fmt.Sprintf("%x", sha256.Sum256(data))
		}
		stamp := ""
		if info.Mode().IsRegular() {
			stamp = info.ModTime().String()
		}
		out[path] = stamp + "|" + sum
		return nil
	}))
	return out
}

func TestRun_ClassifiesUnmanagedDirectories(t *testing.T) {
	s := newSandbox(t)
	cacheRoot := filepath.Join(s.home, ".cache")
	dataRoot := filepath.Join(s.home, "state")
	applesRoot := filepath.Join(s.home, "Library", "Caches")
	s.fill(filepath.Join(cacheRoot, "bigtool"), 1<<16)
	s.fill(filepath.Join(cacheRoot, "opencode"), 1<<16)
	s.fill(filepath.Join(cacheRoot, "checkout"), 1<<16)
	require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, "checkout", ".git"), 0o750))
	s.fill(filepath.Join(cacheRoot, "odd[name"), 1<<16)
	s.fill(filepath.Join(dataRoot, "appstate"), 1<<16)
	s.fill(filepath.Join(applesRoot, "com.apple.thing"), 1<<16)

	opts := s.options()
	opts.Sizes = false
	opts.Roots = []string{cacheRoot, dataRoot, applesRoot}
	opts.Unmanaged = 20
	opts.UnmanagedMin = 1
	report := Run(context.Background(), opts)

	bigtool := find(t, report, "custom-bigtool")
	assert.Equal(t, KindUnmanaged, bigtool.Kind)
	assert.Equal(t, TierRegenerable, bigtool.Tier)
	assert.Equal(t, ActionEnable, bigtool.Action)
	require.NotNil(t, bigtool.Define)
	assert.Equal(t, config.TypeDirPattern, bigtool.Define.Type)
	assert.Equal(t, []string{"~/.cache/[b]igtool"}, bigtool.Define.Paths)
	valid := &config.Config{Providers: map[string]config.Provider{"custom-bigtool": *bigtool.Define}}
	require.NoError(t, valid.Validate())

	for _, protectedDir := range []string{"opencode", "checkout"} {
		c := find(t, report, filepath.Join(cacheRoot, protectedDir))
		assert.Equal(t, TierProtected, c.Tier, protectedDir)
		assert.Equal(t, ActionNever, c.Action, protectedDir)
		assert.Nil(t, c.Define, protectedDir)
	}

	appstate := find(t, report, "custom-appstate")
	assert.Equal(t, TierOptIn, appstate.Tier)
	assert.Equal(t, ActionOptIn, appstate.Action)

	apple := find(t, report, "custom-com-apple-thing")
	assert.Equal(t, TierOptIn, apple.Tier)

	odd := find(t, report, "custom-odd-name")
	assert.Equal(t, ActionNone, odd.Action)
	assert.Nil(t, odd.Define)
}

func TestRun_UnmanagedDirectoryNamesNeverCollideWithProviders(t *testing.T) {
	s := newSandbox(t)
	s.cfg.Providers["custom-bigtool"] = config.Provider{Enabled: true, Paths: []string{"~/elsewhere"}, MaxSize: "1G", CleanCmd: "true"}
	cacheRoot := filepath.Join(s.home, ".cache")
	s.fill(filepath.Join(cacheRoot, "bigtool"), 1<<16)
	opts := s.options()
	opts.Sizes = false
	opts.Roots = []string{cacheRoot}
	opts.Unmanaged = 5
	opts.UnmanagedMin = 1

	report := Run(context.Background(), opts)

	assert.Equal(t, "custom-bigtool-2", find(t, report, "custom-bigtool-2").Name)
}

func TestCustomProvider(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		want string
		ok   bool
	}{
		{name: "plain", dir: "/h/.cache/tool", want: "/h/.cache/[t]ool", ok: true},
		{name: "dash inside", dir: "/h/.cache/go-build", want: "/h/.cache/[g]o-build", ok: true},
		{name: "glob character", dir: "/h/.cache/a*b"},
		{name: "bracket", dir: "/h/.cache/a[b"},
		{name: "class leader", dir: "/h/.cache/-x"},
		{name: "negation leader", dir: "/h/.cache/^x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def, ok := customProvider(filepath.FromSlash(tc.dir), "")
			require.Equal(t, tc.ok, ok)
			if !tc.ok {
				return
			}
			assert.Equal(t, []string{filepath.FromSlash(tc.want)}, def.Paths)
			assert.Equal(t, config.TypeDirPattern, def.Type)
		})
	}
}

func TestRun_UnmanagedProtectedNamesAreNeverSuggested(t *testing.T) {
	s := newSandbox(t)
	cacheRoot := filepath.Join(s.home, ".cache")
	for _, dir := range []string{"Downloads", "worktrees", "plain"} {
		s.fill(filepath.Join(cacheRoot, dir), 1<<16)
	}
	opts := s.options()
	opts.Sizes = false
	opts.Roots = []string{cacheRoot}
	opts.Unmanaged = 10
	opts.UnmanagedMin = 1

	report := Run(context.Background(), opts)

	assert.Equal(t, ActionEnable, find(t, report, "custom-plain").Action)
	for _, dir := range []string{"Downloads", "worktrees"} {
		c := find(t, report, filepath.Join(cacheRoot, dir))
		assert.Equal(t, TierProtected, c.Tier, dir)
		assert.Equal(t, ActionNever, c.Action, dir)
		assert.Nil(t, c.Define, dir)
	}
}

func TestRun_DisabledProvidersAreNotOfferedAgainAsCustomSweeps(t *testing.T) {
	s := newSandbox(t)
	s.disable("pip")
	hub := s.cacheOf("huggingface")
	s.fill(hub, 1<<16)
	s.fill(s.cacheOf("pip"), 1<<16)
	opts := s.options()
	opts.Sizes = false
	opts.Roots = []string{filepath.Dir(filepath.Dir(hub)), filepath.Dir(s.cacheOf("pip"))}
	opts.Unmanaged = 10
	opts.UnmanagedMin = 1

	report := Run(context.Background(), opts)

	assert.Equal(t, ActionOptIn, find(t, report, "huggingface").Action)
	for _, c := range report.Candidates {
		assert.NotEqual(t, KindUnmanaged, c.Kind, c.Name)
	}
}

func TestRun_UnmanagedCandidatesCarryTheirEntry(t *testing.T) {
	s := newSandbox(t)
	cacheRoot := filepath.Join(s.home, ".cache")
	s.fill(filepath.Join(cacheRoot, "bigtool"), 1<<16)
	opts := s.options()
	opts.Sizes = false
	opts.Roots = []string{cacheRoot}
	opts.Unmanaged = 5
	opts.UnmanagedMin = 1

	got := find(t, Run(context.Background(), opts), "custom-bigtool")

	require.NotNil(t, got.Entry)
	assert.Equal(t, config.TypeDirPattern, got.Entry.Type)
	assert.Equal(t, got.Define.Paths, got.Entry.Paths)
	assert.NotEmpty(t, got.Entry.MinIdle)
}

func TestRun_RecordsAProviderThatCannotBeMeasured(t *testing.T) {
	s := newSandbox(t)
	s.fill(s.cacheOf("pip"), 64)
	opts := s.options()
	opts.Load = func(string, *config.Config) (provider.Provider, error) { return nil, errNotFound }

	got := find(t, Run(context.Background(), opts), "pip")

	assert.Equal(t, errNotFound.Error(), got.Error)
	assert.Zero(t, got.Bytes)
}
