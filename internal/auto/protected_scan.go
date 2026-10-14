package auto

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
)

// DefaultProtectedBudget bounds the size scan of protected entries.
const DefaultProtectedBudget = 10 * time.Second

// ProtectedEntry is a protected location that exists. Partial means the time
// budget ran out while measuring it, so Bytes is a lower bound.
type ProtectedEntry struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Partial bool   `json:"partial,omitempty"`
}

// ProtectedReport lists protected data auto never deletes. Incomplete means
// the budget or the context ended the scan before every entry was measured.
type ProtectedReport struct {
	Entries    []ProtectedEntry `json:"entries"`
	Incomplete bool             `json:"incomplete,omitempty"`
}

// discoverDepth is how far below home the scan looks for worktrees
// directories and git checkouts: ~/code/org/api is three levels down.
const discoverDepth = 4

// discoverDirCap bounds the directories listed by one discovery walk.
const discoverDirCap = 20000

// skippedHomeDirs are never read when looking for protected data: macOS asks
// the user for permission on the first listing of several of them, and none
// is a place checkouts live.
var skippedHomeDirs = []string{"Library", "Documents", "Desktop", "Pictures", "Movies", "Music"}

// dockerDesktopData are the Docker Desktop VM image locations under home. The
// image holds the volumes; auto never prunes volumes, but no protected root
// can name it because the docker provider's own path contains it.
var dockerDesktopData = [][]string{
	{"Library", "Containers", "com.docker.docker", "Data", "vms"},
	{".docker", "desktop", "vms"},
}

// WorktreeHolders returns the worktrees directories and git checkout roots
// within reach of home. Only directory names are read, never file contents.
// A checkout or worktrees directory is not searched further. The walk runs
// off the caller's goroutine so a hung mount cannot outlive ctx; when ctx
// ends first the result is nil.
func WorktreeHolders(ctx context.Context, home string) []string {
	if home == "" {
		return nil
	}
	done := make(chan []string, 1)
	go func() { done <- walkNearHome(ctx, home) }()
	select {
	case out := <-done:
		return out
	case <-ctx.Done():
		return nil
	}
}

func walkNearHome(ctx context.Context, home string) []string {
	var out []string
	level := []string{home}
	listed := 0
	for depth := range discoverDepth {
		var next []string
		for _, dir := range level {
			listed++
			if ctx.Err() != nil || listed > discoverDirCap {
				return out
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			if depth > 0 && slices.ContainsFunc(entries, func(e os.DirEntry) bool { return provider.IsGitMarkerName(e.Name()) }) {
				out = append(out, dir)
				continue
			}
			for _, e := range entries {
				name := e.Name()
				if strings.HasPrefix(name, ".") || name == "node_modules" || (depth == 0 && slices.ContainsFunc(skippedHomeDirs, func(s string) bool { return strings.EqualFold(s, name) })) {
					continue
				}
				path := filepath.Join(dir, name)
				if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
					continue
				}
				if strings.EqualFold(name, "worktrees") {
					out = append(out, path)
				} else {
					next = append(next, path)
				}
			}
		}
		level = next
	}
	return out
}

// dockerDesktopPaths returns the Docker Desktop VM data locations under home.
func dockerDesktopPaths(home string) []string {
	if home == "" {
		return nil
	}
	var out []string
	for _, parts := range dockerDesktopData {
		out = append(out, filepath.Join(append([]string{home}, parts...)...))
	}
	return out
}

// ScanProtected measures the protected locations that exist: the configured
// list plus worktrees directories near home. It stops at budget or when ctx
// is cancelled, and reports sizes found so far as lower bounds.
func ScanProtected(ctx context.Context, cfg *config.Config, home string, budget time.Duration) ProtectedReport {
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}

	candidates := append(append(ProtectedPaths(cfg, home), protectedRoots(home)...), dockerDesktopPaths(home)...)
	candidates = append(candidates, WorktreeHolders(ctx, home)...)
	incomplete := ctx.Err() != nil

	// Resolved, case-folded order puts a parent before its children, so
	// nested entries and aliases of one tree are dropped, not counted twice.
	type entry struct{ path, key string }
	var all []entry
	for _, p := range candidates {
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			continue
		}
		all = append(all, entry{p, foldPath(resolve(p))})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].key != all[j].key {
			return all[i].key < all[j].key
		}
		return all[i].path < all[j].path
	})
	var paths, keys []string
	for _, e := range all {
		if slices.ContainsFunc(keys, func(kept string) bool { return within(e.key, kept) }) {
			continue
		}
		paths = append(paths, e.path)
		keys = append(keys, e.key)
	}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		report = ProtectedReport{Entries: []ProtectedEntry{}, Incomplete: incomplete}
		work   = make(chan string)
	)
	for range unmanagedWorkers {
		wg.Go(func() {
			for path := range work {
				bytes, complete := measureDir(ctx, resolve(path))
				mu.Lock()
				report.Entries = append(report.Entries, ProtectedEntry{Path: path, Bytes: bytes, Partial: !complete})
				if !complete {
					report.Incomplete = true
				}
				mu.Unlock()
			}
		})
	}
feed:
	for i, path := range paths {
		select {
		case work <- path:
		case <-ctx.Done():
			mu.Lock()
			report.Incomplete = true
			for _, left := range paths[i:] {
				report.Entries = append(report.Entries, ProtectedEntry{Path: left, Partial: true})
			}
			mu.Unlock()
			break feed
		}
	}
	close(work)
	wg.Wait()

	sort.Slice(report.Entries, func(i, j int) bool {
		if report.Entries[i].Bytes != report.Entries[j].Bytes {
			return report.Entries[i].Bytes > report.Entries[j].Bytes
		}
		return report.Entries[i].Path < report.Entries[j].Path
	})
	return report
}

// foldPath lowercases on macOS, whose default volumes ignore case.
func foldPath(p string) string {
	if runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}
