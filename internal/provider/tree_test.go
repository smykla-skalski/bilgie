package provider

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeAged(t *testing.T, path string, bytes int, age time.Duration) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, make([]byte, bytes), 0o600))
	when := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, when, when))
}

func ageAll(t *testing.T, root string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	var paths []string
	require.NoError(t, filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		paths = append(paths, p)
		return err
	}))
	for _, p := range paths {
		require.NoError(t, os.Chtimes(p, when, when))
	}
}

// makePackage writes a tree of n files of 100 bytes, all aged alike.
func makePackage(t *testing.T, dir string, n int, age time.Duration) {
	t.Helper()
	for i := range n {
		writeAged(t, filepath.Join(dir, "node_modules", "pkg", "lib", string(rune('a'+i))+".js"), 100, age)
	}
	writeAged(t, filepath.Join(dir, "node_modules", "pkg", "package.json"), 100, age)
	ageAll(t, dir, age)
}

const fakeVerifyTool = "bilgie-fake-verify"

func newTree(t *testing.T, name string, cfg config.Provider) *TreeProvider {
	t.Helper()
	spec, ok := treeSpecs[name]
	require.True(t, ok, name)
	cfg.Enabled = true
	p, err := NewTreeProvider(name, cfg, spec)
	require.NoError(t, err)
	p.procLines = func(context.Context) ([]string, error) { return nil, nil }
	p.imageOnly = false
	p.spec.verify = nil
	if p.busy != nil {
		p.busy.listProcesses = listOf()
	}
	return p
}

func assertWholeOrGone(t *testing.T, dir string, wantFiles int) {
	t.Helper()
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return
	}
	var got int
	require.NoError(t, filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			got++
		}
		return err
	}))
	assert.Equal(t, wantFiles, got, "%s is partly deleted", dir)
}

func npxFixture(t *testing.T) (root string, hashes [3]string) {
	t.Helper()
	root = t.TempDir()
	for i, h := range []string{"h1", "h2", "h3"} {
		dir := filepath.Join(root, "_npx", h)
		makePackage(t, dir, 20, time.Duration(100-10*i)*day)
		hashes[i] = dir
	}
	writeAged(t, filepath.Join(root, "_cacache", "content-v2", "sha512", "aa", "old"), 100, 90*day)
	writeAged(t, filepath.Join(root, "_cacache", "content-v2", "sha512", "bb", "fresh"), 100, day)
	return root, hashes
}

func TestTreeProvider_NpxDryRunListsWholeDirectories(t *testing.T) {
	root, hashes := npxFixture(t)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "3K", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
	require.NoError(t, err)

	assert.Contains(t, res.Output, "would remove: "+hashes[0])
	assert.Contains(t, res.Output, "would remove: "+hashes[1])
	assert.NotContains(t, res.Output, hashes[2])
	assert.Contains(t, res.Output, filepath.Join("_cacache", "content-v2", "sha512", "aa", "old"))
	for line := range strings.SplitSeq(res.Output, "\n") {
		if strings.HasPrefix(line, "would remove: ") {
			assert.NotContains(t, line, "node_modules", "dry-run must not list files inside a tree")
		}
	}
	for _, dir := range hashes {
		assertWholeOrGone(t, dir, 21)
	}
	assert.FileExists(t, filepath.Join(root, "_cacache", "content-v2", "sha512", "aa", "old"))
}

func TestTreeProvider_NpxRealRunLeavesNoPartialPackage(t *testing.T) {
	root, hashes := npxFixture(t)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "3K", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.NoDirExists(t, hashes[0])
	assert.NoDirExists(t, hashes[1])
	assertWholeOrGone(t, hashes[2], 21)
	assert.DirExists(t, hashes[2])
	assert.NoFileExists(t, filepath.Join(root, "_cacache", "content-v2", "sha512", "aa", "old"))
	assert.FileExists(t, filepath.Join(root, "_cacache", "content-v2", "sha512", "bb", "fresh"))
	assert.Equal(t, int64(2*21*100+100), res.BytesCleaned)
	assert.Equal(t, int64(2*21+1), res.FilesDeleted)
	left, err := filepath.Glob(filepath.Join(root, "_npx", ".bilgie-trash-*"))
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestTreeProvider_UnderLimitKeepsOldPackageAlls(t *testing.T) {
	root, hashes := npxFixture(t)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1G", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	for _, dir := range hashes {
		assertWholeOrGone(t, dir, 21)
		assert.DirExists(t, dir)
	}
	assert.NoFileExists(t, filepath.Join(root, "_cacache", "content-v2", "sha512", "aa", "old"))
}

