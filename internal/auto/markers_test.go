package auto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

func withMarkerLimits(t *testing.T, lim scanLimits) {
	t.Helper()
	old := markerLimits
	markerLimits = lim
	t.Cleanup(func() { markerLimits = old })
}

func TestRun_MiseShapedCacheIsNotSkipped(t *testing.T) {
	h := newHarness(t)
	root := h.dir(".local", "share", "mise")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "downloads", "opencode", "1.2.3"), 0o750))
	touch(t, filepath.Join(root, "shims", "opencode"))
	touch(t, filepath.Join(root, "installs", "node", "22", "bin", "node"))
	h.add("mise", true, true, func(f *fakeProvider, pc *config.Provider) {
		pc.Paths = []string{root}
		f.paths = []string{root}
	})

	_, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"mise"}, h.calls)
	assert.NotContains(t, h.out.String(), "protected")
}

func TestRun_ExactOpencodeDirsStayProtected(t *testing.T) {
	for _, parts := range [][]string{
		{".local", "share", "opencode"},
		{".local", "share", "opencode", "storage"},
		{".config", "opencode"},
		{".cache", "opencode"},
		{".opencode"},
		{"Downloads"},
	} {
		h := newHarness(t)
		path := h.dir(parts...)
		h.add("victim", true, true, func(f *fakeProvider, pc *config.Provider) {
			pc.Paths = []string{path}
			f.paths = []string{path}
		})

		_, err := h.run(false, 1*gib)

		require.NoError(t, err)
		assert.Empty(t, h.calls, parts)
		assert.Contains(t, h.out.String(), "protected path", parts)
	}
}

func TestFindGitMarker_DepthAndKinds(t *testing.T) {
	tests := []struct {
		name      string
		files     []string
		worktrees []string
		want      markerOutcome
	}{
		{"git dir at root", []string{".git/HEAD"}, nil, markerFound},
		{"worktree file at depth 2", nil, []string{"a/w"}, markerFound},
		{"worktree file at depth 3", nil, []string{"a/b/w"}, markerFound},
		{"buried past the bound is accepted", nil, []string{"a/b/c/d/w"}, markerNone},
		{"no marker", []string{"a/b/file"}, nil, markerNone},
		{"gitignore is not a marker", []string{".gitignore"}, nil, markerNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tt.files {
				touch(t, filepath.Join(root, filepath.FromSlash(f)))
			}
			for _, w := range tt.worktrees {
				writeWorktreeMarker(t, filepath.Join(root, filepath.FromSlash(w)))
			}

			assert.Equal(t, tt.want, findGitMarker(t.Context(), root, markerLimits).outcome)
		})
	}
}

func TestFindGitMarker_DoesNotFollowSymlinks(t *testing.T) {
	repo := t.TempDir()
	touch(t, filepath.Join(repo, ".git", "HEAD"))
	root := t.TempDir()
	touch(t, filepath.Join(root, "file"))
	if err := os.Symlink(repo, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	assert.Equal(t, markerNone, findGitMarker(t.Context(), root, markerLimits).outcome)
}

func TestFindGitMarker_MissingRootIsClear(t *testing.T) {
	assert.Equal(t, markerNone, findGitMarker(t.Context(), filepath.Join(t.TempDir(), "gone"), markerLimits).outcome)
}

func lockDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
}

func TestFindGitMarker_UnreadableRootFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	lockDir(t, root)

	res := findGitMarker(t.Context(), root, markerLimits)

	assert.Equal(t, markerUnreadable, res.outcome)
	assert.Contains(t, res.detail, root)
}

func TestFindGitMarker_UnreadableSubdirIsSkippedAndSiblingsScanned(t *testing.T) {
	tests := []struct {
		name     string
		locked   []string
		sibling  string
		wantHit  bool
		wantPath string
	}{
		{name: "clean siblings", locked: []string{"a", "locked"}, sibling: "b/c"},
		{name: "deep locked dir", locked: []string{"a", "b", "locked"}, sibling: "a/c"},
		{name: "checkout beside locked dir", locked: []string{"locked"}, sibling: "repo/.git", wantHit: true, wantPath: "repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			lockDir(t, filepath.Join(append([]string{root}, tt.locked...)...))
			touch(t, filepath.Join(root, tt.sibling, "file"))

			res := findGitMarker(t.Context(), root, markerLimits)

			if !tt.wantHit {
				assert.Equal(t, markerNone, res.outcome)
				return
			}
			assert.Equal(t, markerFound, res.outcome)
			assert.Equal(t, filepath.Join(root, tt.wantPath), res.detail)
		})
	}
}

