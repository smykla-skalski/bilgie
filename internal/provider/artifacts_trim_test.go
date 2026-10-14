package provider

import (
	"context"
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
	oldHash   = "aaaaaaaaaaaaaaaa"
	freshHash = "bbbbbbbbbbbbbbbb"
)

// buildUnit lays out one cargo unit under profile and ages every file of it.
func buildUnit(t *testing.T, profile, name, hash string, size int, age time.Duration) {
	t.Helper()
	unit := name + "-" + hash
	writeFile(t, filepath.Join(profile, ".fingerprint", unit, "lib-"+name), "fp")
	writeFile(t, filepath.Join(profile, "build", unit, "output"), "out")
	writeFile(t, filepath.Join(profile, "deps", "lib"+name+"-"+hash+".rlib"), strings.Repeat("r", size))
	writeFile(t, filepath.Join(profile, "deps", name+"-"+hash+".d"), "d")
	ageTree(t, filepath.Join(profile, ".fingerprint", unit), age)
	ageTree(t, filepath.Join(profile, "build", unit), age)
	when := time.Now().Add(-age)
	for _, f := range []string{"lib" + name + "-" + hash + ".rlib", name + "-" + hash + ".d"} {
		require.NoError(t, os.Chtimes(filepath.Join(profile, "deps", f), when, when))
	}
}

// activeRustProject has a recently edited source file, so only trimming applies.
func activeRustProject(t *testing.T, h *artifactHarness, name string) (target, debug string) {
	t.Helper()
	dir := h.path(name)
	rustProject(t, dir, 10, 90*day)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "target", "debug", "deps")))
	recent := time.Now().Add(-day)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "src", "main.rs"), recent, recent))
	target = filepath.Join(dir, "target")
	debug = filepath.Join(target, "debug")
	buildUnit(t, debug, "old", oldHash, 4000, 60*day)
	buildUnit(t, debug, "fresh", freshHash, 100, 2*day)
	writeFile(t, filepath.Join(debug, "incremental", "old-1", "s-1", "data"), "i")
	writeFile(t, filepath.Join(debug, "incremental", "fresh-1", "s-1", "data"), "i")
	ageTree(t, filepath.Join(debug, "incremental", "old-1"), 60*day)
	return target, debug
}

func TestProjectArtifacts_TrimRemovesStaleUnitsOfActiveProject(t *testing.T) {
	h := newArtifactHarness(t, nil)
	target, debug := activeRustProject(t, h, "api")

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoFileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
	assert.NoFileExists(t, filepath.Join(debug, "deps", "old-"+oldHash+".d"))
	assert.NoDirExists(t, filepath.Join(debug, ".fingerprint", "old-"+oldHash))
	assert.NoDirExists(t, filepath.Join(debug, "build", "old-"+oldHash))
	assert.NoDirExists(t, filepath.Join(debug, "incremental", "old-1"))
	assert.FileExists(t, filepath.Join(debug, "deps", "libfresh-"+freshHash+".rlib"))
	assert.DirExists(t, filepath.Join(debug, ".fingerprint", "fresh-"+freshHash))
	assert.DirExists(t, filepath.Join(debug, "build", "fresh-"+freshHash))
	assert.DirExists(t, filepath.Join(debug, "incremental", "fresh-1"))
	assert.DirExists(t, target)
	assert.Equal(t, []string{target}, h.entryPaths(res))
	assert.Greater(t, res.BytesCleaned, int64(4000))
	assert.Contains(t, res.Output, "trimmed: "+target)
}

func TestProjectArtifacts_TrimDryRunReportsBytesWithoutRemoving(t *testing.T) {
	h := newArtifactHarness(t, nil)
	target, debug := activeRustProject(t, h, "api")

	res := h.clean(CleanOptions{Mode: CleanModeFull, DryRun: true})

	assert.FileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
	assert.Equal(t, []string{target}, h.entryPaths(res))
	assert.Greater(t, res.BytesCleaned, int64(4000))
	assert.Contains(t, res.Output, "would trim: "+target)
}

