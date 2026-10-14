package cache

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// TrimOptions configures cache trimming.
type TrimOptions struct {
	MaxSize int64         // Target size (10% buffer applied internally)
	MaxAge  time.Duration // Delete files older than this
	DryRun  bool
	// Exclude drops matching files before planning, so they neither count
	// toward MaxSize nor get deleted.
	Exclude func(path string) bool
}

// TrimResult contains trimming operation results.
type TrimResult struct {
	Output string
	Errors []AccessError
	// Removed lists the files deleted, or that a dry-run would delete.
	Removed      []FileInfo
	DeletedCount int64
	FreedBytes   int64
}

const trimBufferFactor = 0.9 // Keep 10% headroom below max_size

// LinkLedger credits the bytes of a hard-linked inode once, when the last of
// its links is removed, and only if every link on disk was in the listing:
// a link outside it keeps the data alive, so removing the rest frees nothing.
type LinkLedger struct {
	listed  map[osshim.FileID]uint64
	removed map[osshim.FileID]uint64
}

// NewLinkLedger counts the links of each shared inode in files.
func NewLinkLedger(files []FileInfo) *LinkLedger {
	l := &LinkLedger{listed: map[osshim.FileID]uint64{}, removed: map[osshim.FileID]uint64{}}
	for _, f := range files {
		if f.Shared {
			l.listed[f.ID]++
		}
	}
	return l
}

// Freeable reports whether removing f can ever free its bytes: an unshared
// file, or a shared inode whose every link is in the listing.
func (l *LinkLedger) Freeable(f FileInfo) bool {
	return !f.Shared || l.listed[f.ID] >= f.Nlink
}

// GroupLinks reorders files so the links of a shared inode sit together, at
// the position of the first one. A trim then removes an inode whole instead of
// stopping with some links gone and the data still alive.
func GroupLinks(files []FileInfo) []FileInfo {
	byID := map[osshim.FileID][]FileInfo{}
	for _, f := range files {
		if f.Shared {
			byID[f.ID] = append(byID[f.ID], f)
		}
	}
	out := make([]FileInfo, 0, len(files))
	for _, f := range files {
		if !f.Shared {
			out = append(out, f)
			continue
		}
		out = append(out, byID[f.ID]...)
		delete(byID, f.ID)
	}
	return out
}

// UniqueSize is the size of files with each hard-linked inode counted once.
func UniqueSize(files []FileInfo) int64 {
	var total int64
	seen := map[osshim.FileID]bool{}
	for _, f := range files {
		if f.Shared {
			if seen[f.ID] {
				continue
			}
			seen[f.ID] = true
		}
		total += f.Size
	}
	return total
}

// Remove records that f's link is gone and returns the bytes that freed.
func (l *LinkLedger) Remove(f FileInfo) int64 {
	if !f.Shared {
		return f.Size
	}
	l.removed[f.ID]++
	if l.removed[f.ID] == l.listed[f.ID] && l.listed[f.ID] >= f.Nlink {
		return f.Size
	}
	return 0
}

// Trim deletes files that are:
// - older than MaxAge, OR
// - oldest files until total ≤ MaxSize (with 10% buffer).
func Trim(ctx context.Context, paths []string, opts TrimOptions) (TrimResult, error) {
	listResult, err := ListFilesContext(ctx, paths)
	if err != nil {
		return TrimResult{}, err
	}

	files := listResult.Files
	if opts.Exclude != nil {
		files = slices.DeleteFunc(files, func(f FileInfo) bool { return opts.Exclude(f.Path) })
	}
	if len(files) == 0 {
		return TrimResult{Output: "no files found", Errors: listResult.Warnings}, nil
	}

	// Sort by ModTime (oldest first). ModTime is appropriate for dev caches
	// where content-addressable storage updates mtime on access.
	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime.Before(files[j].ModTime)
	})
	files = GroupLinks(files)

	var (
		result        TrimResult
		deleteErrors  []AccessError
		output        strings.Builder
		cutoff        = time.Now().Add(-opts.MaxAge)
		targetSize    = int64(float64(opts.MaxSize) * trimBufferFactor)
		toDelete      []FileInfo
		remainingSize int64
	)

	// Carry forward scan warnings
	deleteErrors = append(deleteErrors, listResult.Warnings...)

	// Hard links share data: sizes count each inode once and a link frees
	// bytes only when the last one goes.
	planned := NewLinkLedger(files)
	remainingSize = UniqueSize(files)

	// Phase 1: mark files older than MaxAge for deletion
	for _, f := range files {
		if f.ModTime.Before(cutoff) {
			toDelete = append(toDelete, f)
			remainingSize -= planned.Remove(f)
		}
	}

	// Phase 2: if still over target, delete oldest remaining files
	if remainingSize > targetSize {
		var remaining []FileInfo
		for _, f := range files {
			if f.ModTime.Before(cutoff) || !planned.Freeable(f) {
				continue // already marked, or removing it frees nothing
			}
			remaining = append(remaining, f)
		}

		for _, f := range remaining {
			if remainingSize <= targetSize {
				break
			}
			toDelete = append(toDelete, f)
			remainingSize -= planned.Remove(f)
		}
	}
	freed := NewLinkLedger(files)

	// Execute deletions
	for _, f := range toDelete {
		select {
		case <-ctx.Done():
			result.Output = "interrupted"
			return result, ctx.Err()
		default:
		}

		if opts.DryRun {
			age := time.Since(f.ModTime).Truncate(time.Hour)
			fmt.Fprintf(&output, "would delete: %s (%s, age: %s)\n", f.Path, size.FormatSize(f.Size), age)
			result.FreedBytes += freed.Remove(f)
			result.DeletedCount++
			result.Removed = append(result.Removed, f)
			continue
		}

		if err := os.Remove(f.Path); err != nil {
			deleteErrors = append(deleteErrors, ClassifyError(f.Path, err))
			continue
		}

		result.FreedBytes += freed.Remove(f)
		result.DeletedCount++
		result.Removed = append(result.Removed, f)
	}

	if opts.DryRun {
		result.Output = output.String()
		return result, nil
	}

	result.Output = fmt.Sprintf("deleted %d files", result.DeletedCount)
	result.Errors = deleteErrors

	return result, nil
}