type faultyDir struct {
	entries []os.DirEntry
	err     error
}

func (d *faultyDir) ReadDir(int) ([]os.DirEntry, error) { return d.entries, d.err }
func (d *faultyDir) Close() error                       { return nil }

func TestFindGitMarker_PermissionErrorsSkipOnlySubdirectories(t *testing.T) {
	denied := &fs.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}
	broken := errors.New("input/output error")
	tests := []struct {
		name     string
		fault    func(rel string) (dirReader, error)
		want     markerOutcome
		wantPath string
	}{
		{
			name:  "subdir open denied",
			fault: func(rel string) (dirReader, error) { return nil, onlyAt(rel, "a", denied) },
			want:  markerFound, wantPath: "b",
		},
		{
			name:  "subdir read denied after entries",
			fault: func(rel string) (dirReader, error) { return readFault(rel, "a", denied), nil },
			want:  markerFound, wantPath: "b",
		},
		{
			name:  "subdir open fails otherwise",
			fault: func(rel string) (dirReader, error) { return nil, onlyAt(rel, "a", broken) },
			want:  markerUnreadable,
		},
		{
			name:  "subdir read fails otherwise",
			fault: func(rel string) (dirReader, error) { return readFault(rel, "a", broken), nil },
			want:  markerUnreadable,
		},
		{
			name:  "root open denied",
			fault: func(rel string) (dirReader, error) { return nil, onlyAt(rel, ".", denied) },
			want:  markerUnreadable,
		},
		{
			name:  "root read denied",
			fault: func(rel string) (dirReader, error) { return readFault(rel, ".", denied), nil },
			want:  markerUnreadable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			old := openDir
			openDir = func(path string) (dirReader, error) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return nil, err
				}
				rel = filepath.ToSlash(rel)
				if d, ferr := tt.fault(rel); ferr != nil || d != nil {
					return d, ferr
				}
				switch rel {
				case ".":
					return &fakeDir{entries: []os.DirEntry{fakeEntry{name: "a", dir: true}, fakeEntry{name: "b", dir: true}}}, nil
				case "b":
					return &fakeDir{entries: []os.DirEntry{fakeEntry{name: ".git", dir: true}}}, nil
				}
				return &fakeDir{}, nil
			}
			t.Cleanup(func() { openDir = old })

			res := findGitMarker(t.Context(), root, markerLimits)

			assert.Equal(t, tt.want, res.outcome)
			if tt.wantPath != "" {
				assert.Equal(t, filepath.Join(root, tt.wantPath), res.detail)
			}
		})
	}
}

func onlyAt(rel, target string, err error) error {
	if rel == target {
		return err
	}
	return nil
}

func readFault(rel, target string, err error) dirReader {
	if rel != target {
		return nil
	}
	return &faultyDir{entries: []os.DirEntry{fakeEntry{name: "file"}}, err: err}
}

func TestFindGitMarker_LimitsReportTooLargeWithPath(t *testing.T) {
	root := t.TempDir()
	for i := range 5 {
		touch(t, filepath.Join(root, fmt.Sprintf("d%d", i), "a"))
	}
	lim := markerLimits

	lim.dirs = 3
	res := findGitMarker(t.Context(), root, lim)
	assert.Equal(t, markerTooLarge, res.outcome)

	lim = markerLimits
	lim.entries = 3
	assert.Equal(t, markerTooLarge, findGitMarker(t.Context(), root, lim).outcome)
}

