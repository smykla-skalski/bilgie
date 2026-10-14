package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestDirProvider(t *testing.T, root string, mutate func(*config.Provider)) *DirPatternProvider {
	t.Helper()

	cfg := config.Provider{
		Type:    config.TypeDirPattern,
		Paths:   []string{filepath.Join(root, "sail*")},
		MaxSize: "1G",
		Enabled: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	p, err := NewDirPatternProvider("sail-test", cfg)
	require.NoError(t, err)
	p.openMany = perDir(func(context.Context, string) (bool, error) { return false, nil })
	return p
}

// perDir adapts a per-directory answer to the one-listing hook.
func perDir(fn func(context.Context, string) (bool, error)) func(context.Context, []string) (map[string]bool, error) {
	return func(ctx context.Context, dirs []string) (map[string]bool, error) {
		open := make(map[string]bool, len(dirs))
		for _, dir := range dirs {
			isOpen, err := fn(ctx, dir)
			if err != nil {
				return nil, err
			}
			open[dir] = isOpen
		}
		return open, nil
	}
}

func makeDir(t *testing.T, root, name string, n int, idle time.Duration) string {
	t.Helper()

	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "data.bin"), make([]byte, n), 0o600))
	ageTree(t, dir, idle)
	return dir
}

func ageTree(t *testing.T, dir string, idle time.Duration) {
	t.Helper()

	when := time.Now().Add(-idle)
	var paths []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}))
	for _, path := range slices.Backward(paths) {
		require.NoError(t, os.Chtimes(path, when, when))
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestDirPatternRemovesStaleDirectories(t *testing.T) {
	root := t.TempDir()
	stale := makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	other := makeDir(t, root, "sail-b", 500, 3*time.Hour)
	unmatched := makeDir(t, root, "keep", 100, 10*time.Hour)

	p := newTestDirProvider(t, root, nil)
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.False(t, exists(stale))
	assert.False(t, exists(other))
	assert.True(t, exists(unmatched), "paths outside the glob must be untouched")
	assert.Equal(t, int64(1500), res.BytesCleaned)
	assert.Equal(t, int64(2), res.FilesDeleted)
	assert.Contains(t, res.Output, "removed: "+stale)
}

func TestDirPatternSkipRules(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, root string) string
		mutate     func(*config.Provider)
		openCheck  func(context.Context, string) (bool, error)
		wantReason string
	}{
		{
			name: "newer than min_idle",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				return makeDir(t, root, "sail-new", 10, 30*time.Minute)
			},
			wantReason: "min_idle",
		},
		{
			name: "fresh file deep in an old tree",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				dir := makeDir(t, root, "sail-deep", 10, 10*time.Hour)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "fresh.txt"), []byte("x"), 0o600))
				return dir
			},
			wantReason: "min_idle",
		},
		{
			name: "custom min_idle",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				return makeDir(t, root, "sail-custom", 10, 5*time.Hour)
			},
			mutate:     func(c *config.Provider) { c.MinIdle = "24h" },
			wantReason: "min_idle 24h0m0s",
		},
		{
			name: "open files",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				return makeDir(t, root, "sail-open", 10, 10*time.Hour)
			},
			openCheck:  func(context.Context, string) (bool, error) { return true, nil },
			wantReason: "has open files",
		},
		{
			name: "open check failure is conservative",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				return makeDir(t, root, "sail-lsof", 10, 10*time.Hour)
			},
			openCheck:  func(context.Context, string) (bool, error) { return false, errors.New("boom") },
			wantReason: "open-file check failed",
		},
		{
			name: ".git directory at top level",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				dir := makeDir(t, root, "sail-git", 10, 10*time.Hour)
				linkWorktree(t, filepath.Join(dir, ".git"), t.TempDir())
				ageTree(t, dir, 10*time.Hour)
				return dir
			},
			wantReason: "linked worktree outside the root",
		},
		{
			name: ".git file (linked worktree) nested deeper",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				dir := makeDir(t, root, "sail-gitfile", 10, 10*time.Hour)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", ".git"), []byte("gitdir: "+t.TempDir()+"\n"), 0o600))
				ageTree(t, dir, 10*time.Hour)
				return dir
			},
			wantReason: "git data outside the root",
		},
		{
			name: ".GIT directory in another letter case",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				dir := makeDir(t, root, "sail-gitcase", 10, 10*time.Hour)
				linkWorktree(t, filepath.Join(dir, ".GIT"), t.TempDir())
				ageTree(t, dir, 10*time.Hour)
				return dir
			},
			wantReason: "linked worktree outside the root",
		},
		{
			name: ".Git file nested deeper",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				dir := makeDir(t, root, "sail-gitcasefile", 10, 10*time.Hour)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", ".Git"), []byte("gitdir: "+t.TempDir()+"\n"), 0o600))
				ageTree(t, dir, 10*time.Hour)
				return dir
			},
			wantReason: "git data outside the root",
		},
		{
			name: "regular file matching the glob",
			setup: func(t *testing.T, root string) string {
				t.Helper()
				f := filepath.Join(root, "sail-file")
				require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
				return f
			},
			wantReason: "not a directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			target := tt.setup(t, root)

			p := newTestDirProvider(t, root, tt.mutate)
			if tt.openCheck != nil {
				p.openMany = perDir(tt.openCheck)
			}

			res, err := p.Clean(t.Context(), CleanOptions{})
			require.NoError(t, err)

			assert.True(t, exists(target), "guarded path must survive")
			assert.Zero(t, res.BytesCleaned)
			assert.Contains(t, res.Output, "skip: "+target)
			assert.Contains(t, res.Output, tt.wantReason)
		})
	}
}