func TestTreeProvider_FailedEvictionLeavesEntryIntact(t *testing.T) {
	root, hashes := npxFixture(t)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "3K", MaxAge: "30d"})
	p.evict = func(dir string) error {
		if dir == hashes[0] {
			return errors.New("busy")
		}
		return evictTree(dir)
	}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.Error(t, err)

	assert.Contains(t, res.Output, "error removing "+hashes[0])
	assertWholeOrGone(t, hashes[0], 21)
	assert.DirExists(t, hashes[0])
	assert.NoDirExists(t, hashes[1])
}

func TestEvictTree_RemovesWholeTreeAndLeavesNoTrash(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "entry")
	makePackage(t, dir, 3, day)

	require.NoError(t, evictTree(dir))

	assert.NoDirExists(t, dir)
	left, err := os.ReadDir(parent)
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestEvictTree_VanishedTreeReportsGone(t *testing.T) {
	parent := t.TempDir()

	err := evictTree(filepath.Join(parent, "absent"))

	require.ErrorIs(t, err, errTreeGone)
	left, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	assert.Empty(t, left)
}

func TestTreeProvider_SweepsLeftoverTrash(t *testing.T) {
	root, _ := npxFixture(t)
	trash := filepath.Join(root, "_npx", treeTrashPrefix+"1-2")
	makePackage(t, trash, 2, day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1G", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "would remove: "+trash)
	assert.DirExists(t, trash)

	_, err = p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NoDirExists(t, trash)
}

func TestTreeProvider_IgnoresDotEntries(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, "_npx", ".staging")
	makePackage(t, hidden, 5, 200*day)
	makePackage(t, filepath.Join(root, "_npx", "old"), 5, 100*day)
	makePackage(t, filepath.Join(root, "_npx", "new"), 5, 50*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1K", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assertWholeOrGone(t, hidden, 6)
	assert.DirExists(t, hidden)
	assert.NotContains(t, res.Output, ".staging")
	assert.NoDirExists(t, filepath.Join(root, "_npx", "old"))
	assert.DirExists(t, filepath.Join(root, "_npx", "new"))
}

func TestTreeProvider_TieBreaksByPath(t *testing.T) {
	root := t.TempDir()
	tieTime := time.Now().Add(-60 * day)
	for _, name := range []string{"c", "a", "d", "b"} {
		dir := filepath.Join(root, "_npx", name)
		makePackage(t, dir, 4, 60*day)
		require.NoError(t, filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(p, tieTime, tieTime)
		}))
	}
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1K", MaxAge: "30d"})

	var first string
	for range 3 {
		res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
		require.NoError(t, err)
		if first == "" {
			first = res.Output
		}
		assert.Equal(t, first, res.Output)
	}
	var order []string
	for line := range strings.SplitSeq(first, "\n") {
		if rest, ok := strings.CutPrefix(line, "would remove: "); ok {
			order = append(order, filepath.Base(strings.Fields(rest)[0]))
		}
	}
	assert.Equal(t, []string{"a", "b", "c"}, order)
}