func TestFindGitMarker_CancelledContextStopsPromptly(t *testing.T) {
	root := t.TempDir()
	for i := range 50 {
		touch(t, filepath.Join(root, fmt.Sprintf("d%d", i), "a"))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	res := findGitMarker(ctx, root, markerLimits)

	assert.Equal(t, markerCancelled, res.outcome)
	assert.Less(t, time.Since(start), time.Second)
}

func TestFindGitMarker_ParentDeadlineIsACancelNotTooLarge(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a"))
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	assert.Equal(t, markerCancelled, findGitMarker(ctx, root, markerLimits).outcome)
}

type fakeEntry struct {
	name string
	dir  bool
}

func (e fakeEntry) Name() string { return e.name }
func (e fakeEntry) IsDir() bool  { return e.dir }
func (e fakeEntry) Type() os.FileMode {
	if e.dir {
		return os.ModeDir
	}
	return 0
}
func (e fakeEntry) Info() (os.FileInfo, error) { return nil, os.ErrInvalid }

type fakeDir struct {
	entries []os.DirEntry
	pos     int
}

func (d *fakeDir) ReadDir(n int) ([]os.DirEntry, error) {
	if d.pos >= len(d.entries) {
		return nil, io.EOF
	}
	end := min(d.pos+n, len(d.entries))
	out := d.entries[d.pos:end]
	d.pos = end
	return out, nil
}

func (d *fakeDir) Close() error { return nil }

func useFakeTree(t *testing.T, root string, layout func(rel string) []os.DirEntry) *int {
	t.Helper()
	reads := new(int)
	old := openDir
	openDir = func(path string) (dirReader, error) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		*reads++
		return &fakeDir{entries: layout(filepath.ToSlash(rel))}, nil
	}
	t.Cleanup(func() { openDir = old })
	return reads
}

func syntheticFiles(n int) []os.DirEntry {
	out := make([]os.DirEntry, n)
	for i := range out {
		out[i] = fakeEntry{name: fmt.Sprintf("f%d", i)}
	}
	return out
}

func TestCheckProtected_600000FilesWithoutMarkerVerifyQuickly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mod")
	layout := func(rel string) []os.DirEntry {
		switch strings.Count(rel, "/") {
		case 0:
			if rel == "." {
				out := make([]os.DirEntry, 10)
				for i := range out {
					out[i] = fakeEntry{name: fmt.Sprintf("a%d", i), dir: true}
				}
				return out
			}
			return []os.DirEntry{fakeEntry{name: "b", dir: true}}
		case 1:
			out := make([]os.DirEntry, 60)
			for i := range out {
				out[i] = fakeEntry{name: fmt.Sprintf("c%d", i), dir: true}
			}
			return out
		default:
			return syntheticFiles(1000)
		}
	}
	reads := useFakeTree(t, root, layout)

	start := time.Now()
	v := checkProtected(t.Context(), root, t.TempDir(), nil, true)
	elapsed := time.Since(start)

	assert.Equal(t, verdictClear, v.kind, v.detail)
	assert.Less(t, elapsed, 5*time.Second)
	assert.Equal(t, 1+10+10+600, *reads, "only directories are listed, never their files")
}

func TestFindGitMarker_RealTreeOfManySmallFiles(t *testing.T) {
	root := t.TempDir()
	for d := range 20 {
		dir := filepath.Join(root, fmt.Sprintf("d%d", d), "sub")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		for i := range 500 {
			require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), nil, 0o600))
		}
	}

	assert.Equal(t, markerNone, findGitMarker(t.Context(), root, markerLimits).outcome)
}

func TestFindGitMarker_StopsAtFirstHit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mod")
	layout := func(rel string) []os.DirEntry {
		if rel == "." {
			return []os.DirEntry{fakeEntry{name: "a", dir: true}, fakeEntry{name: ".git"}, fakeEntry{name: "b", dir: true}}
		}
		return syntheticFiles(10)
	}
	reads := useFakeTree(t, root, layout)

	assert.Equal(t, markerFound, findGitMarker(t.Context(), root, markerLimits).outcome)
	assert.Equal(t, 1, *reads)
}

func TestFindGitMarker_CancelDuringScanStopsPromptly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mod")
	ctx, cancel := context.WithCancel(t.Context())
	layout := func(rel string) []os.DirEntry {
		if rel == "." {
			return []os.DirEntry{fakeEntry{name: "a", dir: true}}
		}
		cancel()
		return syntheticFiles(100000)
	}
	reads := useFakeTree(t, root, layout)

	res := findGitMarker(ctx, root, markerLimits)

	assert.Equal(t, markerCancelled, res.outcome)
	assert.LessOrEqual(t, *reads, 2)
}

