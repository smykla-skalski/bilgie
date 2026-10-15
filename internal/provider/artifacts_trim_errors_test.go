package provider

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireEnforcedPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
}

// lockDown makes path unusable until the test ends.
func lockDown(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Chmod(path, mode))
	t.Cleanup(func() { _ = os.Chmod(path, 0o750) })
}

func unitPaths(profile, hash string) []string {
	return []string{
		filepath.Join(profile, ".fingerprint", "old-"+hash),
		filepath.Join(profile, "build", "old-"+hash),
		filepath.Join(profile, "deps", "libold-"+hash+".rlib"),
	}
}

func TestProjectArtifacts_TrimProceedsWhenCargoLeftAFreeLockFile(t *testing.T) {
	h := newArtifactHarness(t, nil)
	_, debug := activeRustProject(t, h, "api")
	writeFile(t, filepath.Join(debug, cargoLockName), "")

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, filepath.Join(debug, ".fingerprint", "old-"+oldHash))
	assert.Len(t, res.Entries, 1)
	release, acquired, err := osshim.TryLock(filepath.Join(debug, cargoLockName))
	require.NoError(t, err)
	assert.True(t, acquired, "trimming must not leave the profile locked against cargo")
	release()
}

func TestProjectArtifacts_TrimRemovesUnitThatHasNoBuildDirectory(t *testing.T) {
	h := newArtifactHarness(t, nil)
	_, debug := activeRustProject(t, h, "api")
	require.NoError(t, os.RemoveAll(filepath.Join(debug, "build", "old-"+oldHash)))

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, filepath.Join(debug, ".fingerprint", "old-"+oldHash))
	assert.NoFileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
	assert.Len(t, res.Entries, 1)
	assert.Contains(t, res.Output, "trimmed: ")
}

func TestProjectArtifacts_TrimNeverRemovesEntriesThatAreNotUnits(t *testing.T) {
	h := newArtifactHarness(t, nil)
	_, debug := activeRustProject(t, h, "api")
	stray := filepath.Join(debug, ".fingerprint", "notes")
	writeFile(t, filepath.Join(stray, "todo"), "keep")
	ageTree(t, stray, 90*day)

	h.clean(CleanOptions{Mode: CleanModeFull})

	assert.FileExists(t, filepath.Join(stray, "todo"))
	assert.NoDirExists(t, filepath.Join(debug, ".fingerprint", "old-"+oldHash))
}

func TestTrimProfile_AbortsWithoutRemovingAnythingWhenAPartIsUnreadable(t *testing.T) {
	requireEnforcedPermissions(t)
	orphanHash := "cccccccccccccccc"
	tests := []struct {
		name string
		part func(profile string) string
	}{
		{"stale fingerprint", func(p string) string { return filepath.Join(p, ".fingerprint", "old-"+oldHash) }},
		{"deps listing", func(p string) string { return filepath.Join(p, "deps") }},
		{"orphan deps entry", func(p string) string { return filepath.Join(p, "deps", "orphan-"+orphanHash) }},
		{"incremental listing", func(p string) string { return filepath.Join(p, "incremental") }},
		{"incremental entry", func(p string) string { return filepath.Join(p, "incremental", "old-1") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newArtifactHarness(t, nil)
			_, debug := activeRustProject(t, h, "api")
			writeFile(t, filepath.Join(debug, "deps", "orphan-"+orphanHash, "x"), "x")
			ageTree(t, filepath.Join(debug, "deps", "orphan-"+orphanHash), 60*day)
			part := tt.part(debug)
			lockDown(t, part, 0o000)

			res, err := trimProfile(context.Background(), debug, time.Now().Add(-30*day), false)

			require.Error(t, err)
			assert.Zero(t, res.bytes)
			assert.Zero(t, res.files)
			require.NoError(t, os.Chmod(part, 0o750))
			for _, path := range unitPaths(debug, oldHash) {
				assert.True(t, exists(path), "%s must survive an aborted trim", path)
			}
			assert.DirExists(t, filepath.Join(debug, "incremental", "old-1"))
		})
	}
}

