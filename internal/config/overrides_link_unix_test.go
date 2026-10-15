//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteOverrides_KeepsTheModeOfAnExistingFile(t *testing.T) {
	loader, path := overridesLoader(t, "providers:\n  npm:\n    enabled: false\n")
	require.NoError(t, os.Chmod(path, 0o640))

	_, err := loader.WriteOverrides([]Override{{Name: "npm"}})

	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestWriteOverrides_FollowsASymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(t, os.WriteFile(target, []byte("# managed\nproviders:\n  npm:\n    enabled: false\n"), 0o600))
	link := filepath.Join(dir, "cfg", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
	require.NoError(t, os.Symlink(filepath.Join("..", "dotfiles", "config.yaml"), link))
	isolateHome(t)
	loader := NewLoader()
	loader.SetConfigPath(link)

	changed, err := loader.WriteOverrides([]Override{{Name: "npm"}, {Name: "pip"}})

	require.NoError(t, err)
	assert.Equal(t, []string{"npm", "pip"}, changed)
	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link must stay a link")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "# managed\nproviders:\n  npm:\n    enabled: true\n  pip:\n    enabled: true\n", string(data))
}

func TestWriteOverrides_CreatesTheTargetOfADanglingLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	link := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.Symlink(target, link))
	isolateHome(t)
	loader := NewLoader()
	loader.SetConfigPath(link)

	_, err := loader.WriteOverrides([]Override{{Name: "npm"}})

	require.NoError(t, err)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Contains(t, string(data), "npm:")
}

func TestWriteOverrides_ReportsAWriteFailureOnce(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	isolateHome(t)
	loader := NewLoader()
	loader.SetConfigPath(filepath.Join(dir, "sub", "config.yaml"))

	_, err := loader.WriteOverrides([]Override{{Name: "npm"}})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "write config: write config")
}

func TestWriteOverrides_RefusesALinkLoop(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	require.NoError(t, os.Symlink(b, a))
	require.NoError(t, os.Symlink(a, b))
	isolateHome(t)
	loader := NewLoader()
	loader.SetConfigPath(a)

	_, err := loader.WriteOverrides([]Override{{Name: "npm"}})

	require.ErrorContains(t, err, "symbolic links")
}

func TestWriteOverrides_WritesAFileInADirectoryItCannotAddTo(t *testing.T) {
	loader, path := overridesLoader(t, "providers:\n  npm:\n    enabled: false\n")
	dir := filepath.Dir(path)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if probe, err := os.CreateTemp(dir, "probe"); err == nil {
		_ = probe.Close()
		t.Skip("directory permissions are not enforced here")
	}

	changed, err := loader.WriteOverrides([]Override{{Name: "npm"}})

	require.NoError(t, err)
	assert.Equal(t, []string{"npm"}, changed)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "providers:\n  npm:\n    enabled: true\n", string(data))
}

func TestWriteOverrides_KeepsNoLockFileNextToTheConfig(t *testing.T) {
	loader, path := overridesLoader(t, "providers:\n  npm:\n    enabled: false\n")

	_, err := loader.WriteOverrides([]Override{{Name: "npm"}})

	require.NoError(t, err)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "config.yaml", entries[0].Name())
}