func TestRun_TooLargeToVerifyIsSkippedAndReported(t *testing.T) {
	withMarkerLimits(t, scanLimits{timeout: time.Minute, depth: 3, dirs: 2, entries: 1000})
	h := newHarness(t)
	root := h.dir("big")
	for i := range 5 {
		touch(t, filepath.Join(root, fmt.Sprintf("d%d", i), "a"))
	}
	h.add("big", true, true, func(f *fakeProvider, pc *config.Provider) {
		pc.Paths = []string{root}
		f.paths = []string{root}
	})

	report, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Empty(t, h.calls)
	require.Len(t, report.Results, 1)
	assert.Equal(t, StatusSkipped, report.Results[0].Status)
	assert.Equal(t, "too large to verify: "+root, report.Results[0].Reason)
	assert.Contains(t, h.out.String(), "  big: too large to verify: "+root+"\n")
	assert.Contains(t, h.out.String(), "nothing to clean")
}

func TestRun_CancelledScanReturnsContextError(t *testing.T) {
	h := newHarness(t)
	root := h.dir("c")
	touch(t, filepath.Join(root, "a"))
	h.add("c", true, true, func(f *fakeProvider, pc *config.Provider) {
		pc.Paths = []string{root}
		f.paths = []string{root}
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Run(ctx, h.cfg, false, Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 1 * gib, Total: 1000 * gib}, nil },
		NewProvider: func(name string, _ config.Provider) (provider.Provider, error) { return h.fakes[name], nil },
		Out:         &h.out,
		Home:        h.home,
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, h.calls)
}

func TestRun_SweepProtectsRealWorktreeAndNamedPaths(t *testing.T) {
	home := sandboxHome(t)
	noOpenCheck := false
	wt := filepath.Join(home, "scratch", "sail-keep")
	writeWorktreeMarker(t, wt)
	touch(t, filepath.Join(wt, "a.bin"))
	old := time.Now().Add(-90 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(wt, old, old))
	stale := filepath.Join(home, "scratch", "gone-1", "a.bin")
	writeAged(t, stale)
	named := filepath.Join(home, "Downloads", "sail-x", "a.bin")
	writeAged(t, named)

	cfg := &config.Config{Providers: map[string]config.Provider{}, Auto: autoCfg()}
	add := func(name, glob string) {
		cfg.Providers[name] = config.Provider{
			Type: config.TypeDirPattern, Paths: []string{glob}, MaxSize: "1B",
			MinIdle: "1h", SkipIfOpen: &noOpenCheck,
		}
	}
	add("sweep", filepath.Join(home, "scratch", "sail-*"))
	add("named", filepath.Join(home, "Downloads", "sail-*"))
	add("control", filepath.Join(home, "scratch", "gone-*"))

	var out bytes.Buffer
	_, err := Run(t.Context(), cfg, false, Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 1 * gib, Total: 1000 * gib}, nil },
		NewProvider: provider.NewProvider,
		Out:         &out,
		Home:        home,
	})

	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(wt, "a.bin"), "worktree must survive the sweep")
	assert.FileExists(t, named)
	assert.NoFileExists(t, stale, "control sweep must really delete")
}

func TestCheckProtected_FilePathIsClear(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(t.TempDir(), "cache.log")
	touch(t, file)

	assert.Equal(t, verdictClear, checkProtected(t.Context(), file, home, nil, true).kind)
}

func TestRun_SweepSymlinkIntoProtectedNameIsSkipped(t *testing.T) {
	home := sandboxHome(t)
	noOpenCheck := false
	target := filepath.Join(t.TempDir(), "worktrees", "x")
	writeAged(t, filepath.Join(target, "a.bin"))
	if err := os.MkdirAll(filepath.Join(home, "scratch"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, "scratch", "sail-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := &config.Config{Providers: map[string]config.Provider{"sweep": {
		Type: config.TypeDirPattern, Paths: []string{filepath.Join(home, "scratch", "sail-*")}, MaxSize: "1B",
		MinIdle: "1h", SkipIfOpen: &noOpenCheck,
	}}, Auto: autoCfg()}

	_, err := Run(t.Context(), cfg, false, Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 1 * gib, Total: 1000 * gib}, nil },
		NewProvider: provider.NewProvider,
		Out:         &bytes.Buffer{},
		Home:        home,
	})

	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(target, "a.bin"))
}

