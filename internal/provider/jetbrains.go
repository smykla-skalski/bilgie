package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/cache"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

var versionDirPattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*)(\d{4}\.\d+)$`)

// JetBrainsProvider cleans old JetBrains version directories while keeping the latest per product.
type JetBrainsProvider struct {
	*BaseProvider
}

// NewJetBrainsProvider creates a JetBrains version-aware provider.
func NewJetBrainsProvider(name string, cfg config.Provider) (*JetBrainsProvider, error) {
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}

	return &JetBrainsProvider{
		BaseProvider: base,
	}, nil
}

type versionDir struct {
	product string
	version string
	path    string
}

// Clean implements Provider.
func (p *JetBrainsProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	if opts.Mode == CleanModeSmart {
		return p.smartClean(ctx, opts)
	}
	return p.fullClean(ctx, opts)
}

func (p *JetBrainsProvider) smartClean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	removable, err := p.findRemovableDirs()
	if err != nil {
		return CleanResult{}, err
	}

	// Filter by maxAge if configured
	if p.maxAge > 0 {
		cutoff := time.Now().Add(-p.maxAge)
		filtered := make([]string, 0)
		for _, dir := range removable {
			info, err := os.Stat(dir)
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				filtered = append(filtered, dir)
			}
		}
		removable = filtered
	}

	return p.cleanDirs(ctx, removable, opts)
}

func (p *JetBrainsProvider) fullClean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	removable, err := p.findRemovableDirs()
	if err != nil {
		return CleanResult{}, err
	}

	return p.cleanDirs(ctx, removable, opts)
}

func (p *JetBrainsProvider) cleanDirs(ctx context.Context, removable []string, opts CleanOptions) (CleanResult, error) {
	if len(removable) == 0 {
		return CleanResult{Output: "no old versions to clean"}, nil
	}

	var (
		bytesTotal int64
		output     strings.Builder
		entries    []Entry
	)

	for _, dir := range removable {
		select {
		case <-ctx.Done():
			return CleanResult{
				BytesCleaned: bytesTotal,
				Entries:      entries,
				Output:       "interrupted",
			}, ctx.Err()
		default:
		}

		dirSize, err := cache.CalculateSizeContext(ctx, []string{dir})
		if err != nil {
			fmt.Fprintf(&output, "warning: size calculation failed for %s: %v\n", filepath.Base(dir), err)
		}

		if opts.DryRun {
			fmt.Fprintf(&output, "would remove: %s (%s)\n", filepath.Base(dir), size.FormatSize(dirSize.Size))
			bytesTotal += dirSize.Size
			entries = append(entries, Entry{Path: dir, Size: dirSize.Size})
			continue
		}

		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(&output, "error removing %s: %v\n", filepath.Base(dir), err)
			continue
		}

		bytesTotal += dirSize.Size
		entries = append(entries, Entry{Path: dir, Size: dirSize.Size})
	}

	if opts.DryRun {
		return CleanResult{
			Entries:      entries,
			BytesCleaned: bytesTotal,
			Output:       strings.TrimSpace(output.String()),
		}, nil
	}

	result := CleanResult{
		Entries:      entries,
		BytesCleaned: bytesTotal,
		Output:       fmt.Sprintf("removed %d old version directories", len(removable)),
	}
	if output.Len() > 0 {
		result.Output = strings.TrimSpace(output.String())
	}

	return result, nil
}

// findRemovableDirs returns paths of old version directories that can be removed.
func (p *JetBrainsProvider) findRemovableDirs() ([]string, error) {
	products := make(map[string][]versionDir)

	for _, basePath := range p.paths {
		entries, err := os.ReadDir(basePath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			matches := versionDirPattern.FindStringSubmatch(entry.Name())
			if matches == nil {
				continue
			}

			vd := versionDir{
				product: matches[1],
				version: matches[2],
				path:    filepath.Join(basePath, entry.Name()),
			}
			products[vd.product] = append(products[vd.product], vd)
		}
	}

	var removable []string
	for _, versions := range products {
		if len(versions) <= 1 {
			continue
		}

		sort.Slice(versions, func(i, j int) bool {
			return compareVersions(versions[i].version, versions[j].version) < 0
		})

		// Remove all but the latest version
		for _, vd := range versions[:len(versions)-1] {
			removable = append(removable, vd.path)
		}
	}

	return removable, nil
}

// compareVersions compares semantic versions like "2024.1" and "2024.10".
// Returns -1 if v1 < v2, 0 if equal, 1 if v1 > v2.
//
// A missing segment counts as 0. A segment that is non-numeric or out of
// int range is compared lexically rather than silently coerced to 0, so
// malformed versions order deterministically instead of collapsing together.
func compareVersions(v1, v2 string) int {
	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	maxLen := max(len(parts2), len(parts1))

	for i := range maxLen {
		n1, raw1, ok1 := versionSegment(parts1, i)
		n2, raw2, ok2 := versionSegment(parts2, i)

		if !ok1 || !ok2 {
			if c := strings.Compare(raw1, raw2); c != 0 {
				return c
			}
			continue
		}

		if n1 < n2 {
			return -1
		}
		if n1 > n2 {
			return 1
		}
	}

	return 0
}

// versionSegment returns the i-th dot-separated version segment as an int.
// A missing segment is treated as 0 with ok=true; a present segment that
// fails to parse (non-numeric or out of range) returns ok=false along with
// its raw text for lexical fallback comparison.
func versionSegment(parts []string, i int) (n int, raw string, ok bool) {
	if i >= len(parts) {
		return 0, "", true
	}
	raw = parts[i]
	n, err := strconv.Atoi(raw)
	return n, raw, err == nil
}
