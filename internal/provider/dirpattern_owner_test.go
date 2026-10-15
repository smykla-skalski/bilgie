package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ownerPID    = 4242
	ownerIdle   = 10 * time.Hour
	markerStart = int64(1_700_000_000)
)

func ownerMarkerText(pid int, start int64) string {
	return strconv.Itoa(pid) + " " + strconv.FormatInt(start, 10)
}

func writeOwnerMarker(t *testing.T, dir, text string) {
	t.Helper()
	writeText(t, filepath.Join(dir, ".sail-scratch-root"), text)
	ageTree(t, dir, ownerIdle)
}

func markedRoot(t *testing.T, root, name, text string) string {
	t.Helper()
	dir := makeDir(t, root, name, 10, ownerIdle)
	writeOwnerMarker(t, dir, text)
	return dir
}

func newOwnerProvider(t *testing.T, root string, markerOnly bool, owner func(pid int) (time.Time, bool, error)) *DirPatternProvider {
	t.Helper()

	p := newTestDirProvider(t, root, func(c *config.Provider) {
		c.Paths = []string{filepath.Join(root, "*")}
		c.OwnerMarker = markerOnly
	})
	p.ownerStart = func(_ context.Context, pid int) (time.Time, bool, error) { return owner(pid) }
	return p
}

func exitedOwner(int) (time.Time, bool, error) { return time.Time{}, false, nil }

func runningOwner(start int64) func(int) (time.Time, bool, error) {
	return func(int) (time.Time, bool, error) { return time.Unix(start, 0), true, nil }
}

func TestDirPatternOwnerMarkerSweepsAnyNameWhenOwnerExited(t *testing.T) {
	root := t.TempDir()
	dir := markedRoot(t, root, "reef-75.AbCd", ownerMarkerText(ownerPID, markerStart))
	unmarked := makeDir(t, root, "sail-looks-like-one", 10, ownerIdle)

	res, err := newOwnerProvider(t, root, true, exitedOwner).Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.False(t, exists(dir))
	assert.True(t, exists(unmarked), "no marker, no candidate")
	assert.NotContains(t, res.Output, unmarked, "unmarked directories are not even reported")
}

func TestDirPatternOwnerMarkerNeverRemovesRunningOwner(t *testing.T) {
	tests := []struct {
		name  string
		owner func(int) (time.Time, bool, error)
	}{
		{"same start", runningOwner(markerStart)},
		{"start within tolerance", runningOwner(markerStart + 3)},
		{"start unknown", func(int) (time.Time, bool, error) { return time.Time{}, true, nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := markedRoot(t, root, "agent-live", ownerMarkerText(ownerPID, markerStart))

			res, err := newOwnerProvider(t, root, true, tt.owner).Clean(t.Context(), CleanOptions{})
			require.NoError(t, err)

			assert.True(t, exists(dir))
			assert.Contains(t, res.Output, "owner pid 4242 is running")
		})
	}
}

func TestDirPatternOwnerMarkerReusedPidCountsAsExited(t *testing.T) {
	root := t.TempDir()
	dir := markedRoot(t, root, "agent-old", ownerMarkerText(ownerPID, markerStart))

	reused := runningOwner(markerStart + 3600)
	_, err := newOwnerProvider(t, root, true, reused).Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.False(t, exists(dir))
}

func TestDirPatternOwnerMarkerPassesPidToTheLookup(t *testing.T) {
	root := t.TempDir()
	markedRoot(t, root, "agent-pid", ownerMarkerText(777, markerStart))

	var asked []int
	p := newOwnerProvider(t, root, true, func(pid int) (time.Time, bool, error) {
		asked = append(asked, pid)
		return time.Time{}, false, nil
	})
	_, err := p.Clean(t.Context(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []int{777}, asked)
}

func TestDirPatternOwnerMarkerMalformedNeverMakesRootEligible(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"one field", "4242"},
		{"three fields", "4242 1700000000 x"},
		{"words", "owner started"},
		{"zero pid", "0 1700000000"},
		{"negative pid", "-5 1700000000"},
		{"pid too large", "99999999999 1700000000"},
		{"zero start", "4242 0"},
		{"negative start", "4242 -1"},
		{"too large", ownerMarkerText(ownerPID, markerStart) + string(make([]byte, 400))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := markedRoot(t, root, "agent-bad", tt.text)

			res, err := newOwnerProvider(t, root, true, exitedOwner).Clean(t.Context(), CleanOptions{})
			require.NoError(t, err)

			assert.True(t, exists(dir))
			assert.Contains(t, res.Output, "skip: "+dir)
			assert.Contains(t, res.Output, "owner marker")
		})
	}
}