func TestTreeProvider_KeepsTreesInUseAndRecent(t *testing.T) {
	root := t.TempDir()
	inUse := filepath.Join(root, "_npx", "inuse")
	recent := filepath.Join(root, "_npx", "recent")
	idle := filepath.Join(root, "_npx", "idle")
	makePackage(t, inUse, 5, 90*day)
	makePackage(t, recent, 5, time.Minute)
	makePackage(t, idle, 5, 80*day)
	makePackage(t, filepath.Join(root, "_npx", "newest"), 5, 70*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1K", MaxAge: "30d"})
	p.procLines = func(context.Context) ([]string, error) {
		return []string{"node " + filepath.Join(inUse, "node_modules", ".bin", "tool")}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.DirExists(t, inUse)
	assert.DirExists(t, recent)
	assert.NoDirExists(t, idle)
	assert.Contains(t, res.Output, "skip: "+inUse+" (in use by a running process)")
	assert.Contains(t, res.Output, "skip: "+recent)
}

func TestTreeProvider_UnlistableProcessesKeepEveryTree(t *testing.T) {
	root := t.TempDir()
	makePackage(t, filepath.Join(root, "_npx", "a"), 5, 90*day)
	makePackage(t, filepath.Join(root, "_npx", "b"), 5, 80*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})
	p.procLines = func(context.Context) ([]string, error) { return nil, errors.New("no ps") }

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.DirExists(t, filepath.Join(root, "_npx", "a"))
	assert.DirExists(t, filepath.Join(root, "_npx", "b"))
}

type cargoFixture struct {
	root       string
	oldCrate   string
	midCrate   string
	newCrate   string
	oldArchive string
	newArchive string
	indexFile  string
}

func makeCrate(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	writeAged(t, filepath.Join(dir, ".cargo-ok"), 10, age)
	writeAged(t, filepath.Join(dir, "Cargo.toml"), 200, age)
	writeAged(t, filepath.Join(dir, "src", "lib.rs"), 200, age)
	ageAll(t, dir, age)
}

func newCargoFixture(t *testing.T) cargoFixture {
	t.Helper()
	f := cargoFixture{root: t.TempDir()}
	src := filepath.Join(f.root, "src", "index.crates.io-abc")
	f.oldCrate = filepath.Join(src, "serde-1.0.1")
	f.midCrate = filepath.Join(src, "serde-1.0.2")
	f.newCrate = filepath.Join(src, "serde-1.0.3")
	makeCrate(t, f.oldCrate, 90*day)
	makeCrate(t, f.midCrate, 60*day)
	makeCrate(t, f.newCrate, 40*day)
	cacheDir := filepath.Join(f.root, "cache", "index.crates.io-abc")
	f.oldArchive = filepath.Join(cacheDir, "serde-1.0.1.crate")
	f.newArchive = filepath.Join(cacheDir, "serde-1.0.3.crate")
	writeAged(t, f.oldArchive, 300, 90*day)
	writeAged(t, f.newArchive, 300, day)
	f.indexFile = filepath.Join(f.root, "index", "index.crates.io-abc", ".cache", "se", "rd", "serde")
	writeAged(t, f.indexFile, 300, 200*day)
	return f
}

func TestTreeProvider_CargoKeepsCrateWithMarkerWhenUnderLimit(t *testing.T) {
	f := newCargoFixture(t)
	p := newTree(t, "cargo", config.Provider{Paths: []string{f.root}, MaxSize: "1G", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	for _, crate := range []string{f.oldCrate, f.midCrate, f.newCrate} {
		assertWholeOrGone(t, crate, 3)
		assert.FileExists(t, filepath.Join(crate, ".cargo-ok"))
		assert.FileExists(t, filepath.Join(crate, "src", "lib.rs"))
	}
	assert.NoFileExists(t, f.oldArchive)
	assert.FileExists(t, f.newArchive)
	assert.FileExists(t, f.indexFile)
}

func TestTreeProvider_CargoDryRunListsWholeCrates(t *testing.T) {
	f := newCargoFixture(t)
	p := newTree(t, "cargo", config.Provider{Paths: []string{f.root}, MaxSize: "700", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
	require.NoError(t, err)

	assert.Contains(t, res.Output, "would remove: "+f.oldCrate)
	assert.Contains(t, res.Output, "would delete: "+f.oldArchive)
	assert.NotContains(t, res.Output, ".cargo-ok")
	assert.NotContains(t, res.Output, "lib.rs")
	assert.NotContains(t, res.Output, "Cargo.toml")
	assert.DirExists(t, f.oldCrate)
	assert.FileExists(t, filepath.Join(f.oldCrate, ".cargo-ok"))
}

func TestTreeProvider_CargoRealRunNeverLeavesMarkerWithoutFiles(t *testing.T) {
	f := newCargoFixture(t)
	p := newTree(t, "cargo", config.Provider{Paths: []string{f.root}, MaxSize: "700", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.NoDirExists(t, f.oldCrate)
	assert.NoDirExists(t, f.midCrate)
	assertWholeOrGone(t, f.newCrate, 3)
	assert.FileExists(t, filepath.Join(f.newCrate, ".cargo-ok"))
	assert.FileExists(t, f.indexFile)
}

func TestTreeProvider_CargoFullModeTrimsBySizeOnly(t *testing.T) {
	f := newCargoFixture(t)
	p := newTree(t, "cargo", config.Provider{Paths: []string{f.root}, MaxSize: "1G", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)

	assert.FileExists(t, f.oldArchive)
	assert.DirExists(t, f.oldCrate)
}

func TestTreeProvider_CargoGitCheckoutsAndDbAreWholeEntries(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, "db", "repo-1")
	checkout := filepath.Join(root, "checkouts", "repo-1", "abc1234")
	newest := filepath.Join(root, "checkouts", "repo-2", "def5678")
	writeAged(t, filepath.Join(db, "HEAD"), 100, 90*day)
	ageAll(t, db, 90*day)
	makeCrate(t, checkout, 80*day)
	makeCrate(t, newest, 70*day)
	writeAged(t, filepath.Join(root, "db", "repo-2", "HEAD"), 100, 60*day)
	ageAll(t, filepath.Join(root, "db", "repo-2"), 60*day)
	p := newTree(t, "cargo", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.NoDirExists(t, db)
	assert.NoDirExists(t, checkout)
	assertWholeOrGone(t, newest, 3)
	assert.DirExists(t, newest)
}

func TestTreeProvider_GoModEvictsReadOnlyModuleDirsWhole(t *testing.T) {
	root := t.TempDir()
	oldMod := filepath.Join(root, "github.com", "acme", "lib@v1.0.0")
	midMod := filepath.Join(root, "golang.org", "x", "tools@v0.1.0")
	newMod := filepath.Join(root, "gopkg.in", "yaml.v3@v3.0.1")
	for mod, age := range map[string]time.Duration{oldMod: 90 * day, midMod: 60 * day, newMod: 40 * day} {
		writeAged(t, filepath.Join(mod, "go.mod"), 100, age)
		writeAged(t, filepath.Join(mod, "pkg", "a.go"), 100, age)
		ageAll(t, mod, age)
	}
	download := filepath.Join(root, "cache", "download", "github.com", "acme", "lib", "@v", "v1.0.0.zip")
	writeAged(t, download, 500, 200*day)
	if runtime.GOOS != "windows" {
		for _, mod := range []string{oldMod, midMod} {
			require.NoError(t, os.Chmod(filepath.Join(mod, "pkg"), 0o500))
			require.NoError(t, os.Chmod(mod, 0o500))
			t.Cleanup(func() {
				_ = os.Chmod(mod, 0o700)
				_ = os.Chmod(filepath.Join(mod, "pkg"), 0o700)
			})
		}
	}
	p := newTree(t, "go-mod", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d", CleanCmd: "go clean -modcache"})

	dry, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, dry.Output, "would remove: "+oldMod)
	assert.NotContains(t, dry.Output, "a.go")

	_, err = p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.NoDirExists(t, oldMod)
	assert.NoDirExists(t, midMod)
	assertWholeOrGone(t, newMod, 2)
	assert.DirExists(t, newMod)
	assert.FileExists(t, download)
}

func TestTreeProvider_GoModFullModeRunsCleanCommand(t *testing.T) {
	root := t.TempDir()
	writeAged(t, filepath.Join(root, "github.com", "a", "b@v1", "go.mod"), 10, 90*day)
	p := newTree(t, "go-mod", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d", CleanCmd: "go clean -modcache"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull, DryRun: true})
	require.NoError(t, err)

	assert.Equal(t, "would run: go clean -modcache", res.Output)
	assert.DirExists(t, filepath.Join(root, "github.com", "a", "b@v1"))
}

func TestTreeProvider_YarnPackageDirsAreWholeEntries(t *testing.T) {
	root := t.TempDir()
	for name, age := range map[string]time.Duration{"npm-a-1": 90 * day, "npm-b-1": 80 * day, "npm-c-1": 70 * day} {
		dir := filepath.Join(root, "v6", name)
		writeAged(t, filepath.Join(dir, "node_modules", name, "index.js"), 100, age)
		writeAged(t, filepath.Join(dir, ".yarn-metadata.json"), 100, age)
		ageAll(t, dir, age)
	}
	writeAged(t, filepath.Join(root, "v6", ".tmp", "partial"), 100, 200*day)
	p := newTree(t, "yarn", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.NoDirExists(t, filepath.Join(root, "v6", "npm-a-1"))
	assert.NoDirExists(t, filepath.Join(root, "v6", "npm-b-1"))
	assert.DirExists(t, filepath.Join(root, "v6", "npm-c-1"))
	assert.FileExists(t, filepath.Join(root, "v6", ".tmp", "partial"))
	assert.NotContains(t, res.Output, "index.js")
}

func TestTreeProvider_GradleEvictsTopLevelCacheDirsWhole(t *testing.T) {
	root := t.TempDir()
	for name, age := range map[string]time.Duration{"modules-2": 90 * day, "transforms-4": 80 * day, "jars-9": 70 * day} {
		writeAged(t, filepath.Join(root, name, "files", "x.bin"), 100, age)
		writeAged(t, filepath.Join(root, name, name+".lock"), 10, age)
		ageAll(t, filepath.Join(root, name), age)
	}
	p := newTree(t, "gradle", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.NoDirExists(t, filepath.Join(root, "modules-2"))
	assert.NoDirExists(t, filepath.Join(root, "transforms-4"))
	assertWholeOrGone(t, filepath.Join(root, "jars-9"), 2)
}

func TestTreeProvider_XcodeBundlesAreWholeEntries(t *testing.T) {
	root := t.TempDir()
	oldArchive := filepath.Join(root, "2025-01-01", "App.xcarchive")
	newArchive := filepath.Join(root, "2025-06-01", "App.xcarchive")
	for dir, age := range map[string]time.Duration{oldArchive: 90 * day, newArchive: 40 * day} {
		writeAged(t, filepath.Join(dir, "Products", "App"), 100, age)
		writeAged(t, filepath.Join(dir, "Info.plist"), 100, age)
		ageAll(t, dir, age)
	}
	p := newTree(t, "xcode-archives", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.NoDirExists(t, oldArchive)
	assertWholeOrGone(t, newArchive, 2)
	assert.DirExists(t, newArchive)
}

func TestTreeProvider_VerifyRunsAfterFileTrim(t *testing.T) {
	root, _ := npxFixture(t)
	installFakeTool(t, fakeVerifyTool, fakeToolSpec{Default: fakeReply{Stdout: "verified\n"}})
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1G", MaxAge: "30d"})
	p.spec.verify = []string{fakeVerifyTool, "cache", "verify"}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "ran: "+fakeVerifyTool+" cache verify")

	dry, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
	require.NoError(t, err)
	assert.NotContains(t, dry.Output, "ran:")
}

func TestTreeProvider_VerifyFailureIsReportedNotFatal(t *testing.T) {
	root, _ := npxFixture(t)
	installFakeTool(t, fakeVerifyTool, fakeToolSpec{Default: fakeReply{Stderr: "broken", Exit: 1}})
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1G", MaxAge: "30d"})
	p.spec.verify = []string{fakeVerifyTool, "cache", "verify"}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

	require.NoError(t, err)
	assert.Contains(t, res.Output, "note: "+fakeVerifyTool+" cache verify failed")
}

func TestTreeProvider_CargoSkipsWhilePackageCacheLockHeld(t *testing.T) {
	f := newCargoFixture(t)
	p := newTree(t, "cargo", config.Provider{Paths: []string{f.root}, MaxSize: "1", MaxAge: "30d"})
	lock := filepath.Join(filepath.Dir(f.root), ".package-cache")
	release, acquired, err := osshim.TryLock(lock)
	require.NoError(t, err)
	require.True(t, acquired)
	defer release()

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

	require.NoError(t, err)
	assert.Equal(t, "lock held: "+lock, res.SkipReason)
	assert.DirExists(t, f.oldCrate)
	assert.FileExists(t, f.oldArchive)
}

func TestTreeProvider_CargoIgnoresRunningCargoWithoutLock(t *testing.T) {
	f := newCargoFixture(t)
	p := newTree(t, "cargo", config.Provider{Paths: []string{f.root}, MaxSize: "1", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})

	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
}

func TestMatchProcess_GradleDaemon(t *testing.T) {
	line := "/usr/bin/java -cp gradle-launcher-8.5.jar org.gradle.launcher.daemon.bootstrap.GradleDaemon 8.5"
	assert.Equal(t, "gradle", matchProcess(line, busyProcesses["gradle"]))
	assert.Equal(t, "gradlew", matchProcess("./gradlew build", busyProcesses["gradle"]))
}

func TestTreeProvider_Available(t *testing.T) {
	root := t.TempDir()
	assert.True(t, newTree(t, "cargo", config.Provider{Paths: []string{root}, MaxSize: "1G", MaxAge: "30d"}).Available())
	missing := newTree(t, "npm", config.Provider{
		Paths: []string{root}, MaxSize: "1G", MaxAge: "30d", CleanCmd: "no-such-tool-for-bilgie clean",
	})
	assert.False(t, missing.Available())
}

func TestNewProvider_TreeProvidersForTreeShapedCaches(t *testing.T) {
	for _, goos := range []string{config.OSDarwin, config.OSLinux, config.OSWindows} {
		defaults := config.DefaultProvidersFor(config.Platform{OS: goos})
		for name := range treeSpecs {
			cfg, ok := defaults[name]
			if !ok {
				continue
			}
			t.Run(goos+"/"+name, func(t *testing.T) {
				p, err := NewProvider(name, cfg)
				require.NoError(t, err)
				assert.IsType(t, &TreeProvider{}, p)
			})
		}
	}
}

func TestDefaults_NpmAndCargoStayEnabled(t *testing.T) {
	defaults := config.DefaultProvidersFor(config.Platform{OS: config.OSLinux})
	assert.True(t, defaults["npm"].Enabled)
	assert.True(t, defaults["cargo"].Enabled)
}

func TestTreeProvider_GoModUncleanRootStillSkipsDownloadCache(t *testing.T) {
	root := t.TempDir()
	mod := filepath.Join(root, "github.com", "acme", "lib@v1.0.0")
	writeAged(t, filepath.Join(mod, "go.mod"), 100, 90*day)
	other := filepath.Join(root, "github.com", "acme", "lib@v1.1.0")
	writeAged(t, filepath.Join(other, "go.mod"), 100, 80*day)
	ageAll(t, mod, 90*day)
	ageAll(t, other, 80*day)
	download := filepath.Join(root, "cache", "download", "github.com", "acme", "lib", "@v", "v1.0.0.zip")
	writeAged(t, download, 500, 200*day)
	p := newTree(t, "go-mod", config.Provider{
		Paths: []string{root}, MaxSize: "1", MaxAge: "30d",
	})
	p.paths = []string{root + string(filepath.Separator) + string(filepath.Separator)}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.FileExists(t, download)
	assert.NoDirExists(t, mod, res.Output)
}

func TestTreeProvider_TreesStayWhileUnderMaxSizeEvenAboveBuffer(t *testing.T) {
	root := t.TempDir()
	makePackage(t, filepath.Join(root, "_npx", "a"), 4, 90*day)
	makePackage(t, filepath.Join(root, "_npx", "b"), 4, 80*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1100", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.DirExists(t, filepath.Join(root, "_npx", "a"))
}

func TestMatchProcess_GradleLaunchersAndDaemon(t *testing.T) {
	for line, want := range map[string]string{
		"java.exe -cp x.jar org.gradle.launcher.daemon.bootstrap.GradleDaemon 8.5": "gradle",
		"java -cp x.jar org.gradle.launcher.daemon.bootstrap.gradledaemon 8.5":     "gradle",
		"cmd /c C:/proj/gradlew.bat build":                                         "gradlew",
		"C:/gradle/bin/gradle.bat test":                                            "gradle",
	} {
		assert.Equal(t, want, matchProcess(line, busyProcesses["gradle"]), line)
	}
}

func TestTreeProvider_GoModSymlinkedRootStillEvicts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "mod")
	require.NoError(t, os.Symlink(target, link))
	oldMod := filepath.Join(target, "github.com", "a", "b@v1.0.0")
	newMod := filepath.Join(target, "github.com", "a", "b@v1.1.0")
	writeAged(t, filepath.Join(oldMod, "go.mod"), 100, 90*day)
	writeAged(t, filepath.Join(newMod, "go.mod"), 100, 80*day)
	ageAll(t, oldMod, 90*day)
	ageAll(t, newMod, 80*day)
	p := newTree(t, "go-mod", config.Provider{Paths: []string{link}, MaxSize: "1", MaxAge: "30d"})

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.NoDirExists(t, oldMod)
	assert.DirExists(t, newMod)
}

func TestTreeProvider_ImageOnlyProcessTableHoldsEveryTree(t *testing.T) {
	root := t.TempDir()
	for i, name := range []string{"a", "b", "c"} {
		makePackage(t, filepath.Join(root, "_npx", name), 4, time.Duration(90-10*i)*day)
	}
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})
	p.imageOnly = true
	p.procLines = func(context.Context) ([]string, error) { return []string{"node"}, nil }

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	for _, name := range []string{"a", "b", "c"} {
		assert.DirExists(t, filepath.Join(root, "_npx", name))
	}
	assert.Contains(t, res.Output, "node is running")

	p.procLines = func(context.Context) ([]string, error) { return []string{"explorer"}, nil }
	_, err = p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(root, "_npx", "a"))
}

func TestTreeProvider_ReportsEntriesAndSkips(t *testing.T) {
	root := t.TempDir()
	makePackage(t, filepath.Join(root, "_npx", "old"), 4, 90*day)
	makePackage(t, filepath.Join(root, "_npx", "fresh"), 4, time.Minute)
	makePackage(t, filepath.Join(root, "_npx", "newest"), 4, 50*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})

	dry, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
	require.NoError(t, err)

	require.Len(t, dry.Entries, 2)
	assert.Equal(t, filepath.Join(root, "_npx", "old"), dry.Entries[0].Path)
	assert.Equal(t, filepath.Join(root, "_npx", "newest"), dry.Entries[1].Path)
	assert.Equal(t, 1, dry.SkippedEntries)
	assert.DirExists(t, filepath.Join(root, "_npx", "old"))
}

func TestTreeProvider_StopsOnceRecovered(t *testing.T) {
	root := t.TempDir()
	for i, name := range []string{"a", "b", "c", "d"} {
		makePackage(t, filepath.Join(root, "_npx", name), 4, time.Duration(90-10*i)*day)
	}
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{
		Mode:      CleanModeSmart,
		Recovered: func(freed int64) bool { return freed > 0 },
	})
	require.NoError(t, err)

	assert.Len(t, res.Entries, 1)
	assert.NoDirExists(t, filepath.Join(root, "_npx", "a"))
	assert.DirExists(t, filepath.Join(root, "_npx", "b"))
}

func TestTreeProvider_VanishedTreeIsNotCountedAsFreed(t *testing.T) {
	root := t.TempDir()
	for i, name := range []string{"a", "b", "c"} {
		makePackage(t, filepath.Join(root, "_npx", name), 4, time.Duration(90-10*i)*day)
	}
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})
	gone := filepath.Join(root, "_npx", "a")
	p.evict = func(dir string) error {
		if dir == gone {
			require.NoError(t, os.RemoveAll(dir))
		}
		return evictTree(dir)
	}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	require.Len(t, res.Entries, 1)
	assert.Equal(t, filepath.Join(root, "_npx", "b"), res.Entries[0].Path)
	assert.Equal(t, res.Entries[0].Size, res.BytesCleaned)
	assert.Equal(t, int64(5), res.FilesDeleted)
	assert.Contains(t, res.Output, "skip: "+gone+" (already removed)")
	assert.Equal(t, 1, res.SkippedEntries)
}

func TestFoldPath_CollapsesRepeatedSlashes(t *testing.T) {
	assert.Equal(t, foldPath("/a/b/c"), foldPath("/a//b///c"))
	assert.Equal(t, foldPath("C:/a/b"), foldPath(`C:\a\\b`))
}

func TestTreeProvider_DoubleSlashSpellingHoldsTree(t *testing.T) {
	root := t.TempDir()
	inUse := filepath.Join(root, "_npx", "inuse")
	makePackage(t, inUse, 5, 90*day)
	makePackage(t, filepath.Join(root, "_npx", "newest"), 5, 70*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})
	spelled := strings.Replace(filepath.ToSlash(inUse), "/_npx/", "//_npx///", 1)
	p.procLines = func(context.Context) ([]string, error) {
		return []string{"node " + spelled + "/node_modules/.bin/tool"}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.DirExists(t, inUse)
	assert.Contains(t, res.Output, "skip: "+inUse+" (in use by a running process)")
}

func TestTreeProvider_UnreadableSubdirectoryHoldsTree(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "_npx", "locked")
	makePackage(t, locked, 5, 90*day)
	makePackage(t, filepath.Join(root, "_npx", "other"), 5, 80*day)
	makePackage(t, filepath.Join(root, "_npx", "newest"), 5, 70*day)
	hidden := filepath.Join(locked, "node_modules", "pkg", "lib")
	require.NoError(t, os.Chmod(hidden, 0o000))
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o700) })
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.DirExists(t, locked)
	assert.NoDirExists(t, filepath.Join(root, "_npx", "other"))
	assert.Contains(t, res.Output, "skip: "+locked+" (1 unreadable entries)")
}

