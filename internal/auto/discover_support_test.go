package auto

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPathProtection_SeesACheckoutBeyondTheSweepScan(t *testing.T) {
	home := sandboxHome(t)
	dir := filepath.Join(home, "stash", "deep")
	mkdirFile(t, filepath.Join(dir, "a", "b", "c", "d", "e", "f.bin"), 1)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a", "b", "c", "d", "e", ".git"), 0o750))
	shallow := filepath.Join(home, "stash", "shallow")
	mkdirFile(t, filepath.Join(shallow, "a", "b", "f.bin"), 1)

	sweep, err := pathReason(t.Context(), dir, true, true, home, nil)
	require.NoError(t, err)
	assert.Empty(t, sweep, "the sweep's own scan stops at three levels")

	reason, err := PathProtection(t.Context(), dir, home, nil)
	require.NoError(t, err)
	assert.Contains(t, reason, "git checkout at ")
	assert.Contains(t, reason, filepath.Join("a", "b", "c", "d", "e"))

	reason, err = PathProtection(t.Context(), shallow, home, nil)
	require.NoError(t, err)
	assert.Empty(t, reason)
}

func TestPathProtection_SeesAProtectedNameBelowThePath(t *testing.T) {
	home := sandboxHome(t)
	for _, name := range []string{"Downloads", "worktrees", "opencode"} {
		dir := filepath.Join(home, "stash", name+"-holder")
		mkdirFile(t, filepath.Join(dir, "x", "y", name, "f.bin"), 1)

		reason, err := PathProtection(t.Context(), dir, home, nil)

		require.NoError(t, err, name)
		assert.Contains(t, reason, filepath.Join("x", "y", name), name)
	}
}

func TestPathProtection_RunningOutOfBudgetDoesNotBlock(t *testing.T) {
	home := sandboxHome(t)
	dir := filepath.Join(home, "stash", "wide")
	for _, name := range []string{"a", "b", "c"} {
		mkdirFile(t, filepath.Join(dir, name, "f.bin"), 1)
	}
	old := deepLimits
	deepLimits.dirs = 1
	t.Cleanup(func() { deepLimits = old })

	reason, err := PathProtection(t.Context(), dir, home, nil)

	require.NoError(t, err)
	assert.Empty(t, reason)
}

func TestPathProtection_ACancelledContextIsAnError(t *testing.T) {
	home := sandboxHome(t)
	dir := filepath.Join(home, "stash", "plain")
	mkdirFile(t, filepath.Join(dir, "a", "f.bin"), 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := PathProtection(ctx, dir, home, nil)

	require.ErrorIs(t, err, context.Canceled)
}

func TestDeepReason_PermissionErrorsSkipOnlySubdirectories(t *testing.T) {
	denied := &fs.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}
	broken := errors.New("input/output error")
	tests := []struct {
		name  string
		fault func(rel string) (dirReader, error)
		want  string
	}{
		{
			name:  "subdir open denied",
			fault: func(rel string) (dirReader, error) { return nil, onlyAt(rel, "a", denied) },
			want:  "git checkout at ",
		},
		{
			name:  "subdir read denied after entries",
			fault: func(rel string) (dirReader, error) { return readFault(rel, "a", denied), nil },
			want:  "git checkout at ",
		},
		{
			name:  "subdir open fails otherwise",
			fault: func(rel string) (dirReader, error) { return nil, onlyAt(rel, "a", broken) },
		},
		{
			name:  "root open denied",
			fault: func(rel string) (dirReader, error) { return nil, onlyAt(rel, ".", denied) },
		},
		{
			name:  "root read denied after entries",
			fault: func(rel string) (dirReader, error) { return rootReadFault(rel, denied), nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
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

			reason, err := deepReason(t.Context(), root, deepLimits)

			require.NoError(t, err)
			if tt.want == "" {
				assert.Empty(t, reason)
				return
			}
			assert.Contains(t, reason, tt.want+filepath.Join(root, "b"))
		})
	}
}

func rootReadFault(rel string, err error) dirReader {
	if rel != "." {
		return nil
	}
	entries := []os.DirEntry{fakeEntry{name: "a", dir: true}, fakeEntry{name: "b", dir: true}}
	return &faultyDir{entries: entries, err: err}
}
