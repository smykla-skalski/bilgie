package provider

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/smykla-skalski/bilgie/internal/cache"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// BaseProvider implements common functionality for providers.
type BaseProvider struct {
	name    string
	paths   []string
	maxSize int64
	maxAge  time.Duration
	busy    *busyGuard
}

// NewBaseProvider creates a BaseProvider from config.
func NewBaseProvider(name string, cfg config.Provider) (*BaseProvider, error) {
	paths, err := config.ExpandPaths(cfg.Paths)
	if err != nil {
		return nil, fmt.Errorf("expand paths: %w", err)
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("path %q must be absolute or start with ~/", path)
		}
	}
	return newBaseProviderWithPaths(name, cfg, paths)
}

// newBaseProviderWithPaths builds a BaseProvider from paths that are already
// final, so glob characters in them stay literal.
func newBaseProviderWithPaths(name string, cfg config.Provider, paths []string) (*BaseProvider, error) {
	maxBytes, err := size.ParseSize(cfg.MaxSize)
	if err != nil {
		return nil, fmt.Errorf("parse max_size: %w", err)
	}

	maxAge, err := config.ParseDuration(cfg.MaxAge)
	if err != nil {
		return nil, fmt.Errorf("parse max_age: %w", err)
	}

	return &BaseProvider{
		name:    name,
		paths:   paths,
		maxSize: maxBytes,
		maxAge:  maxAge,
		busy:    newBusyGuard(name, paths),
	}, nil
}

// skipIfBusy returns a skipped result when the provider's tool is active.
func (b *BaseProvider) skipIfBusy(ctx context.Context) (CleanResult, bool) {
	reason := b.busy.busyReason(ctx)
	if reason == "" || ctx.Err() != nil {
		return CleanResult{}, false
	}
	return CleanResult{SkipReason: reason, Output: "skipped: " + reason}, true
}

// Name implements Provider.
func (b *BaseProvider) Name() string {
	return b.name
}

// Paths implements Provider.
func (b *BaseProvider) Paths() []string {
	return b.paths
}

// CurrentSize implements Provider.
func (b *BaseProvider) CurrentSize(ctx context.Context) (int64, error) {
	result, err := cache.CalculateSizeContext(ctx, b.paths)
	return result.Size, err
}

// MaxSize implements Provider.
func (b *BaseProvider) MaxSize() int64 {
	return b.maxSize
}

// MaxAge implements Provider.
func (b *BaseProvider) MaxAge() time.Duration {
	return b.maxAge
}

// Available implements Provider.
func (b *BaseProvider) Available() bool {
	return true
}