func TestProjectArtifacts_TrimSkipsProfileWhileCargoHoldsLock(t *testing.T) {
	h := newArtifactHarness(t, nil)
	_, debug := activeRustProject(t, h, "api")
	release, acquired, err := osshim.TryLock(filepath.Join(debug, cargoLockName))
	require.NoError(t, err)
	require.True(t, acquired)
	defer release()

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.FileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
	assert.Empty(t, res.Entries)
	assert.Contains(t, res.Output, "cargo holds")
}

func TestProjectArtifacts_TrimSkipsProfileWhenLockProbeFails(t *testing.T) {
	h := newArtifactHarness(t, nil)
	_, debug := activeRustProject(t, h, "api")
	require.NoError(t, os.MkdirAll(filepath.Join(debug, cargoLockName, "x"), 0o750))
	require.NoError(t, os.Chmod(filepath.Join(debug, cargoLockName), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(debug, cargoLockName), 0o750) })

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.FileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
	assert.Empty(t, res.Entries)
	assert.Contains(t, res.Output, "cannot probe")
}

func TestProjectArtifacts_TrimRespectsConfiguredAge(t *testing.T) {
	h := newArtifactHarness(t, func(c *config.Provider) { c.TrimIdle = "90d" })
	_, debug := activeRustProject(t, h, "api")

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.FileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
	assert.Empty(t, res.Entries)
	assert.Equal(t, 1, res.SkippedEntries)
}

func TestProjectArtifacts_TrimHandlesTargetTriples(t *testing.T) {
	h := newArtifactHarness(t, nil)
	target, _ := activeRustProject(t, h, "api")
	release := filepath.Join(target, "aarch64-apple-darwin", "release")
	buildUnit(t, release, "old", oldHash, 10, 60*day)

	h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoFileExists(t, filepath.Join(release, "deps", "libold-"+oldHash+".rlib"))
}

func TestProjectArtifacts_IdleProjectStillRemovedWhole(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 4000, 60*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, h.path("api", "target"))
	assert.Contains(t, res.Output, "removed: ")
}

func TestProjectArtifacts_TrimLeavesNodeArtifactsAlone(t *testing.T) {
	h := newArtifactHarness(t, nil)
	nodeProject(t, h.path("web"), 100, 60*day)
	recent := time.Now().Add(-day)
	require.NoError(t, os.Chtimes(h.path("web", "index.js"), recent, recent))

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
	assert.DirExists(t, h.path("web", "node_modules"))
}

func TestNewProjectArtifactsProvider_RejectsBadTrimIdle(t *testing.T) {
	_, err := NewProjectArtifactsProvider("project-artifacts", config.Provider{
		Type: config.TypeProjectArtifacts, Paths: []string{t.TempDir()}, MaxSize: "1G", TrimIdle: "soon", Enabled: true,
	})
	require.ErrorContains(t, err, "trim_idle")
}

func TestUnitSuffix(t *testing.T) {
	tests := map[string]string{
		"serde-0123456789abcdef":      "0123456789abcdef",
		"my-crate-0123456789abcdef":   "0123456789abcdef",
		"serde":                       "",
		"serde-short":                 "",
		"serde-0123456789abcdefextra": "",
	}
	for name, want := range tests {
		assert.Equal(t, want, unitSuffix(name), name)
	}
}

func TestProjectArtifacts_TrimDropsOrphanDepsFilesByAge(t *testing.T) {
	h := newArtifactHarness(t, nil)
	_, debug := activeRustProject(t, h, "api")
	oldOrphan := filepath.Join(debug, "deps", "liborphan-cccccccccccccccc.rlib")
	newOrphan := filepath.Join(debug, "deps", "libnewer-dddddddddddddddd.rlib")
	writeFile(t, oldOrphan, "o")
	writeFile(t, newOrphan, "n")
	ancient := time.Now().Add(-60 * day)
	require.NoError(t, os.Chtimes(oldOrphan, ancient, ancient))
	writeFile(t, filepath.Join(debug, "deps", "README"), "no hash")

	h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoFileExists(t, oldOrphan)
	assert.FileExists(t, newOrphan)
	assert.FileExists(t, filepath.Join(debug, "deps", "README"))
}