func TestTreeProvider_OverLimitWithNothingEvictableSaysSo(t *testing.T) {
	root := t.TempDir()
	makePackage(t, filepath.Join(root, "_npx", "fresh"), 4, time.Minute)
	makePackage(t, filepath.Join(root, "_npx", "newest"), 4, 50*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})
	p.procLines = func(context.Context) ([]string, error) {
		return []string{"node " + filepath.Join(root, "_npx", "newest")}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(res.Output, "over limit"), res.Output)
	assert.Contains(t, res.Output, "nothing is evictable")
	assert.NotContains(t, res.Output, "already under limit")
	assert.Equal(t, "over limit but nothing is evictable", res.SkipReason)
	assert.Equal(t, 2, res.SkippedEntries)
}

func TestTreeProvider_UnreadableNoteComesBeforeSkipLines(t *testing.T) {
	root := t.TempDir()
	makePackage(t, filepath.Join(root, "_npx", "fresh"), 4, time.Minute)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1G", MaxAge: "30d"})
	units := []treeUnit{{path: "x", hold: "modified 1m ago", isTree: true}}

	out := p.noopOutput(units, 3)

	assert.Equal(t, "already under limit (3 unreadable)\nskip: x (modified 1m ago)", out)
}

func TestTreeProvider_SizeCountsOnlyWhatItMayDelete(t *testing.T) {
	root := t.TempDir()
	makePackage(t, filepath.Join(root, "_npx", "a"), 4, 90*day)
	writeAged(t, filepath.Join(root, "_cacache", "content-v2", "x"), 700, day)
	writeAged(t, filepath.Join(root, "_logs", "debug.log"), 5000, day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1G", MaxAge: "30d"})

	got, err := p.CurrentSize(context.Background())
	require.NoError(t, err)

	assert.Equal(t, int64(500+700), got)
}

func TestTreeProvider_YarnSkipsWhileYarnRuns(t *testing.T) {
	for _, cmdline := range []string{"node /usr/lib/yarn/lib/cli.js yarn install", "/opt/yarn/bin/yarn.js add x", "yarnpkg install"} {
		root := t.TempDir()
		dir := filepath.Join(root, "v6", "npm-pkg-1")
		makePackage(t, dir, 3, 90*day)
		makePackage(t, filepath.Join(root, "v6", "npm-zzz-1"), 3, 80*day)
		p := newTree(t, "yarn", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})
		require.NotNil(t, p.busy)
		p.busy.listProcesses = listOf(cmdline)

		res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

		require.NoError(t, err)
		assert.Contains(t, res.SkipReason, "yarn is running", cmdline)
		assert.DirExists(t, dir)
	}
}