func TestRun_VerboseShowsTooLargeAndCannotVerifyReasons(t *testing.T) {
	withMarkerLimits(t, scanLimits{timeout: time.Minute, depth: 3, dirs: 2, entries: 1000})
	h := newHarness(t)
	root := h.dir("big")
	for i := range 5 {
		touch(t, filepath.Join(root, fmt.Sprintf("d%d", i), "a"))
	}
	h.add("big", true, true, func(f *fakeProvider, pc *config.Provider) {
		pc.Paths = []string{root}
		f.paths = []string{root}
	})

	_, err := h.runWith(true, false, 1*gib)

	require.NoError(t, err)
	assert.Contains(t, h.out.String(), "big: skipped (too large to verify: "+root+")")
}

func TestFindGitMarker_FlatTreeOfSubdirsStopsQueueingAtDirLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mod")
	layout := func(rel string) []os.DirEntry {
		out := make([]os.DirEntry, 100000)
		for i := range out {
			out[i] = fakeEntry{name: fmt.Sprintf("d%d", i), dir: true}
		}
		return out
	}
	reads := useFakeTree(t, root, layout)
	lim := markerLimits
	lim.dirs = 10

	assert.Equal(t, markerTooLarge, findGitMarker(t.Context(), root, lim).outcome)
	assert.Equal(t, 1, *reads)
}

func TestFindGitMarker_AnyCaseMarkerIsFound(t *testing.T) {
	for _, name := range []string{".GIT", ".Git", ".gIt"} {
		for _, kind := range []string{"dir", "worktree file"} {
			t.Run(name+" "+kind, func(t *testing.T) {
				root := t.TempDir()
				repo := filepath.Join(root, "p")
				require.NoError(t, os.MkdirAll(repo, 0o750))
				marker := filepath.Join(repo, name)
				if kind == "dir" {
					require.NoError(t, os.Mkdir(marker, 0o750))
				} else {
					require.NoError(t, os.WriteFile(marker, []byte("gitdir: "+filepath.ToSlash(filepath.Join(root, "main.git", "worktrees", "p"))+"\n"), 0o600))
				}
				if _, err := os.Lstat(filepath.Join(repo, name)); err != nil {
					t.Skipf("cannot create %s on this filesystem: %v", name, err)
				}

				res := findGitMarker(t.Context(), root, markerLimits)

				assert.Equal(t, markerFound, res.outcome)
				assert.Equal(t, repo, res.detail)
			})
		}
	}
}

func TestCheckProtected_AnyCaseMarkerProtectsProvider(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "p", ".GIT"), 0o750))

	v := checkProtected(t.Context(), root, t.TempDir(), nil, true)

	assert.Equal(t, verdictProtected, v.kind)
}

func TestInsideGitCheckout_AnyCaseMarkerOnAncestor(t *testing.T) {
	for _, name := range []string{".GIT", ".Git"} {
		for _, kind := range []string{"dir", "worktree file"} {
			t.Run(name+" "+kind, func(t *testing.T) {
				home := t.TempDir()
				repo := filepath.Join(home, "proj")
				require.NoError(t, os.MkdirAll(filepath.Join(repo, "node_modules", ".cache"), 0o750))
				marker := filepath.Join(repo, name)
				if kind == "dir" {
					require.NoError(t, os.Mkdir(marker, 0o750))
				} else {
					require.NoError(t, os.WriteFile(marker, []byte("gitdir: x\n"), 0o600))
				}
				if _, err := os.Lstat(marker); err != nil {
					t.Skipf("cannot create %s on this filesystem: %v", name, err)
				}

				assert.True(t, insideGitCheckout(filepath.Join(repo, "node_modules", ".cache"), home))
				assert.True(t, insideGitCheckout(repo, home))
				assert.False(t, insideGitCheckout(filepath.Join(home, "other"), home))
			})
		}
	}
}
