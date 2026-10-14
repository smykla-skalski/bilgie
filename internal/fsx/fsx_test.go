package fsx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadRegular(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o600))

	data, err := ReadRegular(file, 10)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	_, err = ReadRegular(file, 4)
	assert.ErrorIs(t, err, ErrTooLarge)

	_, err = ReadRegular(filepath.Join(dir, "missing"), 10)
	assert.ErrorIs(t, err, os.ErrNotExist)

	_, err = ReadRegular(dir, 10)
	assert.ErrorIs(t, err, ErrNotRegular)
}

func TestReadRegular_FollowsLinksToRegularFilesOnly(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	if err := os.Symlink(file, filepath.Join(dir, "to-file")); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	require.NoError(t, os.Symlink(dir, filepath.Join(dir, "to-dir")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "dangling")))

	data, err := ReadRegular(filepath.Join(dir, "to-file"), 10)
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
	_, err = ReadRegular(filepath.Join(dir, "to-dir"), 10)
	assert.ErrorIs(t, err, ErrNotRegular)
	_, err = ReadRegular(filepath.Join(dir, "dangling"), 10)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestCheckRegular(t *testing.T) {
	dir := t.TempDir()

	assert.NoError(t, CheckRegular(filepath.Join(dir, "missing")))
	assert.ErrorIs(t, CheckRegular(dir), ErrNotRegular)
}

func TestWriteFileNoLink_ReplacesContentAndCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")

	require.NoError(t, WriteFileNoLink(path, []byte("a long first line\n"), 0o600))
	require.NoError(t, WriteFileNoLink(path, []byte("short"), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "short", string(data))
}

func TestOpenAppendNoLink_AppendsAfterExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o600))

	f, err := OpenAppendNoLink(path, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("two\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", string(data))
}

func TestOpenRegular(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("abc"), 0o600))

	f, err := OpenRegular(file)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = OpenRegular(dir)
	require.ErrorIs(t, err, ErrNotRegular)
	_, err = OpenRegular(filepath.Join(dir, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
