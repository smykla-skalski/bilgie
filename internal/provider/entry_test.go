package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeEntry(t *testing.T, root, name string, bytes int, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "f"), make([]byte, bytes), 0o600))
	when := time.Now().Add(-age)
	// Entry age is the newest file mtime, so age every node, not just dir.
	for _, p := range []string{filepath.Join(dir, "sub", "f"), filepath.Join(dir, "sub"), dir} {
		require.NoError(t, os.Chtimes(p, when, when))
	}
	return dir
}

func newEntryProvider(t *testing.T, root, maxSize, maxAge string) *EntryProvider {
	t.Helper()
	p, err := NewEntryProvider("test", config.Provider{Paths: []string{root}, MaxSize: maxSize, MaxAge: maxAge, Enabled: true})
	require.NoError(t, err)
	return p
}

func TestEntryProvider_RemovesWholeOldestEntries(t *testing.T) {
	root := t.TempDir()
	old := makeEntry(t, root, "old", 2048, 48*time.Hour)
	mid := makeEntry(t, root, "mid", 2048, 24*time.Hour)
	fresh := makeEntry(t, root, "fresh", 2048, time.Hour)

	res, err := newEntryProvider(t, root, "4K", "").Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)

	assert.NoDirExists(t, old)
	assert.DirExists(t, mid)
	assert.DirExists(t, fresh)
	assert.FileExists(t, filepath.Join(mid, "sub", "f"))
	assert.Equal(t, int64(2048), res.BytesCleaned)
}

func TestEntryProvider_UnderLimitKeepsAll(t *testing.T) {
	root := t.TempDir()
	e := makeEntry(t, root, "a", 10, time.Hour)

	res, err := newEntryProvider(t, root, "1G", "").Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.DirExists(t, e)
	assert.Equal(t, "already under limit", res.Output)
}

func TestEntryProvider_DryRunKeepsFiles(t *testing.T) {
	root := t.TempDir()
	e := makeEntry(t, root, "a", 2048, 2*time.Hour)
	makeEntry(t, root, "b", 10, time.Hour)

	res, err := newEntryProvider(t, root, "1K", "").Clean(context.Background(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.DirExists(t, e)
	assert.Contains(t, res.Output, "would remove")
}

func TestEntryProvider_SmartIgnoresMaxAge(t *testing.T) {
	root := t.TempDir()
	old := makeEntry(t, root, "old", 10, 100*24*time.Hour)

	_, err := newEntryProvider(t, root, "1G", "30d").Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.DirExists(t, old)
}

func TestEntryProvider_AgesByNewestFile(t *testing.T) {
	root := t.TempDir()
	// Old directory mtime, but a file written inside recently.
	busy := makeEntry(t, root, "busy", 2048, 100*24*time.Hour)
	file := filepath.Join(busy, "sub", "f")
	require.NoError(t, os.Chtimes(file, time.Now(), time.Now()))
	idle := makeEntry(t, root, "idle", 2048, 24*time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(idle, "sub", "f"), time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour)))

	_, err := newEntryProvider(t, root, "2K", "").Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.DirExists(t, busy)
	assert.NoDirExists(t, idle)
}

func TestEntryProvider_MissingPath(t *testing.T) {
	res, err := newEntryProvider(t, filepath.Join(t.TempDir(), "nope"), "1K", "").Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), res.BytesCleaned)
}

func TestEntryProvider_KeepsNewestEntryOverLimit(t *testing.T) {
	root := t.TempDir()
	only := makeEntry(t, root, "big", 4096, time.Hour)

	_, err := newEntryProvider(t, root, "1K", "").Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.DirExists(t, only)
}

func TestEntryProvider_RemovalFailureIsAnError(t *testing.T) {
	root := t.TempDir()
	makeEntry(t, root, "old", 2048, 48*time.Hour)
	makeEntry(t, root, "new", 10, time.Hour)
	require.NoError(t, os.Chmod(root, 0o500))
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	res, err := newEntryProvider(t, root, "1K", "").Clean(context.Background(), CleanOptions{})
	if err == nil {
		t.Skip("removal succeeded despite read-only parent (running as root?)")
	}
	assert.Contains(t, res.Output, "error removing")
	// RemoveAll emptied the entry before failing on the entry itself.
	assert.Equal(t, int64(2048), res.BytesCleaned)
	assert.Equal(t, res.BytesCleaned, entriesTotal(res), "partial removal must stay listed")
}

