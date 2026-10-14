package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/cache"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatResultWithErrors(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		want    string
		errors  []cache.AccessError
		deleted int64
	}{
		{
			name:    "no errors returns base",
			base:    "deleted 5 files",
			deleted: 5,
			errors:  nil,
			want:    "deleted 5 files",
		},
		{
			name:    "empty errors returns base",
			base:    "deleted 5 files",
			deleted: 5,
			errors:  []cache.AccessError{},
			want:    "deleted 5 files",
		},
		{
			name:    "permission denied only",
			base:    "deleted 5 files",
			deleted: 5,
			errors: []cache.AccessError{
				{Path: "/a", Reason: cache.ReasonPermissionDenied},
				{Path: "/b", Reason: cache.ReasonPermissionDenied},
			},
			want: "deleted 5 files (2 permission denied)",
		},
		{
			name:    "locked only",
			base:    "deleted 3 files",
			deleted: 3,
			errors: []cache.AccessError{
				{Path: "/a", Reason: cache.ReasonFileLocked},
			},
			want: "deleted 3 files (1 locked)",
		},
		{
			name:    "other errors only",
			base:    "deleted 2 files",
			deleted: 2,
			errors: []cache.AccessError{
				{Path: "/a", Reason: cache.ReasonUnknown},
				{Path: "/b", Reason: cache.ReasonNotFound},
			},
			want: "deleted 2 files (2 other errors)",
		},
		{
			name:    "mixed errors",
			base:    "deleted 10 files",
			deleted: 10,
			errors: []cache.AccessError{
				{Path: "/a", Reason: cache.ReasonPermissionDenied},
				{Path: "/b", Reason: cache.ReasonFileLocked},
				{Path: "/c", Reason: cache.ReasonUnknown},
			},
			want: "deleted 10 files (1 permission denied, 1 locked, 1 other errors)",
		},
		{
			name:    "empty base with errors",
			base:    "",
			deleted: 5,
			errors: []cache.AccessError{
				{Path: "/a", Reason: cache.ReasonPermissionDenied},
			},
			want: "deleted 5 files (1 permission denied)",
		},
		{
			name:    "preserves non-standard base",
			base:    "trimmed old files",
			deleted: 3,
			errors: []cache.AccessError{
				{Path: "/a", Reason: cache.ReasonPermissionDenied},
			},
			want: "trimmed old files (1 permission denied)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatResultWithErrors(tt.base, tt.deleted, tt.errors)
			if got != tt.want {
				t.Errorf("formatResultWithErrors() = %q, want %q", got, tt.want)
			}
		})
	}
}

func newOpenGuardedFileProvider(t *testing.T, paths []string, open map[string]bool) *FileProvider {
	t.Helper()

	on := true
	p, err := NewFileProvider("guarded", config.Provider{Paths: paths, MaxSize: "1B", MaxAge: "30d", SkipIfOpen: &on})
	require.NoError(t, err)
	p.openCheck = func(_ context.Context, dir string) (bool, error) { return open[dir], nil }
	return p
}

func TestFileProviderSkipsPathsWithOpenFiles(t *testing.T) {
	modes := map[string]CleanMode{"smart": CleanModeSmart, "full": CleanModeFull}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			busy := filepath.Join(root, "a-updater")
			idle := filepath.Join(root, "b-updater")
			for _, dir := range []string{busy, idle} {
				require.NoError(t, os.MkdirAll(dir, 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "f.bin"), make([]byte, 100), 0o600))
			}

			p := newOpenGuardedFileProvider(t, []string{busy, idle}, map[string]bool{busy: true})
			res, err := p.Clean(t.Context(), CleanOptions{Mode: mode})
			require.NoError(t, err)

			assert.True(t, exists(filepath.Join(busy, "f.bin")), "open path must stay")
			assert.False(t, exists(filepath.Join(idle, "f.bin")))
			assert.Contains(t, res.Output, "skip: "+busy+" (has open files)")
		})
	}
}

func TestFileProviderOpenCheckFailureCountsAsOpen(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.bin"), make([]byte, 100), 0o600))

	p := newOpenGuardedFileProvider(t, []string{dir}, nil)
	p.openCheck = func(context.Context, string) (bool, error) { return false, errors.New("lsof not found") }
	res, err := p.Clean(t.Context(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)

	assert.True(t, exists(filepath.Join(dir, "f.bin")))
	assert.Contains(t, res.Output, "open-file check failed")
}

func TestFileProviderMissingPathSkippedSilently(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	p := newOpenGuardedFileProvider(t, []string{missing}, nil)
	res, err := p.Clean(t.Context(), CleanOptions{Mode: CleanModeFull})
	require.NoError(t, err)
	assert.NotContains(t, res.Output, "skip")
}

func TestDirPatternSweepsSailE2EDirectories(t *testing.T) {
	root := t.TempDir()
	stale := makeDir(t, root, "dev.smykla.sail.e2e.1", 1000, 5*time.Hour)
	fresh := makeDir(t, root, "dev.smykla.sail.e2e.2", 1000, time.Minute)
	repo := makeDir(t, root, "dev.smykla.sail.e2e.3", 1000, 5*time.Hour)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o750))
	ageTree(t, repo, 5*time.Hour)
	other := makeDir(t, root, "dev.smykla.sail.app", 1000, 5*time.Hour)

	p := newTestDirProvider(t, root, func(c *config.Provider) {
		c.Paths = []string{filepath.Join(root, "dev.smykla.sail.e2e.*")}
	})
	_, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)

	assert.False(t, exists(stale))
	assert.True(t, exists(fresh), "recently modified")
	assert.True(t, exists(repo), "contains .git")
	assert.True(t, exists(other), "outside the glob")
}
