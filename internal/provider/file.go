package provider

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/smykla-skalski/bilgie/internal/cache"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// FileProvider cleans caches by deleting oldest files until under limit.
type FileProvider struct {
	*BaseProvider
	skipIfOpen bool
	openCheck  func(ctx context.Context, dir string) (bool, error)
}

// NewFileProvider creates a provider that cleans by file deletion.
func NewFileProvider(name string, cfg config.Provider) (*FileProvider, error) {
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}

	return &FileProvider{
		BaseProvider: base,
		skipIfOpen:   cfg.SkipIfOpen != nil && *cfg.SkipIfOpen,
		openCheck:    osshim.HasOpenFiles,
	}, nil
}

// Clean implements Provider.
func (p *FileProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	if skipped, ok := p.skipIfBusy(ctx); ok {
		return skipped, nil
	}
	if opts.Mode == CleanModeSmart {
		return p.smartClean(ctx, opts)
	}
	return p.fullClean(ctx, opts)
}

// cleanablePaths drops every path with a file open in a running process when
// skip_if_open is set. An unanswerable check counts as open.
func (p *FileProvider) cleanablePaths(ctx context.Context) (paths []string, skipped string) {
	if !p.skipIfOpen {
		return p.paths, ""
	}
	var out strings.Builder
	for _, path := range p.paths {
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		open, err := p.openCheck(ctx, path)
		switch {
		case err != nil:
			fmt.Fprintf(&out, "skip: %s (open-file check failed: %v)\n", path, err)
		case open:
			fmt.Fprintf(&out, "skip: %s (has open files)\n", path)
		default:
			paths = append(paths, path)
		}
	}
	return paths, out.String()
}

func (p *FileProvider) smartClean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	paths, skipped := p.cleanablePaths(ctx)
	trimResult, err := cache.Trim(ctx, paths, cache.TrimOptions{
		MaxSize: p.maxSize,
		MaxAge:  p.maxAge,
		DryRun:  opts.DryRun,
	})
	if err != nil {
		// An interrupted trim reports what it already removed.
		return CleanResult{
			BytesCleaned: trimResult.FreedBytes,
			FilesDeleted: trimResult.DeletedCount,
			Output:       trimResult.Output,
			Entries:      entriesFromFiles(trimResult.Removed),
		}, err
	}

	result := CleanResult{
		BytesCleaned: trimResult.FreedBytes,
		FilesDeleted: trimResult.DeletedCount,
		Output:       skipped + trimResult.Output,
		Entries:      entriesFromFiles(trimResult.Removed),
	}

	if len(trimResult.Errors) > 0 {
		result.Output = skipped + formatResultWithErrors(trimResult.Output, trimResult.DeletedCount, trimResult.Errors)
	}

	return result, nil
}

func (p *FileProvider) fullClean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	paths, skipped := p.cleanablePaths(ctx)
	sized, err := cache.CalculateSizeContext(ctx, paths)
	if err != nil {
		return CleanResult{}, err
	}
	currentSize := sized.Size

	if currentSize <= p.maxSize {
		return CleanResult{
			Output: skipped + "already under limit",
		}, nil
	}

	listResult, err := cache.ListFilesContext(ctx, paths)
	if err != nil {
		return CleanResult{}, err
	}

	files := listResult.Files
	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime.Before(files[j].ModTime)
	})
	files = cache.GroupLinks(files)

	ledger := cache.NewLinkLedger(files)
	var (
		bytesToDelete = currentSize - p.maxSize
		bytesDeleted  int64
		filesDeleted  int64
		deleteErrors  []cache.AccessError
		output        strings.Builder
		entries       []Entry
	)

	// Carry forward scan warnings
	deleteErrors = append(deleteErrors, listResult.Warnings...)

	for _, f := range files {
		if bytesDeleted >= bytesToDelete {
			break
		}
		if !ledger.Freeable(f) {
			continue
		}

		select {
		case <-ctx.Done():
			return CleanResult{
				BytesCleaned: bytesDeleted,
				FilesDeleted: filesDeleted,
				Entries:      entries,
				Output:       "interrupted",
			}, ctx.Err()
		default:
		}

		if opts.DryRun {
			fmt.Fprintf(&output, "would delete: %s (%s)\n", f.Path, size.FormatSize(f.Size))
			bytesDeleted += ledger.Remove(f)
			filesDeleted++
			entries = append(entries, Entry{Path: f.Path, Size: f.Size})
			continue
		}

		if err := os.Remove(f.Path); err != nil {
			deleteErrors = append(deleteErrors, cache.ClassifyError(f.Path, err))
			continue
		}

		bytesDeleted += ledger.Remove(f)
		filesDeleted++
		entries = append(entries, Entry{Path: f.Path, Size: f.Size})
	}

	if opts.DryRun {
		return CleanResult{
			BytesCleaned: bytesDeleted,
			FilesDeleted: filesDeleted,
			Output:       skipped + output.String(),
			Entries:      entries,
		}, nil
	}

	result := CleanResult{
		BytesCleaned: bytesDeleted,
		FilesDeleted: filesDeleted,
		Output:       skipped + fmt.Sprintf("deleted %d files", filesDeleted),
		Entries:      entries,
	}

	if len(deleteErrors) > 0 {
		result.Output = skipped + formatResultWithErrors(result.Output[len(skipped):], filesDeleted, deleteErrors)
	}

	return result, nil
}

func formatResultWithErrors(base string, deleted int64, errors []cache.AccessError) string {
	var permCount, lockedCount, otherCount int
	for _, e := range errors {
		switch e.Reason {
		case cache.ReasonPermissionDenied:
			permCount++
		case cache.ReasonFileLocked:
			lockedCount++
		default:
			otherCount++
		}
	}

	var parts []string
	if permCount > 0 {
		parts = append(parts, fmt.Sprintf("%d permission denied", permCount))
	}
	if lockedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d locked", lockedCount))
	}
	if otherCount > 0 {
		parts = append(parts, fmt.Sprintf("%d other errors", otherCount))
	}

	if len(parts) == 0 {
		return base
	}

	errorSummary := fmt.Sprintf("(%s)", strings.Join(parts, ", "))
	if base == "" {
		return fmt.Sprintf("deleted %d files %s", deleted, errorSummary)
	}
	return fmt.Sprintf("%s %s", base, errorSummary)
}
