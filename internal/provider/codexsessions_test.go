package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeRollout(t *testing.T, root, rel string, bytes int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, make([]byte, bytes), 0o600))
	when := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, when, when))
	return path
}

func newCodexProvider(t *testing.T, root, maxAge string) *CodexSessionsProvider {
	t.Helper()
	p, err := NewCodexSessionsProvider("codex-sessions", config.Provider{
		Paths: []string{root}, MaxSize: "1G", MaxAge: maxAge, Enabled: true,
	})
	require.NoError(t, err)
	p.openMany = func(context.Context, []string) (map[string]bool, error) { return map[string]bool{}, nil }
	return p
}

func TestCodexSessions_RemovesOnlyOldRollouts(t *testing.T) {
	root := t.TempDir()
	old := writeRollout(t, root, "2026/01/01/rollout-a.jsonl", 100, 100*day)
	fresh := writeRollout(t, root, "2026/10/01/rollout-b.jsonl", 100, 5*day)
	other := writeRollout(t, root, "2026/01/01/notes.txt", 100, 100*day)
	otherJSONL := writeRollout(t, root, "2026/01/01/history.jsonl", 100, 100*day)

	res, err := newCodexProvider(t, root, "30d").Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)

	assert.NoFileExists(t, old)
	assert.FileExists(t, fresh)
	assert.FileExists(t, other)
	assert.FileExists(t, otherJSONL)
	assert.Equal(t, int64(100), res.BytesCleaned)
	assert.Equal(t, int64(1), res.FilesDeleted)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, old, res.Entries[0].Path)
}

func TestCodexSessions_DryRunListsWithoutRemoving(t *testing.T) {
	root := t.TempDir()
	old := writeRollout(t, root, "rollout-a.jsonl", 2048, 100*day)

	res, err := newCodexProvider(t, root, "30d").Clean(context.Background(), CleanOptions{DryRun: true})
	require.NoError(t, err)

	assert.FileExists(t, old)
	assert.Contains(t, res.Output, "would remove: "+old)
	assert.Contains(t, res.Output, "2.0 KiB")
	assert.Equal(t, int64(2048), res.BytesCleaned)
	assert.Equal(t, int64(1), res.FilesDeleted)
}

func TestCodexSessions_KeepsOpenFiles(t *testing.T) {
	root := t.TempDir()
	open := writeRollout(t, root, "rollout-open.jsonl", 100, 100*day)
	closed := writeRollout(t, root, "rollout-closed.jsonl", 100, 100*day)
	p := newCodexProvider(t, root, "30d")
	p.openMany = func(context.Context, []string) (map[string]bool, error) {
		return map[string]bool{open: true}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)

	assert.FileExists(t, open)
	assert.NoFileExists(t, closed)
	assert.Contains(t, res.Output, "kept 1 sessions")
}

func TestCodexSessions_UnknownOpenStateKeepsEverything(t *testing.T) {
	root := t.TempDir()
	old := writeRollout(t, root, "rollout-a.jsonl", 100, 100*day)
	p := newCodexProvider(t, root, "30d")
	p.openMany = func(context.Context, []string) (map[string]bool, error) {
		return nil, errors.New("lsof not found")
	}

	res, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)

	assert.FileExists(t, old)
	assert.Zero(t, res.FilesDeleted)
	assert.Contains(t, res.Output, "open-file check failed")
}

func TestCodexSessions_SkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	target := writeRollout(t, t.TempDir(), "rollout-real.jsonl", 100, 100*day)
	link := filepath.Join(root, "rollout-link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}

	_, err := newCodexProvider(t, root, "30d").Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)

	assert.FileExists(t, target)
	assert.FileExists(t, link)
}

func TestCodexSessions_SizeCountsOnlyRollouts(t *testing.T) {
	root := t.TempDir()
	writeRollout(t, root, "a/rollout-1.jsonl", 100, day)
	writeRollout(t, root, "b/rollout-2.jsonl", 50, day)
	writeRollout(t, root, "b/other.bin", 1000, day)

	got, err := newCodexProvider(t, root, "30d").CurrentSize(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(150), got)
}

func TestCodexSessions_MissingRoot(t *testing.T) {
	p := newCodexProvider(t, filepath.Join(t.TempDir(), "nope"), "30d")

	res, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.Zero(t, res.FilesDeleted)

	got, err := p.CurrentSize(context.Background())
	require.NoError(t, err)
	assert.Zero(t, got)
}

func TestCodexSessions_EmptyMaxAgeUsesDefault(t *testing.T) {
	p, err := NewCodexSessionsProvider("codex-sessions", config.Provider{
		Paths: []string{t.TempDir()}, MaxSize: "1G", Enabled: true,
	})
	require.NoError(t, err)
	assert.Equal(t, config.DefaultMaxAge, p.MaxAge())
}

func TestCodexSessions_CancelledContext(t *testing.T) {
	root := t.TempDir()
	old := writeRollout(t, root, "rollout-a.jsonl", 100, 100*day)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newCodexProvider(t, root, "30d").Clean(ctx, CleanOptions{})
	require.Error(t, err)
	assert.FileExists(t, old)
}

func TestNewProvider_CodexSessions(t *testing.T) {
	p, err := NewProvider("codex-sessions", config.DefaultProvidersFor(config.Platform{OS: config.OSDarwin, Home: t.TempDir()})["codex-sessions"])
	require.NoError(t, err)
	assert.IsType(t, &CodexSessionsProvider{}, p)
}
