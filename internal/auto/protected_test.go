package auto

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	return home
}

func writeAged(t *testing.T, path string) {
	t.Helper()
	mkdirFile(t, path, 4096)
	old := time.Now().Add(-90 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	require.NoError(t, os.Chtimes(filepath.Dir(path), old, old))
}

// TestRun_NeverDeletesProtectedPaths drives the real providers at the
// critical tier, the most aggressive one, and checks that nothing protected
// is removed while an unprotected control is.
func TestRun_NeverDeletesProtectedPaths(t *testing.T) {
	home := sandboxHome(t)
	cfg := &config.Config{
		Providers: map[string]config.Provider{},
		Auto:      autoCfg(),
		Protected: []string{"~/precious", "~/scratch/sail-keep"},
	}

	protectedFiles := map[string]string{
		"configured entry":   filepath.Join(home, "precious", "data", "a.bin"),
		"downloads":          filepath.Join(home, "Downloads", "a.bin"),
		"opencode data":      filepath.Join(home, ".local", "share", "opencode", "a.bin"),
		"git worktree root":  filepath.Join(home, "repos", "feature", "a.bin"),
		"sweep in protected": filepath.Join(home, "precious", "sweep-1", "a.bin"),
		"glob matches entry": filepath.Join(home, "scratch", "sail-keep", "a.bin"),
		"inside checkout":    filepath.Join(home, "repos", "feature", "node_modules", ".cache", "a.bin"),
	}
	for _, p := range protectedFiles {
		writeAged(t, p)
	}
	mkdirFile(t, filepath.Join(home, "repos", "feature", ".git"), 1)
	noOpenCheck := false
	sweepControl := filepath.Join(home, "scratch", "gone-1", "a.bin")
	writeAged(t, sweepControl)
	control := filepath.Join(home, "plain", "a.bin")
	writeAged(t, control)

	add := func(name string, enabled bool, pc config.Provider) {
		pc.Enabled, pc.MaxSize = enabled, "1B"
		cfg.Providers[name] = pc
	}
	add("uv", true, config.Provider{Paths: []string{filepath.Join(home, "precious", "data")}})
	add("cargo", true, config.Provider{Paths: []string{filepath.Join(home, "Downloads")}})
	add("gradle", true, config.Provider{Paths: []string{filepath.Join(home, ".local", "share", "opencode")}})
	add("playwright", true, config.Provider{Paths: []string{filepath.Join(home, "repos", "feature")}})
	add("sweep", false, config.Provider{
		Type:    config.TypeDirPattern,
		Paths:   []string{filepath.Join(home, "precious", "sweep-*")},
		MinIdle: "1h", SkipIfOpen: &noOpenCheck,
	})
	add("globbed", false, config.Provider{
		Type:    config.TypeDirPattern,
		Paths:   []string{filepath.Join(home, "scratch", "sail-*")},
		MinIdle: "1h", SkipIfOpen: &noOpenCheck,
	})
	add("sweep-control", false, config.Provider{
		Type:  config.TypeDirPattern,
		Paths: []string{filepath.Join(home, "scratch", "gone-*")}, MinIdle: "1h", SkipIfOpen: &noOpenCheck,
	})
	add("edge", true, config.Provider{Paths: []string{filepath.Join(home, "repos", "feature", "node_modules", ".cache")}})
	add("vivaldi", true, config.Provider{Paths: []string{filepath.Join(home, "plain")}})

	deps := Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 1 * gib, Total: 1000 * gib}, nil },
		NewProvider: provider.NewProvider,
		Out:         &bytes.Buffer{},
		Home:        home,
	}
	report, err := Run(t.Context(), cfg, false, deps)
	require.NoError(t, err)

	for name, p := range protectedFiles {
		assert.FileExists(t, p, name)
	}
	assert.NoFileExists(t, control, "control provider must really delete, or the test proves nothing")
	assert.NoFileExists(t, sweepControl, "control sweep must really delete")
	skipped := 0
	for _, res := range report.Results {
		if res.Status == StatusSkipped && res.Reason != "" {
			skipped++
		}
	}
	assert.GreaterOrEqual(t, skipped, 5)
}

func TestProtectedPaths_UnionOfDefaultsAndConfig(t *testing.T) {
	home := absPath("/Users/me")
	got := ProtectedPaths(&config.Config{Protected: []string{"~/keep", absPath("/data/keep"), "~/Downloads"}}, home)

	assert.Contains(t, got, filepath.Join(home, "Downloads"))
	assert.Contains(t, got, filepath.Join(home, ".local", "share", "opencode"))
	if runtime.GOOS != "windows" {
		assert.Contains(t, got, "/var/lib/docker/volumes")
	}
	assert.Contains(t, got, filepath.Join(home, "keep"))
	assert.Contains(t, got, absPath("/data/keep"))

	empty := ProtectedPaths(&config.Config{}, home)
	assert.Contains(t, empty, filepath.Join(home, "Downloads"))
	assert.Len(t, got, len(empty)+2)
}

