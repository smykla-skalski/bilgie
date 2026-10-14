package auto

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsideGitCheckout_SymlinkSpellingOfHomeStopsAtHome(t *testing.T) {
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home := filepath.Join(resolved, "home")
	mkdirFile(t, filepath.Join(home, ".git", "HEAD"), 1)
	mkdirFile(t, filepath.Join(home, "plain", "a.bin"), 1)
	mkdirFile(t, filepath.Join(home, "repo", ".git", "HEAD"), 1)
	alias := filepath.Join(resolved, "alias")
	require.NoError(t, os.Symlink(home, alias))

	assert.False(t, insideGitCheckout(filepath.Join(alias, "plain"), home))
	assert.False(t, insideGitCheckout(filepath.Join(home, "plain"), alias))
	assert.True(t, insideGitCheckout(filepath.Join(alias, "repo"), home))
	assert.Equal(t, verdictClear, checkProtected(t.Context(), filepath.Join(alias, "plain"), home, nil, true).kind)
}

func TestCheckProtected_ProviderPathAboveCheckoutIsProtected(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "dev", "repo", ".git", "HEAD"), 1)
	mkdirFile(t, filepath.Join(home, "dev", "repo", "src", "a.go"), 1)
	mkdirFile(t, filepath.Join(home, "clean", "a.bin"), 1)

	assert.Equal(t, verdictProtected, checkProtected(t.Context(), filepath.Join(home, "dev"), home, nil, true).kind)
	assert.Equal(t, verdictClear, checkProtected(t.Context(), filepath.Join(home, "clean"), home, nil, true).kind)
}

func TestScanProtected_ListsCheckoutsOutsideWorktreesDirs(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "code", "api", ".git", "HEAD"), 10)
	mkdirFile(t, filepath.Join(home, "code", "api", "node_modules", "x", ".git", "HEAD"), 10)
	mkdirFile(t, filepath.Join(home, "code", "org", "web", ".git", "HEAD"), 10)
	mkdirFile(t, filepath.Join(home, "code", "notes", "a.md"), 10)
	mkdirFile(t, filepath.Join(home, "Library", "x", ".git", "HEAD"), 10)
	mkdirFile(t, filepath.Join(home, ".git", "HEAD"), 10)

	report := ScanProtected(t.Context(), &config.Config{}, home, time.Minute)

	var got []string
	for _, e := range report.Entries {
		got = append(got, e.Path)
	}
	assert.ElementsMatch(t, []string{filepath.Join(home, "code", "api"), filepath.Join(home, "code", "org", "web")}, got)
}

func TestScanProtected_ListsWorktreesDirThreeLevelsDown(t *testing.T) {
	home := sandboxHome(t)
	mkdirFile(t, filepath.Join(home, "a", "b", "worktrees", "r", "f.bin"), 10)

	report := ScanProtected(t.Context(), &config.Config{}, home, time.Minute)

	require.Len(t, report.Entries, 1)
	assert.Equal(t, filepath.Join(home, "a", "b", "worktrees"), report.Entries[0].Path)
}

func TestScanProtected_ListsDockerDesktopVolumes(t *testing.T) {
	home := sandboxHome(t)
	vms := filepath.Join(home, "Library", "Containers", "com.docker.docker", "Data", "vms")
	mkdirFile(t, filepath.Join(vms, "0", "data", "Docker.raw"), 100000)
	mkdirFile(t, filepath.Join(home, "Library", "Containers", "com.docker.docker", "Data", "log", "a.log"), 100000)

	report := ScanProtected(t.Context(), &config.Config{}, home, time.Minute)

	require.Len(t, report.Entries, 1)
	assert.Equal(t, vms, report.Entries[0].Path)
}