func TestProjectArtifacts_TrimWorksWithoutDepsOrIncremental(t *testing.T) {
	h := newArtifactHarness(t, nil)
	_, debug := activeRustProject(t, h, "api")
	require.NoError(t, os.RemoveAll(filepath.Join(debug, "deps")))
	require.NoError(t, os.RemoveAll(filepath.Join(debug, "incremental")))

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, filepath.Join(debug, ".fingerprint", "old-"+oldHash))
	assert.Len(t, res.Entries, 1)
}

func TestProjectArtifacts_TrimStopsOnceSmartCleanIsSatisfied(t *testing.T) {
	h := newArtifactHarness(t, func(c *config.Provider) { c.MaxSize = "1G" })
	_, debug := activeRustProject(t, h, "api")

	res := h.clean(CleanOptions{Mode: CleanModeSmart})

	assert.FileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
	assert.Empty(t, res.Entries)
}

func TestProjectArtifacts_TrimIgnoresTargetWithoutProfiles(t *testing.T) {
	h := newArtifactHarness(t, nil)
	target, debug := activeRustProject(t, h, "api")
	require.NoError(t, os.RemoveAll(debug))
	writeFile(t, filepath.Join(target, "stray.txt"), "x")

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
	assert.Equal(t, 1, res.SkippedEntries)
}

func TestTrimTarget_StopsOnCancelledContext(t *testing.T) {
	h := newArtifactHarness(t, nil)
	target, debug := activeRustProject(t, h, "api")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := h.p.trimTarget(ctx, target, false)

	require.ErrorIs(t, err, context.Canceled)
	assert.FileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
}

func TestTrimProfile_FailsWithoutFingerprintDir(t *testing.T) {
	_, err := trimProfile(context.Background(), t.TempDir(), time.Now(), false)

	require.Error(t, err)
}

func TestRustProfiles_MissingTargetHasNone(t *testing.T) {
	assert.Empty(t, rustProfiles(filepath.Join(t.TempDir(), "absent")))
}

func TestRustProfiles_SkipsFilesAndPlainDirectories(t *testing.T) {
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "stray.txt"), "x")
	writeFile(t, filepath.Join(target, "plain", "nested", "file"), "x")
	writeFile(t, filepath.Join(target, "debug", ".fingerprint", "u-"+oldHash, "f"), "x")
	writeFile(t, filepath.Join(target, "triple", "release", ".fingerprint", "u-"+oldHash, "f"), "x")

	got := rustProfiles(target)

	assert.ElementsMatch(t, []string{filepath.Join(target, "debug"), filepath.Join(target, "triple", "release")}, got)
}

func TestLastUse_MissingPathIsZero(t *testing.T) {
	used, err := lastUse(context.Background(), filepath.Join(t.TempDir(), "absent"))

	require.NoError(t, err)
	assert.True(t, used.IsZero())
}

func TestMeasureTrimEntry_MissingPathIsNotOK(t *testing.T) {
	_, _, ok := measureTrimEntry(context.Background(), filepath.Join(t.TempDir(), "absent"))

	assert.False(t, ok)
}

func TestMeasureTrimEntry_SizesFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "f1"), "123")
	writeFile(t, filepath.Join(dir, "a", "f2"), "45")
	writeFile(t, filepath.Join(dir, "single"), "6789")

	bytes, files, ok := measureTrimEntry(context.Background(), filepath.Join(dir, "a"))
	assert.True(t, ok)
	assert.Equal(t, int64(5), bytes)
	assert.Equal(t, int64(2), files)

	bytes, files, ok = measureTrimEntry(context.Background(), filepath.Join(dir, "single"))
	assert.True(t, ok)
	assert.Equal(t, int64(4), bytes)
	assert.Equal(t, int64(1), files)
}

func TestLockProfile_NoLockFileMeansFree(t *testing.T) {
	release, skip := lockProfile(t.TempDir())

	assert.Empty(t, skip)
	release()
}
