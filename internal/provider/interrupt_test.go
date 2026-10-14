package provider

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doneBudgetCtx reports cancellation once Done has been polled more than
// allowed times, which lets a test interrupt a loop after N iterations.
type doneBudgetCtx struct {
	context.Context
	mu      sync.Mutex
	allowed int
	polls   int
	closed  chan struct{}
	open    chan struct{}
}

func newDoneBudgetCtx(allowed int) *doneBudgetCtx {
	c := &doneBudgetCtx{Context: context.Background(), allowed: allowed, closed: make(chan struct{}), open: make(chan struct{})}
	close(c.closed)
	return c
}

func (c *doneBudgetCtx) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polls++
	if c.polls > c.allowed {
		return c.closed
	}
	return c.open
}

func (c *doneBudgetCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.polls > c.allowed {
		return context.Canceled
	}
	return nil
}

func entriesTotal(res CleanResult) int64 {
	var total int64
	for _, e := range res.Entries {
		total += e.Size
	}
	return total
}

func TestJetBrainsProvider_InterruptKeepsEntriesInStepWithBytes(t *testing.T) {
	root := t.TempDir()
	names := []string{"GoLand2023.1", "GoLand2023.2", "GoLand2023.3"}
	dirs := make([]string, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), make([]byte, 1024), 0o600))
		dirs = append(dirs, dir)
	}
	p, err := NewJetBrainsProvider("jetbrains", config.Provider{Paths: []string{root}, MaxSize: "1G", Enabled: true})
	require.NoError(t, err)

	res, err := p.cleanDirs(newDoneBudgetCtx(1), dirs, CleanOptions{})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "interrupted", res.Output)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, dirs[0], res.Entries[0].Path)
	assert.Positive(t, res.BytesCleaned)
	assert.Equal(t, res.BytesCleaned, entriesTotal(res))
}

func ageFile(t *testing.T, path string, bytes int, age time.Duration) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, make([]byte, bytes), 0o600))
	when := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, when, when))
}

func TestFileProvider_InterruptKeepsEntriesInStepWithBytes(t *testing.T) {
	for name, mode := range map[string]CleanMode{"full": CleanModeFull, "smart": CleanModeSmart} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for i, name := range []string{"a", "b", "c", "d"} {
				ageFile(t, filepath.Join(root, name), 1000, time.Duration(100-i)*24*time.Hour)
			}
			p, err := NewFileProvider("test", config.Provider{Paths: []string{root}, MaxSize: "1K", MaxAge: "30d", Enabled: true})
			require.NoError(t, err)

			var res CleanResult
			if mode == CleanModeSmart {
				res, err = p.smartClean(newDoneBudgetCtx(1), CleanOptions{Mode: mode})
			} else {
				res, err = p.fullClean(newDoneBudgetCtx(1), CleanOptions{Mode: mode})
			}

			require.ErrorIs(t, err, context.Canceled)
			assert.NotEmpty(t, res.Entries)
			assert.Positive(t, res.BytesCleaned)
			assert.Equal(t, res.BytesCleaned, entriesTotal(res))
		})
	}
}
