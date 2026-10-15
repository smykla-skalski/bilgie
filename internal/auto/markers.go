package auto

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/smykla-skalski/bilgie/internal/provider"
)

// scanLimits bound the git marker scan of one provider path. Depth counts the
// levels listed below the root, so a marker sits in a directory at most depth
// levels down. Dirs and entries cap the directories listed and names read.
type scanLimits struct {
	timeout time.Duration
	depth   int
	dirs    int
	entries int
}

// markerLimits is the budget of the scan. A checkout buried deeper than
// depth is accepted as unverified by the scan; insideGitCheckout still
// protects every path that lies inside a checkout.
var markerLimits = scanLimits{timeout: 5 * time.Second, depth: 3, dirs: 50000, entries: 2000000}

const listBatch = 2048

type dirReader interface {
	ReadDir(n int) ([]os.DirEntry, error)
	Close() error
}

var openDir = func(path string) (dirReader, error) { return os.Open(path) }

type markerOutcome int

const (
	markerNone markerOutcome = iota
	markerFound
	markerTooLarge
	markerUnreadable
	markerCancelled
	markerTimedOut
)

type markerResult struct {
	detail  string
	outcome markerOutcome
}

// findGitMarker looks for a git checkout or worktree below root: an entry
// named .git in any letter case, of any kind. A .git directory is a checkout; a .git file is a
// worktree or submodule checkout whose gitdir line points into another
// repository. Both protect alike, so no file is ever opened. The walk lists
// directory names only, with no stat per file, goes breadth first so shallow
// hits come first, never follows symlinks, stops at the first hit, and fails
// closed when the limits, a read error on the root or ctx stop it before the
// tree is verified. A subdirectory refused with a permission error (macOS
// TCC) is skipped, so a .git below it goes unseen.
func findGitMarker(ctx context.Context, root string, lim scanLimits) markerResult {
	parent := ctx
	if lim.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lim.timeout)
		defer cancel()
	}

	level := []string{root}
	dirs, entries := 0, 0
	for depth := 0; len(level) > 0 && depth <= lim.depth; depth++ {
		var next []string
		for _, dir := range level {
			dirs++
			if dirs > lim.dirs {
				return markerResult{outcome: markerTooLarge}
			}
			listing := listDir(ctx, parent, dir, lim.entries-entries, lim.dirs-dirs-len(next), depth < lim.depth, depth == 0)
			if listing.outcome != markerNone {
				return listing.markerResult
			}
			if listing.found {
				return markerResult{outcome: markerFound, detail: dir}
			}
			entries += listing.count
			next = append(next, listing.subdirs...)
		}
		level = next
	}
	return markerResult{}
}

type dirListing struct {
	markerResult
	subdirs []string
	count   int
	found   bool
}

// listDir reads one directory in batches, so a directory with hundreds of
// thousands of files is never held in memory whole, and returns at the first
// .git entry. budget is the number of names still allowed. A permission error
// skips a subdirectory but never the root.
func listDir(ctx, parent context.Context, dir string, budget, dirBudget int, wantSubdirs, isRoot bool) dirListing {
	f, err := openDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return dirListing{}
	}
	if err != nil {
		if !isRoot && errors.Is(err, fs.ErrPermission) {
			return dirListing{}
		}
		return dirListing{markerResult: markerResult{outcome: markerUnreadable, detail: "cannot verify: " + dir}}
	}
	defer func() { _ = f.Close() }()

	var out dirListing
	for {
		if ctx.Err() != nil {
			out.outcome = ctxOutcome(parent)
			return out
		}
		batch, readErr := f.ReadDir(listBatch)
		out.count += len(batch)
		if out.count > budget {
			out.outcome = markerTooLarge
			return out
		}
		for _, e := range batch {
			if provider.IsGitMarkerName(e.Name()) {
				out.found = true
				return out
			}
			if wantSubdirs && e.IsDir() {
				if len(out.subdirs) >= dirBudget {
					out.outcome = markerTooLarge
					return out
				}
				out.subdirs = append(out.subdirs, filepath.Join(dir, e.Name()))
			}
		}
		if errors.Is(readErr, io.EOF) {
			return out
		}
		if readErr != nil {
			if !isRoot && errors.Is(readErr, fs.ErrPermission) {
				return out
			}
			return dirListing{markerResult: markerResult{outcome: markerUnreadable, detail: "cannot verify: " + dir}}
		}
	}
}

// ctxOutcome tells the scan's own timeout from a caller that cancelled or
// ran out of time. A timeout says the filesystem was slow, not that the tree
// is large.
func ctxOutcome(parent context.Context) markerOutcome {
	if parent.Err() != nil {
		return markerCancelled
	}
	return markerTimedOut
}