func TestDirPatternGuardsCanBeDisabled(t *testing.T) {
	root := t.TempDir()
	dir := makeDir(t, root, "sail-git", 10, 10*time.Hour)
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o750))
	ageTree(t, dir, 10*time.Hour)

	off := false
	p := newTestDirProvider(t, root, func(c *config.Provider) {
		c.SkipIfGitWorktree = &off
		c.SkipIfOpen = &off
	})
	p.openMany = perDir(func(context.Context, string) (bool, error) { return true, nil })

	_, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.False(t, exists(dir))
}

func TestDirPatternSymlinkIsNeverFollowed(t *testing.T) {
	root := t.TempDir()
	outside := makeDir(t, t.TempDir(), "precious", 10, 10*time.Hour)
	link := filepath.Join(root, "sail-link")
	require.NoError(t, os.Symlink(outside, link))

	inner := makeDir(t, root, "sail-inner", 10, 10*time.Hour)
	require.NoError(t, os.Symlink(outside, filepath.Join(inner, "escape")))
	ageTree(t, inner, 10*time.Hour)

	p := newTestDirProvider(t, root, func(c *config.Provider) { c.MinIdle = "0s" })
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.True(t, exists(link), "top-level symlink is skipped")
	assert.Contains(t, res.Output, "skip: "+link+" (symlink)")
	assert.False(t, exists(inner))
	assert.True(t, exists(filepath.Join(outside, "sub", "data.bin")), "symlink target must survive")
	assert.Equal(t, int64(10), res.BytesCleaned)
}

func TestDirPatternDryRunDeletesNothing(t *testing.T) {
	root := t.TempDir()
	stale := makeDir(t, root, "sail-stale", 2048, 5*time.Hour)
	fresh := makeDir(t, root, "sail-fresh", 10, time.Minute)

	p := newTestDirProvider(t, root, nil)
	res, err := p.Clean(t.Context(), CleanOptions{DryRun: true})
	require.NoError(t, err)

	assert.True(t, exists(stale))
	assert.True(t, exists(fresh))
	assert.Equal(t, int64(2048), res.BytesCleaned)
	assert.Contains(t, res.Output, "would remove: "+stale)
	assert.Contains(t, res.Output, "2.0 KiB")
	assert.Contains(t, res.Output, "idle 5h0m")
	assert.Contains(t, res.Output, "skip: "+fresh)
	assert.Contains(t, res.Output, "min_idle")
}