func TestDirPatternOwnerMarkerMustBeARegularFile(t *testing.T) {
	root := t.TempDir()
	dir := makeDir(t, root, "agent-dir-marker", 10, ownerIdle)
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".sail-scratch-root"), 0o750))
	ageTree(t, dir, ownerIdle)

	res, err := newOwnerProvider(t, root, true, exitedOwner).Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.True(t, exists(dir))
	assert.Contains(t, res.Output, "is not a regular file")
}

func TestDirPatternOwnerLookupFailureBlocksRemoval(t *testing.T) {
	root := t.TempDir()
	dir := markedRoot(t, root, "agent-unknown", ownerMarkerText(ownerPID, markerStart))

	failing := func(int) (time.Time, bool, error) { return time.Time{}, false, errors.New("ps: not found") }
	res, err := newOwnerProvider(t, root, true, failing).Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.True(t, exists(dir))
	assert.Contains(t, res.Output, "owner pid 4242 cannot be checked")
}

func TestDirPatternExitedOwnerStillNeedsEveryOtherCheck(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		open   bool
		reason string
	}{
		{"recent write", func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "fresh"), []byte("x"), 0o600))
		}, false, "min_idle"},
		{"open files", func(*testing.T, string) {}, true, "has open files"},
		{"git data outside", func(t *testing.T, dir string) {
			t.Helper()
			writeText(t, filepath.Join(dir, "repo", ".git"), "gitdir: "+t.TempDir()+"\n")
			ageTree(t, dir, ownerIdle)
		}, false, "git data outside the root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := markedRoot(t, root, "agent-checks", ownerMarkerText(ownerPID, markerStart))
			tt.setup(t, dir)

			p := newOwnerProvider(t, root, true, exitedOwner)
			p.openMany = perDir(func(context.Context, string) (bool, error) { return tt.open, nil })
			res, err := p.Clean(t.Context(), CleanOptions{})
			require.NoError(t, err)

			assert.True(t, exists(dir))
			assert.Contains(t, res.Output, tt.reason)
		})
	}
}

func TestDirPatternOwnerMarkerOnlyProviderSizesMarkedRootsOnly(t *testing.T) {
	root := t.TempDir()
	markedRoot(t, root, "agent-a", ownerMarkerText(ownerPID, markerStart))
	makeDir(t, root, "plain", 5000, ownerIdle)

	p := newOwnerProvider(t, root, true, exitedOwner)
	size, err := p.CurrentSize(t.Context())
	require.NoError(t, err)
	assert.Less(t, size, int64(1000))
	assert.Len(t, p.Paths(), 1)
}

func TestDirPatternNameProviderHonoursLiveOwnerMarker(t *testing.T) {
	root := t.TempDir()
	live := markedRoot(t, root, "sail-runtime-live", ownerMarkerText(ownerPID, markerStart))
	gone := markedRoot(t, root, "sail-runtime-gone", ownerMarkerText(ownerPID+1, markerStart))
	plain := makeDir(t, root, "sail-plain", 10, ownerIdle)

	p := newTestDirProvider(t, root, nil)
	p.ownerStart = func(_ context.Context, pid int) (time.Time, bool, error) {
		return time.Unix(markerStart, 0), pid == ownerPID, nil
	}
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.True(t, exists(live))
	assert.False(t, exists(gone))
	assert.False(t, exists(plain))
	assert.Contains(t, res.Output, "skip: "+live)
}

func TestDirPatternOwnerMarkerRecheckedBeforeRemoval(t *testing.T) {
	root := t.TempDir()
	dir := markedRoot(t, root, "agent-race", ownerMarkerText(ownerPID, markerStart))

	calls := 0
	p := newOwnerProvider(t, root, true, func(int) (time.Time, bool, error) {
		calls++
		return time.Unix(markerStart, 0), calls > 1, nil
	})
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.True(t, exists(dir))
	assert.Contains(t, res.Output, "owner pid 4242 is running")
	assert.GreaterOrEqual(t, calls, 2)
}