func TestTreeProvider_FullCleanMeasuresWholePath(t *testing.T) {
	root := t.TempDir()
	writeAged(t, filepath.Join(root, "_logs", "debug.log"), 5000, day)
	installFakeTool(t, fakeVerifyTool, fakeToolSpec{Default: fakeReply{Remove: []string{filepath.Join(root, "_logs")}}})
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d", CleanCmd: fakeVerifyTool})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)

	assert.Equal(t, int64(5000), res.BytesCleaned)
}

func TestTreeProvider_DotDotSpellingHoldsTree(t *testing.T) {
	root := t.TempDir()
	makePackage(t, filepath.Join(root, "_npx", "h2"), 5, 90*day)
	inUse := filepath.Join(root, "_npx", "h3")
	makePackage(t, inUse, 5, 80*day)
	makePackage(t, filepath.Join(root, "_npx", "newest"), 5, 70*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})
	line := "node " + filepath.ToSlash(filepath.Join(root, "_npx", "h2")) + "/../h3/x.js"
	p.procLines = func(context.Context) ([]string, error) { return []string{line}, nil }

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.DirExists(t, inUse)
}

func TestFoldPath_KeepsPrefixGluedToPath(t *testing.T) {
	assert.Contains(t, foldPath("tool --x=/a/../../home/u/cache"), "/home/u/cache")
}

