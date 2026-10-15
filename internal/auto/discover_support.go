package auto

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
)

// deepLimits bound the extra scan discover runs below a candidate. Unlike the
// marker scan every provider gets, running out of budget here proves nothing
// and does not block the candidate: only a checkout or protected name that
// the walk actually sees does.
var deepLimits = scanLimits{timeout: 2 * time.Second, depth: 8, dirs: 20000, entries: 500000}

// NeverRuns reports whether auto never cleans the provider, whatever its
// settings: docker volumes and Xcode archives, however they are named.
func NeverRuns(name string, pc config.Provider) bool {
	return neverRun(name, pc)
}

// PathProtection explains why auto will not sweep path, or returns "" when
// the path is clear. It runs the verdict a directory-pattern sweep gets,
// which adds the protection by name (Downloads, worktrees) to the same
// checks every provider gets, including the
// bounded scan for git checkouts below path, so a tree it cannot verify
// counts as protected. Discover is stricter than the sweep: it also refuses a
// path with a checkout or a protected name deeper than that scan reaches,
// since suggesting the removal of such a tree would invite the loss of work.
// The error is the context's, when it ended the scan.
func PathProtection(ctx context.Context, path, home string, protected []string) (string, error) {
	reason, err := pathReason(ctx, path, true, true, home, protected)
	if err != nil || reason != "" {
		return reason, err
	}
	return deepReason(ctx, path, deepLimits)
}

// deepReason walks below path for a git marker or a directory named like
// protected data, however deep within lim, and reports the first it sees.
func deepReason(ctx context.Context, path string, lim scanLimits) (string, error) {
	root := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		root = resolved
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", nil
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, lim.timeout)
	defer cancel()

	level := []string{root}
	dirs, entries := 0, 0
	for depth := 0; len(level) > 0 && depth <= lim.depth; depth++ {
		var next []string
		for _, dir := range level {
			dirs++
			if dirs > lim.dirs {
				return "", nil
			}
			listing := listDir(ctx, parent, dir, lim.entries-entries, lim.dirs-dirs-len(next), depth < lim.depth)
			switch {
			case listing.outcome == markerCancelled:
				return "", parent.Err()
			case listing.outcome != markerNone:
				return "", nil
			case listing.found:
				return "protected path " + path + " (git checkout at " + dir + ")", nil
			}
			entries += listing.count
			for _, sub := range listing.subdirs {
				if protectedElement(filepath.Base(sub)) {
					return "protected path " + path + " (holds " + sub + ")", nil
				}
			}
			next = append(next, listing.subdirs...)
		}
		level = next
	}
	return "", nil
}