func TestIsProtectedWith_ConfiguredAndGitRoots(t *testing.T) {
	home := t.TempDir()
	extra := []string{filepath.Join(home, "keep")}
	repo := filepath.Join(home, "repo")
	mkdirFile(t, filepath.Join(repo, ".git"), 1)

	assert.True(t, isProtectedWith(filepath.Join(home, "keep", "x"), home, extra, false))
	assert.True(t, isProtectedWith(repo, home, nil, false))
	assert.False(t, isProtectedWith(filepath.Join(home, "other"), home, extra, false))
}

func TestScanProtected_MeasuresExistingEntries(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "Downloads", "a.bin"), 300000)
	mkdirFile(t, filepath.Join(home, "keep", "a.bin"), 100000)
	mkdirFile(t, filepath.Join(home, "sideprojects", "worktrees", "r", "a.bin"), 200000)
	mkdirFile(t, filepath.Join(home, "deep", "er", "and", "deeper", "worktrees", "a.bin"), 50000)
	cfg := &config.Config{Protected: []string{"~/keep", "~/missing"}}

	report := ScanProtected(t.Context(), cfg, home, time.Minute)

	var names []string
	for _, e := range report.Entries {
		rel, err := filepath.Rel(home, e.Path)
		require.NoError(t, err)
		names = append(names, rel)
		assert.Positive(t, e.Bytes)
	}
	assert.Equal(t, []string{"Downloads", filepath.Join("sideprojects", "worktrees"), "keep"}, names)
	assert.False(t, report.Incomplete)
}

func TestScanProtected_CancelledContextIsIncomplete(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "Downloads", "a.bin"), 1000)
	ctx, stop := context.WithCancel(t.Context())
	stop()
	report := ScanProtected(ctx, &config.Config{}, home, 0)

	assert.True(t, report.Incomplete)
	assert.NotNil(t, report.Entries)
}

func TestProtectedPaths_DropsRelativeEntries(t *testing.T) {
	got := ProtectedPaths(&config.Config{Protected: []string{"Downloads2", "./x"}}, absPath("/Users/me"))

	assert.NotContains(t, got, "Downloads2")
	assert.Len(t, got, len(ProtectedPaths(&config.Config{}, absPath("/Users/me"))))
}

func TestInsideGitCheckout_StopsAtHome(t *testing.T) {
	home := t.TempDir()
	mkdirFile(t, filepath.Join(home, ".git", "HEAD"), 1)
	mkdirFile(t, filepath.Join(home, "plain", "a.bin"), 1)
	mkdirFile(t, filepath.Join(home, "repo", ".git"), 1)
	mkdirFile(t, filepath.Join(home, "repo", "sub", "deep", "a.bin"), 1)

	assert.False(t, insideGitCheckout(filepath.Join(home, "plain"), home), "dotfiles repo in home must not protect everything")
	assert.True(t, insideGitCheckout(filepath.Join(home, "repo", "sub", "deep"), home))
	assert.True(t, insideGitCheckout(filepath.Join(home, "repo"), home))
}

func TestScanProtected_DropsNestedEntriesAndFlagsBudgetDuringWalk(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "Downloads", "proj", "a.bin"), 1000)
	cfg := &config.Config{Protected: []string{"~/Downloads/proj"}}

	report := ScanProtected(t.Context(), cfg, home, time.Minute)

	require.Len(t, report.Entries, 1)
	assert.Equal(t, filepath.Join(home, "Downloads"), report.Entries[0].Path)
}

func TestScanProtected_MeasuresThroughSymlinkedEntry(t *testing.T) {
	home := sandboxHome(t)
	external := t.TempDir()
	mkdirFile(t, filepath.Join(external, "a.bin"), 200000)
	require.NoError(t, os.Symlink(external, filepath.Join(home, "Downloads")))

	report := ScanProtected(t.Context(), &config.Config{}, home, time.Minute)

	require.Len(t, report.Entries, 1)
	assert.GreaterOrEqual(t, report.Entries[0].Bytes, int64(200000))
}