func TestDirPatternSmartModeRemovesWholeDirectories(t *testing.T) {
	root := t.TempDir()
	stale := makeDir(t, root, "sail-a", 100, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	_, err := p.Clean(t.Context(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.False(t, exists(stale), "no empty directory may be left behind")
}

func TestDirPatternProtectedPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	p, err := NewDirPatternProvider("x", config.Provider{
		Type: config.TypeDirPattern, Paths: []string{filepath.Join(home, "sail*")}, MaxSize: "1G",
	})
	require.NoError(t, err)

	assert.True(t, p.isProtected(home))
	assert.True(t, p.isProtected(filepath.Dir(home)))
	assert.True(t, p.isProtected(string(filepath.Separator)))
	assert.True(t, p.isProtected(filepath.Join(home, "Documents")))
	assert.True(t, p.isProtected("/Applications"))
	assert.False(t, p.isProtected(filepath.Join(home, "work", "sail-a")))
	assert.False(t, p.isProtected(filepath.Join(os.TempDir(), "sail-a")))
}

func TestDirPatternProtectedMatchIsSkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	p, err := NewDirPatternProvider("x", config.Provider{
		Type: config.TypeDirPattern, Paths: []string{filepath.Join(home, "sail*")}, MaxSize: "1G", MinIdle: "0s",
	})
	require.NoError(t, err)
	p.paths = []string{home}
	p.openMany = perDir(func(context.Context, string) (bool, error) { return false, nil })

	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "protected path")
	assert.True(t, exists(home))
}

func TestDirPatternUnreadableSubtreeIsSkipped(t *testing.T) {
	skipWithoutModeBits(t)
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}

	root := t.TempDir()
	dir := makeDir(t, root, "sail-locked", 10, 10*time.Hour)
	locked := filepath.Join(dir, "sub")
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	p := newTestDirProvider(t, root, nil)
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.True(t, exists(dir))
	assert.Contains(t, res.Output, "scan failed")
}

func TestDirPatternCancelledContext(t *testing.T) {
	root := t.TempDir()
	dir := makeDir(t, root, "sail-a", 10, 5*time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	p := newTestDirProvider(t, root, nil)
	_, err := p.Clean(ctx, CleanOptions{})
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, exists(dir))
}

func TestNewDirPatternProviderValidation(t *testing.T) {
	_, err := NewDirPatternProvider("x", config.Provider{
		Type: config.TypeDirPattern, Paths: []string{"/nonexistent-cb/sail*"}, MaxSize: "1G", MinIdle: "soon",
	})
	require.ErrorContains(t, err, "min_idle")

	p, err := NewDirPatternProvider("x", config.Provider{
		Type: config.TypeDirPattern, Paths: []string{"/nonexistent-cb/sail*"}, MaxSize: "1G",
	})
	require.NoError(t, err)
	assert.Equal(t, 2*time.Hour, p.minIdle, "default min_idle")
	assert.True(t, p.skipIfOpen)
	assert.True(t, p.skipIfGitWorktree)
	assert.True(t, p.Available())
}

func TestNewProviderSelectsDirPattern(t *testing.T) {
	p, err := NewProvider("sail-dirs", config.Provider{
		Type: config.TypeDirPattern, Paths: []string{"/nonexistent-cb/sail*"}, MaxSize: "1G",
	})
	require.NoError(t, err)
	assert.IsType(t, &DirPatternProvider{}, p)
}

func TestDefaultSailProviderIsOptIn(t *testing.T) {
	cfg := config.DefaultConfig()
	sail, ok := cfg.GetProvider("sail-dirs")
	require.True(t, ok)

	assert.False(t, sail.Enabled, "destructive provider must be opt-in")
	assert.Contains(t, sail.Paths, filepath.Join(os.TempDir(), "sail*"))
	assert.LessOrEqual(t, len(sail.Paths), 2, "OS temp dir plus the system temp dir at most")
	assert.Equal(t, config.TypeDirPattern, sail.Type)
	assert.NotContains(t, cfg.AllEnabledProviders(), "sail-dirs")
}

