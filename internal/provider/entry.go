package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/cache"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// EntryProvider cleans caches whose top-level entries are only usable whole
// (model snapshots, browser installs, VM images). It removes whole entries,
// oldest first (ties broken by path), never individual files inside one.
// The newest entry is always kept.
type EntryProvider struct {
	*BaseProvider
	skipPrefixes []string
}

// NewEntryProvider creates a provider that deletes whole top-level entries.
func NewEntryProvider(name string, cfg config.Provider) (*EntryProvider, error) {
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}
	return &EntryProvider{BaseProvider: base, skipPrefixes: cfg.SkipPrefixes}, nil
}

type cacheEntry struct {
	modTime time.Time
	path    string
	size    int64
}

// Clean implements Provider. Both modes only trim to max_size: file mtimes
// record download time, not use, so max_age would evict models and
// browsers still in daily use.
func (p *EntryProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	entries, total, err := p.listEntries(ctx)
	if err != nil {
		return CleanResult{}, err
	}

	sortEntries(entries)

	var (
		freed          int64
		removed        int64
		failed         int
		output         strings.Builder
		removedEntries []Entry
	)
	for i, e := range entries {
		// The newest entry is likely in use; keep it even if over the limit.
		if total-freed <= p.maxSize || i == len(entries)-1 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return CleanResult{BytesCleaned: freed, FilesDeleted: removed, Output: "interrupted", Entries: removedEntries}, err
		}
		if opts.DryRun {
			fmt.Fprintf(&output, "would remove: %s (%s)\n", e.path, size.FormatSize(e.size))
		} else if err := os.RemoveAll(e.path); err != nil {
			fmt.Fprintf(&output, "error removing %s: %v\n", e.path, err)
			failed++
			// RemoveAll may have deleted part of the tree before failing.
			if part := p.partiallyFreed(ctx, e); part > 0 {
				freed += part
				removedEntries = append(removedEntries, Entry{Path: e.path, Size: part})
			}
			continue
		}
		freed += e.size
		removed++
		removedEntries = append(removedEntries, Entry{Path: e.path, Size: e.size})
	}

	res := CleanResult{BytesCleaned: freed, FilesDeleted: removed, Entries: removedEntries}
	if failed > 0 {
		res.Output = strings.TrimSpace(output.String())
		return res, fmt.Errorf("%d entries could not be removed", failed)
	}
	switch {
	case opts.DryRun:
		res.Output = strings.TrimSpace(output.String())
	case output.Len() > 0:
		res.Output = strings.TrimSpace(output.String())
	case removed == 0:
		res.Output = "already under limit"
	default:
		res.Output = fmt.Sprintf("removed %d entries", removed)
	}
	return res, nil
}

// partiallyFreed returns bytes a failed RemoveAll still deleted. An
// unmeasurable remainder credits nothing, so freed is never over-reported.
func (p *EntryProvider) partiallyFreed(ctx context.Context, e cacheEntry) int64 {
	res, err := cache.CalculateSizeContext(ctx, []string{e.path})
	if err != nil || res.Size >= e.size {
		return 0
	}
	return e.size - res.Size
}

// skipped reports whether name matches a configured protected prefix.
func (p *EntryProvider) skipped(name string) bool {
	for _, prefix := range p.skipPrefixes {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (p *EntryProvider) listEntries(ctx context.Context) (entries []cacheEntry, total int64, err error) {
	for _, base := range p.paths {
		dirents, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, 0, err
		}
		for _, d := range dirents {
			// Lock and bookkeeping entries are neither evictable nor
			// candidates for the protected newest slot.
			if strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "__") || p.skipped(d.Name()) {
				continue
			}
			path := filepath.Join(base, d.Name())
			info, err := os.Lstat(path)
			if err != nil {
				continue
			}
			listing, err := cache.ListFilesContext(ctx, []string{path})
			if err != nil {
				return nil, 0, err
			}
			// Newest file wins: a directory mtime misses writes below it.
			e := cacheEntry{path: path, modTime: info.ModTime()}
			e.size = cache.UniqueSize(listing.Files)
			for _, f := range listing.Files {
				if f.ModTime.After(e.modTime) {
					e.modTime = f.ModTime
				}
			}
			entries = append(entries, e)
			total += e.size
		}
	}
	return entries, total, nil
}

// sortEntries orders oldest first; equal mtimes tie-break on path so
// eviction order is deterministic.
func sortEntries(entries []cacheEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].modTime.Before(entries[j].modTime)
		}
		return entries[i].path < entries[j].path
	})
}