func TestEntryProvider_IgnoresBookkeepingEntries(t *testing.T) {
	root := t.TempDir()
	old := makeEntry(t, root, "old", 2048, 48*time.Hour)
	newest := makeEntry(t, root, "model", 10, time.Hour)
	lock := makeEntry(t, root, ".locks", 10, time.Minute)

	_, err := newEntryProvider(t, root, "1K", "").Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.NoDirExists(t, old)
	assert.DirExists(t, newest)
	assert.DirExists(t, lock)
}

func TestEntryProvider_SkipPrefixNeverEvicted(t *testing.T) {
	root := t.TempDir()
	profile := makeEntry(t, root, "chrome-profile-abc", 4096, 90*24*time.Hour)
	old := makeEntry(t, root, "old", 2048, 48*time.Hour)
	newest := makeEntry(t, root, "new", 10, time.Hour)

	p, err := NewEntryProvider("test", config.Provider{
		Paths: []string{root}, MaxSize: "1K", Enabled: true,
		SkipPrefixes: []string{"chrome-profile-"},
	})
	require.NoError(t, err)
	_, err = p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)

	assert.DirExists(t, profile)
	assert.NoDirExists(t, old)
	assert.DirExists(t, newest)
}

func TestEntryProvider_DefaultChromeDevtoolsKeepsProfiles(t *testing.T) {
	cfg := config.DefaultConfig()
	def := cfg.Providers["chrome-devtools-mcp"]
	assert.False(t, def.Enabled)
	assert.NotContains(t, cfg.AllEnabledProviders(), "chrome-devtools-mcp")

	root := t.TempDir()
	profile := makeEntry(t, root, "chrome-profile-default", 4096, 90*24*time.Hour)
	makeEntry(t, root, "old", 2048, 48*time.Hour)
	makeEntry(t, root, "new", 10, time.Hour)

	def.Paths = []string{root}
	def.MaxSize = "1K"
	p, err := NewProvider("chrome-devtools-mcp", def)
	require.NoError(t, err)
	_, err = p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.DirExists(t, profile)
}

func TestEntryProvider_PartialRemovalIsCredited(t *testing.T) {
	root := t.TempDir()
	old := makeEntry(t, root, "old", 2048, 48*time.Hour)
	require.NoError(t, os.MkdirAll(filepath.Join(old, "locked"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(old, "locked", "keep"), make([]byte, 1024), 0o600))
	past := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(old, "locked", "keep"), past, past))
	require.NoError(t, os.Chtimes(filepath.Join(old, "locked"), past, past))
	require.NoError(t, os.Chtimes(old, past, past))
	require.NoError(t, os.Chmod(filepath.Join(old, "locked"), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(old, "locked"), 0o700) })
	makeEntry(t, root, "new", 10, time.Hour)

	res, err := newEntryProvider(t, root, "1K", "").Clean(context.Background(), CleanOptions{})
	if err == nil {
		t.Skip("removal succeeded despite read-only dir (running as root?)")
	}
	assert.Equal(t, int64(2048), res.BytesCleaned)
}

func TestSortEntries_EqualMtimeOrdersByPath(t *testing.T) {
	when := time.Now()
	var entries []cacheEntry
	for i := 40; i > 0; i-- {
		entries = append(entries, cacheEntry{path: fmt.Sprintf("/c/e%02d", i), modTime: when})
	}
	entries = append(entries, cacheEntry{path: "/c/zz-older", modTime: when.Add(-time.Hour)})

	sortEntries(entries)

	assert.Equal(t, "/c/zz-older", entries[0].path)
	for i := 2; i < len(entries); i++ {
		assert.Less(t, entries[i-1].path, entries[i].path)
	}
}
