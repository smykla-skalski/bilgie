package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gitTestIdle = 10 * time.Hour

func writeText(t *testing.T, path, text string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
}

// linkWorktree makes gitDir a repository with one linked worktree whose
// .git file lives at worktreeRoot.
func linkWorktree(t *testing.T, gitDir, worktreeRoot string) {
	t.Helper()
	writeText(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
	writeText(t, filepath.Join(gitDir, "worktrees", "wt", "gitdir"), filepath.Join(worktreeRoot, ".git")+"\n")
	writeText(t, filepath.Join(gitDir, "worktrees", "wt", "commondir"), "../..\n")
}

// scratchRoot builds a sweep match holding a repository and a linked
// worktree of it, all inside the match.
func scratchRoot(t *testing.T, root, name string) string {
	t.Helper()
	dir := makeDir(t, root, name, 10, gitTestIdle)
	repo := filepath.Join(dir, "repo")
	worktree := filepath.Join(dir, "wt")
	linkWorktree(t, filepath.Join(repo, ".git"), worktree)
	writeText(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt")+"\n")
	writeText(t, filepath.Join(dir, "other", ".git", "HEAD"), "ref: refs/heads/main\n")
	ageTree(t, dir, gitTestIdle)
	return dir
}

func TestDirPatternRemovesRootWhoseGitDataStaysInside(t *testing.T) {
	root := t.TempDir()
	dir := scratchRoot(t, root, "sail-inside")
	p := newTestDirProvider(t, root, nil)

	dry, err := p.Clean(t.Context(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.True(t, exists(dir))
	assert.Contains(t, dry.Output, "would remove: "+dir)
	assert.Contains(t, dry.Output, "allowed despite 3 .git entries with all git data inside")

	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.False(t, exists(dir))
	assert.Contains(t, res.Output, "removed: "+dir)
	assert.Positive(t, res.BytesCleaned)
}

func TestDirPatternKeepsRootWhoseGitFilePointsOutside(t *testing.T) {
	tests := []struct {
		name    string
		pointer func(t *testing.T, from, outside string) string
	}{
		{"absolute", func(_ *testing.T, _, outside string) string { return "gitdir: " + outside + "\n" }},
		{"relative", func(t *testing.T, from, outside string) string {
			t.Helper()
			rel, err := filepath.Rel(from, outside)
			require.NoError(t, err)
			return "gitdir: " + filepath.ToSlash(rel) + "\n"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "scratch")
			outside := filepath.Join(parent, "main", ".git", "worktrees", "wt")
			require.NoError(t, os.MkdirAll(outside, 0o750))
			require.NoError(t, os.MkdirAll(root, 0o750))

			dir := scratchRoot(t, root, "sail-escape")
			writeText(t, filepath.Join(dir, "linked", ".git"), tt.pointer(t, filepath.Join(dir, "linked"), outside))
			ageTree(t, dir, gitTestIdle)

			res, err := newTestDirProvider(t, root, nil).Clean(t.Context(), CleanOptions{})
			require.NoError(t, err)
			assert.True(t, exists(dir))
			assert.Contains(t, res.Output, "skip: "+dir)
			resolvedOutside, evalErr := filepath.EvalSymlinks(outside)
			require.NoError(t, evalErr)
			assert.Contains(t, res.Output, "points to "+resolvedOutside)
		})
	}
}

func TestDirPatternKeepsRootWithWorktreeOutside(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "scratch")
	require.NoError(t, os.MkdirAll(root, 0o750))
	worktree := filepath.Join(parent, "elsewhere", "wt")
	require.NoError(t, os.MkdirAll(worktree, 0o750))

	dir := scratchRoot(t, root, "sail-worktree")
	linkWorktree(t, filepath.Join(dir, "repo2", ".git"), worktree)
	ageTree(t, dir, gitTestIdle)

	res, err := newTestDirProvider(t, root, nil).Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.True(t, exists(dir))
	assert.Contains(t, res.Output, "linked worktree outside the root")
	resolvedWorktree, evalErr := filepath.EvalSymlinks(worktree)
	require.NoError(t, evalErr)
	assert.Contains(t, res.Output, filepath.Join(resolvedWorktree, ".git"))
}

func TestDirPatternKeepsRootWithUnresolvableGit(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		reason string
	}{
		{"malformed file", func(t *testing.T, dir string) {
			t.Helper()
			writeText(t, filepath.Join(dir, "x", ".git"), "nonsense\n")
		}, "malformed"},
		{"empty pointer", func(t *testing.T, dir string) {
			t.Helper()
			writeText(t, filepath.Join(dir, "x", ".git"), "gitdir:\n")
		}, "empty gitdir"},
		{"empty file", func(t *testing.T, dir string) {
			t.Helper()
			writeText(t, filepath.Join(dir, "x", ".git"), "")
		}, "malformed"},
		{"worktree entry without pointer", func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "x", ".git", "worktrees", "broken"), 0o750))
		}, "cannot verify linked worktree broken"},
		{"commondir outside", func(t *testing.T, dir string) {
			t.Helper()
			gitdir := filepath.Join(dir, "x", "gd")
			writeText(t, filepath.Join(dir, "x", ".git"), "gitdir: "+gitdir+"\n")
			writeText(t, filepath.Join(gitdir, "commondir"), t.TempDir()+"\n")
		}, "shares"},
		{"symlinked marker", func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "x"), 0o750))
			require.NoError(t, os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "x", ".git")))
		}, "not a directory or file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := makeDir(t, root, "sail-bad", 10, gitTestIdle)
			tt.setup(t, dir)
			ageTree(t, dir, gitTestIdle)

			p := newTestDirProvider(t, root, nil)
			// Symlinks keep their own mtime, so look at the tree from later.
			p.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
			res, err := p.Clean(t.Context(), CleanOptions{})
			require.NoError(t, err)
			assert.True(t, exists(dir))
			assert.Contains(t, res.Output, tt.reason)
		})
	}
}

func TestDirPatternGitFileInsideRootStillNeedsIdle(t *testing.T) {
	root := t.TempDir()
	dir := scratchRoot(t, root, "sail-fresh")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "repo", "new.txt"), []byte("x"), 0o600))

	p := newTestDirProvider(t, root, nil)
	p.openMany = perDir(func(context.Context, string) (bool, error) { return true, nil })
	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.True(t, exists(dir))
	assert.Contains(t, res.Output, "min_idle")
}

func TestDirPatternChecksGitOnlyWhenGuardIsOn(t *testing.T) {
	p := newTestDirProvider(t, t.TempDir(), nil)
	assert.True(t, p.ChecksGit())

	off := false
	p = newTestDirProvider(t, t.TempDir(), func(c *config.Provider) { c.SkipIfGitWorktree = &off })
	assert.False(t, p.ChecksGit())
}
