package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
)

// FileInfo holds file metadata for cache entries.
type FileInfo struct {
	ModTime time.Time
	Path    string
	Size    int64
	// ID and Nlink identify the inode of a hard-linked file; Shared is false
	// for a file with a single link or an unreadable identity.
	ID     osshim.FileID
	Nlink  uint64
	Shared bool
}

// ScanResult contains size calculation results with access warnings.
type ScanResult struct {
	Warnings []AccessError
	Size     int64
}

// CalculateSize calculates total size of all files under given paths.
// It is the uncancellable form of CalculateSizeContext.
func CalculateSize(paths []string) (ScanResult, error) {
	return CalculateSizeContext(context.Background(), paths)
}

// CalculateSizeContext calculates total size of all files under given paths.
// Walks directories in parallel. Returns 0 if paths is empty.
// Each hard-linked file is counted once, across all paths; where the file
// identity is unreadable (Windows without an openable handle) every link
// counts in full, so the total can only overstate.
// Access errors are collected as warnings rather than stopping the scan.
// The walk stops early and returns ctx.Err() once ctx is cancelled.
func CalculateSizeContext(ctx context.Context, paths []string) (ScanResult, error) {
	var total atomic.Int64
	var firstErr atomic.Value
	var mu sync.Mutex
	var warnings []AccessError
	var links osshim.LinkSet

	var wg sync.WaitGroup
	for _, path := range distinctRoots(paths) {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						accessErr := ClassifyError(path, err)
						mu.Lock()
						warnings = append(warnings, accessErr)
						mu.Unlock()
					}
					return nil
				}
				if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					accessErr := ClassifyError(path, err)
					mu.Lock()
					warnings = append(warnings, accessErr)
					mu.Unlock()
					return nil
				}
				if id, _, shared := osshim.SharedFileID(path, info); shared && !links.Add(id) {
					return nil
				}
				total.Add(info.Size())
				return nil
			})
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
			}
		}(path)
	}
	wg.Wait()

	result := ScanResult{
		Size:     total.Load(),
		Warnings: warnings,
	}

	if err, ok := firstErr.Load().(error); ok && err != nil {
		return result, fmt.Errorf("calculate size: %w", err)
	}
	return result, nil
}

// ListResult contains file listing results with access warnings.
type ListResult struct {
	Files    []FileInfo
	Warnings []AccessError
}

// ListFiles returns file info for all files under given paths.
// It is the uncancellable form of ListFilesContext.
func ListFiles(paths []string) (ListResult, error) {
	return ListFilesContext(context.Background(), paths)
}

// ListFilesContext returns file info for all files under given paths.
// Walks directories in parallel. Returns empty slice if paths is empty.
// Access errors are collected as warnings rather than stopping the scan.
// The walk stops early and returns ctx.Err() once ctx is cancelled.
func ListFilesContext(ctx context.Context, paths []string) (ListResult, error) {
	var mu sync.Mutex
	var files []FileInfo
	var warnings []AccessError
	var firstErr atomic.Value

	var wg sync.WaitGroup
	for _, path := range distinctRoots(paths) {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						accessErr := ClassifyError(path, err)
						mu.Lock()
						warnings = append(warnings, accessErr)
						mu.Unlock()
					}
					return nil
				}
				if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					accessErr := ClassifyError(path, err)
					mu.Lock()
					warnings = append(warnings, accessErr)
					mu.Unlock()
					return nil
				}
				fi := FileInfo{
					Path:    path,
					Size:    info.Size(),
					ModTime: info.ModTime(),
				}
				fi.ID, fi.Nlink, fi.Shared = osshim.SharedFileID(path, info)
				mu.Lock()
				files = append(files, fi)
				mu.Unlock()
				return nil
			})
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
			}
		}(path)
	}
	wg.Wait()

	result := ListResult{
		Files:    files,
		Warnings: warnings,
	}

	if err, ok := firstErr.Load().(error); ok && err != nil {
		return result, fmt.Errorf("list files: %w", err)
	}
	return result, nil
}

// distinctRoots drops paths that name the same place twice: the same file
// once symlinks are resolved, or inside another directory root of the list.
// A root that is itself a symlink never absorbs others, because walking it
// visits nothing, and where a symlink root and a real one name the same file
// the real one is kept. Identity comes from the file system, not from the
// spelling, so case-insensitive and case-sensitive volumes both compare right.
// The result is ordered, so scans are deterministic.
func distinctRoots(paths []string) []string {
	type root struct {
		path, key string
		info      os.FileInfo
		dir, link bool
	}
	roots := make([]root, 0, len(paths))
	for _, p := range paths {
		r := root{path: p, key: filepath.Clean(p)}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			r.key = resolved
		}
		if info, err := os.Lstat(p); err == nil && info != nil {
			r.dir = info.IsDir()
			r.link = info.Mode()&fs.ModeSymlink != 0
		}
		if info, err := os.Stat(r.key); err == nil {
			r.info = info
		}
		roots = append(roots, r)
	}
	slices.SortFunc(roots, func(a, b root) int {
		return cmp.Or(cmp.Compare(len(a.key), len(b.key)), strings.Compare(a.key, b.key), compareBool(a.link, b.link), strings.Compare(a.path, b.path))
	})

	var kept []root
	for _, r := range roots {
		covered := slices.ContainsFunc(kept, func(k root) bool {
			if r.info != nil && k.info != nil && os.SameFile(r.info, k.info) {
				return k.dir == r.dir
			}
			if !k.dir {
				return false
			}
			if r.info == nil || k.info == nil {
				rel, err := filepath.Rel(k.key, r.key)
				return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
			}
			for a := filepath.Dir(r.key); ; a = filepath.Dir(a) {
				if info, err := os.Stat(a); err == nil && os.SameFile(info, k.info) {
					return true
				}
				if filepath.Dir(a) == a {
					return false
				}
			}
		})
		if !covered {
			kept = append(kept, r)
		}
	}
	out := make([]string, len(kept))
	for i, k := range kept {
		out[i] = k.path
	}
	return out
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	default:
		return 1
	}
}