func TestDirPatternActivityDuringChecksBlocksRemoval(t *testing.T) {
	root := t.TempDir()
	dir := makeDir(t, root, "sail-busy", 10, 10*time.Hour)

	p := newTestDirProvider(t, root, nil)
	p.openMany = perDir(func(context.Context, string) (bool, error) {
		return false, os.WriteFile(filepath.Join(dir, "sub", "late.txt"), []byte("x"), 0o600)
	})

	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.True(t, exists(dir))
	assert.Contains(t, res.Output, "modified during checks")
}

func TestDirPatternGitAppearingDuringChecksBlocksRemoval(t *testing.T) {
	root := t.TempDir()
	dir := makeDir(t, root, "sail-late-git", 10, 10*time.Hour)

	p := newTestDirProvider(t, root, nil)
	p.openMany = perDir(func(context.Context, string) (bool, error) {
		return false, os.Mkdir(filepath.Join(dir, ".git"), 0o750)
	})

	_, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.True(t, exists(dir))
}

func TestNewDirPatternProviderRejectsLiteralPaths(t *testing.T) {
	_, err := NewDirPatternProvider("x", config.Provider{
		Type: config.TypeDirPattern, Paths: []string{t.TempDir()}, MaxSize: "1G",
	})
	require.ErrorContains(t, err, "must contain a glob")
}

func TestNewDirPatternProviderRejectsRelativePaths(t *testing.T) {
	for _, path := range []string{"*", "Documents/*", "-x*", "./sail*"} {
		_, err := NewDirPatternProvider("x", config.Provider{
			Type: config.TypeDirPattern, Paths: []string{path}, MaxSize: "1G",
		})
		require.ErrorContains(t, err, "absolute", path)
	}
}

func TestDirPatternRelativeMatchIsProtected(t *testing.T) {
	p := newTestDirProvider(t, t.TempDir(), nil)
	assert.True(t, p.isProtected("Documents"))
}

func TestDirPatternProtectsHomeReachedThroughSymlink(t *testing.T) {
	sandbox := t.TempDir()
	home := filepath.Join(sandbox, "home")
	require.NoError(t, os.Mkdir(home, 0o750))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	require.NoError(t, os.Symlink(home, filepath.Join(sandbox, "homelink")))
	victim := makeDir(t, home, "sail-home", 10, 10*time.Hour)

	p, err := NewDirPatternProvider("x", config.Provider{
		Type: config.TypeDirPattern, Paths: []string{filepath.Join(sandbox, "homelink", "sail*")}, MaxSize: "1G",
	})
	require.NoError(t, err)
	p.openMany = perDir(func(context.Context, string) (bool, error) { return false, nil })

	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.True(t, exists(victim))
	assert.Contains(t, res.Output, "protected path")
}

func TestDirPatternNeverRemovesGitDirectoryItself(t *testing.T) {
	root := t.TempDir()
	repo := makeDir(t, root, "repo", 10, 10*time.Hour)
	gitDir := filepath.Join(repo, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects"), 0o750))
	ageTree(t, repo, 10*time.Hour)

	p, err := NewDirPatternProvider("x", config.Provider{
		Type: config.TypeDirPattern, Paths: []string{filepath.Join(root, "*", ".git*")}, MaxSize: "1G",
	})
	require.NoError(t, err)
	p.openMany = perDir(func(context.Context, string) (bool, error) { return false, nil })

	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.True(t, exists(gitDir))
	assert.Contains(t, res.Output, "is a .git directory")
}

func TestDirPatternSkipsTheTempDirInUse(t *testing.T) {
	root := t.TempDir()
	inUse := makeDir(t, root, "sailbox", 10, 10*time.Hour)
	other := makeDir(t, root, "sail-old", 10, 10*time.Hour)
	p := newTestDirProvider(t, root, nil)
	p.keep = []string{filepath.Join(inUse, "sub")}

	res, err := p.Clean(context.Background(), CleanOptions{})

	require.NoError(t, err)
	assert.DirExists(t, inUse, "an ancestor of the temp dir in use is never removed")
	assert.NoDirExists(t, other)
	assert.Contains(t, res.Output, "contains the temp dir in use")
}

// linkOrSkip hard-links oldname to newname, skipping on file systems that
// cannot.
func linkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Link(oldname, newname); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
}