func TestScanProtected_CancelledScanKeepsEntriesAsPartial(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "Downloads", "a.bin"), 1000)
	ctx, stop := context.WithCancel(t.Context())
	stop()

	report := ScanProtected(ctx, &config.Config{}, home, 0)

	require.Len(t, report.Entries, 1)
	assert.True(t, report.Entries[0].Partial)
}

func TestScanProtected_DedupsSymlinkAliasAndCase(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "Zed", "inner", "a.bin"), 100000)
	require.NoError(t, os.Symlink(filepath.Join(home, "Zed", "inner"), filepath.Join(home, "Alias")))
	cfg := &config.Config{Protected: []string{"~/Alias", "~/Zed"}}

	report := ScanProtected(t.Context(), cfg, home, time.Minute)

	require.Len(t, report.Entries, 1)
	assert.Equal(t, filepath.Join(home, "Zed"), report.Entries[0].Path)
}

func TestScanProtected_ListsSymlinkedWorktreesDir(t *testing.T) {
	home := sandboxHome(t)
	external := t.TempDir()
	mkdirFile(t, filepath.Join(external, "repo", "a.bin"), 100000)
	require.NoError(t, os.Symlink(external, filepath.Join(home, "worktrees")))

	report := ScanProtected(t.Context(), &config.Config{}, home, time.Minute)

	require.Len(t, report.Entries, 1)
	assert.Equal(t, filepath.Join(home, "worktrees"), report.Entries[0].Path)
	assert.GreaterOrEqual(t, report.Entries[0].Bytes, int64(100000))
}

func TestWorktreeHolders_SkipsPermissionPromptDirs(t *testing.T) {
	home := sandboxHome(t)
	for _, parent := range []string{"Library", "Documents", "Desktop", "proj"} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, parent, "worktrees"), 0o750))
	}

	got := WorktreeHolders(t.Context(), home)

	assert.Equal(t, []string{filepath.Join(home, "proj", "worktrees")}, got)
}

func TestWorktreeHolders_ReturnsPromptlyWhenContextExpires(t *testing.T) {
	home := sandboxHome(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "proj", "worktrees"), 0o750))
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	start := time.Now()
	got := WorktreeHolders(ctx, home)

	assert.Less(t, time.Since(start), time.Second)
	assert.Empty(t, got)
}

func TestScanProtected_IncludesAutoProtectedXcodeArchives(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "Library", "Developer", "Xcode", "Archives", "a.bin"), 100000)

	report := ScanProtected(t.Context(), &config.Config{}, home, time.Minute)

	require.Len(t, report.Entries, 1)
	assert.Equal(t, filepath.Join(home, "Library", "Developer", "Xcode", "Archives"), report.Entries[0].Path)
}

func TestInsideGitCheckout_DataAliasAndCaseOfHome(t *testing.T) {
	home := t.TempDir()
	mkdirFile(t, filepath.Join(home, ".git", "HEAD"), 1)
	mkdirFile(t, filepath.Join(home, "plain", "a.bin"), 1)

	assert.False(t, insideGitCheckout(filepath.Join(home, "plain"), strings.ToUpper(home)))
	assert.False(t, insideGitCheckout("/System/Volumes/Data"+filepath.Join(home, "plain"), home))
}

func TestInsideGitCheckout_AliasSpellingStopsAtDottedHome(t *testing.T) {
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home := filepath.Join(resolved, "marcin.skalski")
	mkdirFile(t, filepath.Join(home, ".git", "HEAD"), 1)
	mkdirFile(t, filepath.Join(home, "plain", "a.bin"), 1)
	mkdirFile(t, filepath.Join(home, "repo", ".git", "HEAD"), 1)
	aliased := dataVolumeAlias + home
	if _, err := os.Lstat(aliased); err != nil {
		t.Skip("data volume firmlink is not available on this OS")
	}

	assert.False(t, insideGitCheckout(aliased+"/plain", home))
	assert.True(t, insideGitCheckout(aliased+"/repo", home))
}

func absPath(posix string) string {
	if runtime.GOOS == "windows" {
		return `C:` + filepath.FromSlash(posix)
	}
	return posix
}

func TestProtectedPaths_BackslashHomeEntryIsExpandedOnEveryOS(t *testing.T) {
	home := absPath("/Users/me")

	got := ProtectedPaths(&config.Config{Protected: []string{`~\Photos\raw`}}, home)

	assert.Contains(t, got, filepath.Join(home, "Photos", "raw"))
}

func writeWorktreeMarker(t *testing.T, dir string) {
	t.Helper()
	gitdir := filepath.Join(filepath.Dir(dir), "main.git", "worktrees", filepath.Base(dir))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.ToSlash(gitdir)+"\n"), 0o600))
}