func TestTrimProfile_ReportsEntryItCannotRemove(t *testing.T) {
	requireEnforcedPermissions(t)
	h := newArtifactHarness(t, nil)
	_, debug := activeRustProject(t, h, "api")
	fingerprints := filepath.Join(debug, ".fingerprint")
	lockDown(t, fingerprints, 0o500)

	_, err := trimProfile(context.Background(), debug, time.Now().Add(-30*day), false)

	require.ErrorContains(t, err, "trim "+filepath.Join(fingerprints, "old-"+oldHash))
	assert.DirExists(t, filepath.Join(fingerprints, "fresh-"+freshHash))
}

func TestProjectArtifacts_TrimFailureKeepsEarlierProfileProgressAndReleasesLocks(t *testing.T) {
	requireEnforcedPermissions(t)
	h := newArtifactHarness(t, nil)
	target, debug := activeRustProject(t, h, "api")
	release := filepath.Join(target, "release")
	buildUnit(t, release, "old", oldHash, 10, 60*day)
	for _, profile := range []string{debug, release} {
		writeFile(t, filepath.Join(profile, cargoLockName), "")
	}
	lockDown(t, filepath.Join(release, ".fingerprint", "old-"+oldHash), 0o000)
	h.p.invalidate()

	res, err := h.p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})

	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(debug, "deps", "libold-"+oldHash+".rlib"))
	assert.Equal(t, []string{target}, h.entryPaths(res))
	assert.Positive(t, res.BytesCleaned)
	assert.Contains(t, res.Output, "trimmed: "+target)
	for _, profile := range []string{debug, release} {
		unlock, acquired, lockErr := osshim.TryLock(filepath.Join(profile, cargoLockName))
		require.NoError(t, lockErr)
		assert.True(t, acquired, "%s stays locked after a failed trim", profile)
		unlock()
	}
}

func TestProjectArtifacts_TrimFailureOfOneProjectDoesNotStopTheNext(t *testing.T) {
	requireEnforcedPermissions(t)
	h := newArtifactHarness(t, nil)
	_, brokenDebug := activeRustProject(t, h, "broken")
	_, goodDebug := activeRustProject(t, h, "good")
	lockDown(t, filepath.Join(brokenDebug, "deps"), 0o000)
	h.p.invalidate()

	res, err := h.p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})

	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(goodDebug, "deps", "libold-"+oldHash+".rlib"))
	assert.Len(t, res.Entries, 1)
	require.NoError(t, os.Chmod(filepath.Join(brokenDebug, "deps"), 0o750))
	assert.FileExists(t, filepath.Join(brokenDebug, "deps", "libold-"+oldHash+".rlib"))
}

func TestRustProfiles_SkipsDirectoriesItCannotList(t *testing.T) {
	requireEnforcedPermissions(t)
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "debug", ".fingerprint", "u-"+oldHash, "f"), "x")
	writeFile(t, filepath.Join(target, "sealed", "release", ".fingerprint", "u-"+oldHash, "f"), "x")
	lockDown(t, filepath.Join(target, "sealed"), 0o000)

	got := rustProfiles(target)

	assert.Equal(t, []string{filepath.Join(target, "debug")}, got)
}

func TestLockProfile_ProbeFailureSkipsProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports a missing path for a file used as a directory")
	}
	notADir := filepath.Join(t.TempDir(), "profile")
	writeFile(t, notADir, "x")

	release, skip := lockProfile(notADir)

	assert.Nil(t, release)
	assert.Contains(t, string(skip), "cannot probe")
}

func TestLastUse_StopsOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := lastUse(ctx, dir)

	require.ErrorIs(t, err, context.Canceled)
}

func TestLastUse_FailsOnUnreadableDirectory(t *testing.T) {
	requireEnforcedPermissions(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sealed", "f"), "x")
	lockDown(t, filepath.Join(dir, "sealed"), 0o000)

	_, err := lastUse(context.Background(), dir)

	require.Error(t, err)
}