func TestFoldPathSpaced_ResolvesDotDotAcrossASpace(t *testing.T) {
	assert.Contains(t, foldPathSpaced("node /Users/John Smith/../Bob/cache/tree/x.js"), foldPath("/Users/Bob/cache/tree"))
	assert.Equal(t, foldPath("/a/c"), foldPathSpaced("/a/b c/../c"))
	assert.Equal(t, foldPath("/a/c"), foldPathSpaced("/a/b/d e/../../c"))
	assert.Equal(t, foldPath("/a/b"), foldPathSpaced("/a/b c/../b"))
}

func TestTreeProvider_DotDotAfterSpacedDirectoryHoldsTree(t *testing.T) {
	root := t.TempDir()
	makePackage(t, filepath.Join(root, "_npx", "h2"), 5, 90*day)
	inUse := filepath.Join(root, "_npx", "h3")
	makePackage(t, inUse, 5, 80*day)
	makePackage(t, filepath.Join(root, "_npx", "newest"), 5, 70*day)
	p := newTree(t, "npm", config.Provider{Paths: []string{root}, MaxSize: "1", MaxAge: "30d"})
	line := "node " + filepath.ToSlash(filepath.Join(root, "_npx", "John Smith")) + "/../h3/x.js"
	p.procLines = func(context.Context) ([]string, error) { return []string{line}, nil }

	_, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.DirExists(t, inUse)
}