func TestDirPatternCountsHardlinkedFileOnce(t *testing.T) {
	root := t.TempDir()
	dir := makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	data := filepath.Join(dir, "sub", "data.bin")
	linkOrSkip(t, data, filepath.Join(dir, "copy.bin"))
	linkOrSkip(t, data, filepath.Join(dir, "sub", "again.bin"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unique.bin"), make([]byte, 200), 0o600))
	ageTree(t, dir, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	dry, err := p.Clean(t.Context(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, int64(1200), dry.BytesCleaned, "three links to one inode are 1000 bytes once")
	assert.Equal(t, int64(1200), dry.Entries[0].Size)

	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1200), res.BytesCleaned)
	assert.False(t, exists(dir))
}

func TestDirPatternCountsHardlinkAcrossDirectoriesOnce(t *testing.T) {
	root := t.TempDir()
	first := makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	second := makeDir(t, root, "sail-b", 300, 5*time.Hour)
	linkOrSkip(t, filepath.Join(first, "sub", "data.bin"), filepath.Join(second, "shared.bin"))
	ageTree(t, second, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	dry, err := p.Clean(t.Context(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, int64(1300), dry.BytesCleaned, "the shared 1000 bytes appear in the dry-run total once: %s", dry.Output)
	var sum int64
	for _, e := range dry.Entries {
		sum += e.Size
	}
	assert.Equal(t, dry.BytesCleaned, sum, "entry sizes add up to the total")

	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1300), res.BytesCleaned)
}

func TestDirPatternSkipsBytesOfInodeWithSurvivingLink(t *testing.T) {
	root := t.TempDir()
	dir := makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	outside := filepath.Join(t.TempDir(), "keep.bin")
	require.NoError(t, os.WriteFile(outside, make([]byte, 700), 0o600))
	linkOrSkip(t, outside, filepath.Join(dir, "olink.bin"))
	ageTree(t, dir, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.Equal(t, int64(1000), res.BytesCleaned, "the inode kept by a link outside frees nothing")
	assert.True(t, exists(outside))
}

func TestDirPatternSkippedSiblingKeepsSharedInode(t *testing.T) {
	root := t.TempDir()
	first := makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	fresh := makeDir(t, root, "sail-b", 10, time.Minute)
	linkOrSkip(t, filepath.Join(first, "sub", "data.bin"), filepath.Join(fresh, "shared.bin"))
	ageTree(t, first, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	res, err := p.Clean(t.Context(), CleanOptions{DryRun: true})
	require.NoError(t, err)

	assert.Zero(t, res.BytesCleaned, "the surviving sibling still holds the inode")
}

func TestDirPatternDuplicatePatternsCountOnce(t *testing.T) {
	root := t.TempDir()
	makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	makeDir(t, root, "sail-b", 300, 5*time.Hour)

	p := newTestDirProvider(t, root, func(c *config.Provider) {
		c.Paths = []string{filepath.Join(root, "sail*"), filepath.Join(root, "sail*")}
	})
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.Equal(t, int64(1300), res.BytesCleaned)
	assert.Len(t, res.Entries, 2)
}

// stepClock moves the provider's clock a minute on every read, so each
// open-file check finds the earlier listing stale and takes a new one.
func stepClock(p *DirPatternProvider) {
	base := time.Now()
	var n int
	p.now = func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Minute)
	}
}

func TestDirPatternSkippedByRecheckDoesNotCreditSharedInode(t *testing.T) {
	root := t.TempDir()
	first := makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	require.NoError(t, os.WriteFile(filepath.Join(first, "own.bin"), make([]byte, 50), 0o600))
	second := makeDir(t, root, "sail-b", 10, 5*time.Hour)
	linkOrSkip(t, filepath.Join(first, "sub", "data.bin"), filepath.Join(second, "shared.bin"))
	ageTree(t, first, 5*time.Hour)
	ageTree(t, second, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	stepClock(p)
	var calls int
	p.openMany = func(context.Context, []string) (map[string]bool, error) {
		calls++
		if calls == 3 {
			return nil, os.WriteFile(filepath.Join(second, "late.txt"), []byte("x"), 0o600)
		}
		return map[string]bool{}, nil
	}
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.False(t, exists(first))
	assert.True(t, exists(second))
	assert.Equal(t, int64(50), res.BytesCleaned, "the inode still linked from the kept sibling frees nothing")
	assert.Contains(t, res.Output, "skip: "+second+" (modified during checks)")
}

func TestDirPatternRecheckRepeatsTheOpenFilesCheck(t *testing.T) {
	root := t.TempDir()
	first := makeDir(t, root, "sail-a", 10, 5*time.Hour)
	second := makeDir(t, root, "sail-b", 10, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	stepClock(p)
	var calls int
	p.openMany = func(_ context.Context, dirs []string) (map[string]bool, error) {
		calls++
		return map[string]bool{first: calls == 2}, nil
	}
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.True(t, exists(first), "opened between evaluation and removal")
	assert.False(t, exists(second))
	assert.Contains(t, res.Output, "skip: "+first+" (has open files)")
}

func TestDirPatternTakesOneOpenFileListingPerPass(t *testing.T) {
	root := t.TempDir()
	const dirs = 20
	for i := range dirs {
		makeDir(t, root, fmt.Sprintf("sail-%02d", i), 10, 5*time.Hour)
	}

	p := newTestDirProvider(t, root, nil)
	var calls int
	p.openMany = func(context.Context, []string) (map[string]bool, error) {
		calls++
		return map[string]bool{}, nil
	}
	res, err := p.Clean(t.Context(), CleanOptions{DryRun: true})
	require.NoError(t, err)

	assert.Len(t, res.Entries, dirs)
	assert.Equal(t, 1, calls)
}

func TestDirPatternFailedListingCountsEveryDirectoryAsOpen(t *testing.T) {
	root := t.TempDir()
	makeDir(t, root, "sail-a", 10, 5*time.Hour)
	makeDir(t, root, "sail-b", 10, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	p.openMany = func(context.Context, []string) (map[string]bool, error) { return nil, errors.New("boom") }
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.Zero(t, res.BytesCleaned)
	assert.Equal(t, 2, strings.Count(res.Output, "open-file check failed"), res.Output)
}

func TestDirPatternRemovesOldestFirstAndStopsOnceRecovered(t *testing.T) {
	root := t.TempDir()
	oldest := makeDir(t, root, "sail-c", 10, 9*time.Hour)
	middle := makeDir(t, root, "sail-a", 10, 7*time.Hour)
	newest := makeDir(t, root, "sail-b", 10, 5*time.Hour)
	require.NoError(t, os.Chtimes(oldest, time.Now().Add(-9*time.Hour), time.Now().Add(-9*time.Hour)))
	require.NoError(t, os.Chtimes(middle, time.Now().Add(-7*time.Hour), time.Now().Add(-7*time.Hour)))
	require.NoError(t, os.Chtimes(newest, time.Now().Add(-5*time.Hour), time.Now().Add(-5*time.Hour)))

	p := newTestDirProvider(t, root, nil)
	res, err := p.Clean(t.Context(), CleanOptions{
		Recovered: func(freed int64) bool { return freed >= 20 },
	})
	require.NoError(t, err)

	assert.False(t, exists(oldest))
	assert.False(t, exists(middle))
	assert.True(t, exists(newest), "the sweep stops once free space is back")
	assert.Equal(t, int64(20), res.BytesCleaned)
}

func TestDirPatternCancelledSweepKeepsWhatItFreed(t *testing.T) {
	root := t.TempDir()
	first := makeDir(t, root, "sail-a", 10, 9*time.Hour)
	second := makeDir(t, root, "sail-b", 10, 5*time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	p := newTestDirProvider(t, root, nil)
	res, err := p.Clean(ctx, CleanOptions{
		Recovered: func(int64) bool {
			cancel()
			return false
		},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, exists(first))
	assert.True(t, exists(second))
	assert.Equal(t, int64(10), res.BytesCleaned)
	assert.Contains(t, res.Output, "removed: "+first)
}

func TestDirPatternWalkStopsAtTheFirstRecentFile(t *testing.T) {
	root := t.TempDir()
	dir := makeDir(t, root, "sail-a", 10, 5*time.Hour)
	for i := range 50 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d", i)), []byte("x"), 0o600))
	}
	ageTree(t, dir, 5*time.Hour)
	recent := time.Now()
	require.NoError(t, os.Chtimes(filepath.Join(dir, "f00"), recent, recent))

	cutoff := time.Now().Add(-2 * time.Hour)
	sc, reason := scanDir(t.Context(), dir, time.Now().Add(-5*time.Hour), cutoff)

	assert.Empty(t, reason)
	assert.True(t, sc.newest.After(cutoff))
	assert.Less(t, sc.files, int64(50), "the walk stops instead of visiting every file")
}

func TestDirPatternNestedMatchesAreScannedOnce(t *testing.T) {
	root := t.TempDir()
	outer := makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	inner := filepath.Join(outer, "sail-inner")
	require.NoError(t, os.MkdirAll(inner, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(inner, "x.bin"), make([]byte, 500), 0o600))
	linkOrSkip(t, filepath.Join(inner, "x.bin"), filepath.Join(outer, "x-link.bin"))
	ageTree(t, outer, 5*time.Hour)

	p := newTestDirProvider(t, root, func(c *config.Provider) {
		c.Paths = []string{filepath.Join(root, "sail*"), filepath.Join(root, "sail*", "sail*")}
	})
	require.Len(t, p.paths, 2)
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.Equal(t, int64(1500), res.BytesCleaned)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, outer, res.Entries[0].Path)
	assert.NotContains(t, res.Output, "skip:")
	assert.False(t, exists(outer))
}

func TestDirPatternTwoSpellingsOfOneDirectoryCountOnce(t *testing.T) {
	root := t.TempDir()
	makeDir(t, root, "sail-a", 1000, 5*time.Hour)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	p := newTestDirProvider(t, root, func(c *config.Provider) {
		c.Paths = []string{filepath.Join(root, "sail*"), filepath.Join(alias, "sail*")}
	})
	require.Len(t, p.paths, 2)
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.Equal(t, int64(1000), res.BytesCleaned)
	assert.Len(t, res.Entries, 1)
	assert.Equal(t, 1, strings.Count(res.Output, "removed:"), res.Output)
}

func TestDirPatternVanishedDirectoryIsSkippedNotRemoved(t *testing.T) {
	root := t.TempDir()
	first := makeDir(t, root, "sail-a", 10, 5*time.Hour)
	second := makeDir(t, root, "sail-b", 10, 5*time.Hour)

	p := newTestDirProvider(t, root, nil)
	stepClock(p)
	var calls int
	p.openMany = func(context.Context, []string) (map[string]bool, error) {
		calls++
		if calls == 3 {
			return nil, os.RemoveAll(second)
		}
		return map[string]bool{}, nil
	}
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.NotContains(t, res.Output, "removed: "+second)
	assert.Contains(t, res.Output, "removed: "+first)
	assert.Len(t, res.Entries, 1)
}
