package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// openEvalTTL is how old a listing of open files may be when a directory is
// first evaluated; the check right before removal accepts openSnapshotTTL.
const openEvalTTL = time.Minute

var errRecentEntry = errors.New("entry modified within min_idle")

// defaultMinIdle is how long a directory must be untouched before removal.
const defaultMinIdle = 2 * time.Hour

// DirPatternProvider removes whole stale directories matching a glob.
// A directory is only removed when it is idle, has no open files, keeps no
// git data outside itself and has no running owner named by an owner marker;
// every skip is reported with its reason.
type DirPatternProvider struct {
	*BaseProvider
	minIdle           time.Duration
	skipIfOpen        bool
	skipIfGitWorktree bool
	ownerMarker       bool

	now func() time.Time
	// ownerStart answers whether a process runs and when it started.
	ownerStart func(ctx context.Context, pid int) (time.Time, bool, error)
	// openMany answers for many directories from one listing of open files.
	openMany  func(ctx context.Context, dirs []string) (map[string]bool, error)
	protected []string
	// keep holds directories a match must neither equal nor contain, so a
	// sweep never removes the temp dir the process itself runs in.
	keep  []string
	guard func(path string) string
}

// ChecksGit implements GitChecker.
func (p *DirPatternProvider) ChecksGit() bool { return p.skipIfGitWorktree }

// SetPathGuard implements PathGuarded.
func (p *DirPatternProvider) SetPathGuard(guard func(path string) string) { p.guard = guard }

// NewDirPatternProvider creates a provider that removes stale directories matching cfg.Paths.
func NewDirPatternProvider(name string, cfg config.Provider) (*DirPatternProvider, error) {
	if err := cfg.DirPatternError(); err != nil {
		return nil, err
	}

	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}

	minIdle := defaultMinIdle
	if cfg.MinIdle != "" {
		minIdle, err = config.ParseDuration(cfg.MinIdle)
		if err != nil {
			return nil, fmt.Errorf("parse min_idle: %w", err)
		}
	}

	p := &DirPatternProvider{
		BaseProvider:      base,
		minIdle:           minIdle,
		skipIfOpen:        cfg.SkipIfOpen == nil || *cfg.SkipIfOpen,
		skipIfGitWorktree: cfg.SkipIfGitWorktree == nil || *cfg.SkipIfGitWorktree,
		ownerMarker:       cfg.OwnerMarker,
		now:               time.Now,
		ownerStart:        osshim.ProcessStart,
		openMany:          osshim.OpenFilesUnder,
	}
	if p.ownerMarker {
		p.paths = slices.DeleteFunc(p.paths, func(path string) bool { return !hasOwnerMarker(path) })
	}

	p.keep = []string{os.TempDir()}
	if resolved, evalErr := filepath.EvalSymlinks(os.TempDir()); evalErr == nil {
		p.keep = append(p.keep, resolved)
	}

	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		p.protected = append(p.protected, home)
		if resolved, evalErr := filepath.EvalSymlinks(home); evalErr == nil && resolved != home {
			p.protected = append(p.protected, resolved)
		}
	}

	return p, nil
}

// dirScan is the result of walking one candidate directory.
type dirScan struct {
	newest time.Time
	// gitMarkers holds the .git entries below the directory.
	gitMarkers []string
	// links holds the hard-linked files of the walk, kept out of size: an
	// inode frees space only when every link to it is removed.
	links map[osshim.FileID]linkInfo
	size  int64
	files int64
}

// linkInfo is one hard-linked inode: its size, its total link count on disk
// and how many of those links the walk found.
type linkInfo struct {
	size        int64
	nlink, seen uint64
}

// Clean implements Provider. Smart and full modes behave the same: only
// whole stale directories are removed, never individual files.
//
// Directories are handled one at a time, oldest first, so space is freed as
// soon as the first one passes its checks instead of after every match was
// evaluated. Under disk pressure the sweep stops once opts.Recovered reports
// recovery, and a cancelled sweep returns what it freed so far.
func (p *DirPatternProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	var (
		out    strings.Builder
		result CleanResult
		errs   []error
	)
	finish := func(err error) (CleanResult, error) {
		result.Output = out.String()
		return result, errors.Join(append(errs, err)...)
	}

	dirs, err := p.candidates(ctx)
	if err != nil {
		return finish(err)
	}
	sw := &sweep{p: p, dryRun: opts.DryRun, dirs: dirs, links: newFreedLinks(), files: make(map[string]int64)}

	for i, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		sw.next = i
		if sw.insideRemoved(dir) {
			continue
		}

		sc, reason := p.evaluate(ctx, dir, sw)
		if !opts.DryRun && reason == "" {
			// Confirm the directory is still idle, clean of .git and free
			// of open files right before it goes.
			sc, reason = p.recheckBeforeRemoval(ctx, dir, sc, sw)
		}
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		if reason == "" && !opts.DryRun {
			if _, statErr := os.Lstat(dir); errors.Is(statErr, fs.ErrNotExist) {
				reason = "already removed"
			}
		}
		if reason != "" {
			fmt.Fprintf(&out, "skip: %s (%s)\n", dir, reason)
			result.SkippedEntries++
			continue
		}

		idle := p.now().Sub(sc.newest).Round(time.Second)
		verb := "would remove"
		if !opts.DryRun {
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
				fmt.Fprintf(&out, "error: %s (%v)\n", dir, err)
				continue
			}
			verb = "removed"
		}

		sc.size += sw.links.remove(sc)
		if opts.DryRun {
			sw.dropNested(&result, dir)
		}
		sw.removed = append(sw.removed, dir, resolveParent(dir))
		sw.files[dir] = sc.files
		fmt.Fprintf(&out, "%s: %s (%s, idle %s%s)\n", verb, dir, size.FormatSize(sc.size), idle, gitNote(sc))
		result.BytesCleaned += sc.size
		result.FilesDeleted += sc.files
		result.Entries = append(result.Entries, Entry{Path: dir, Size: sc.size})

		if opts.Recovered != nil && opts.Recovered(result.BytesCleaned) {
			break
		}
	}

	return finish(nil)
}

// candidates lists every distinct matched directory, oldest first. Two
// spellings of one directory (case variants, symlinked parents) count once.
func (p *DirPatternProvider) candidates(ctx context.Context) ([]string, error) {
	type match struct {
		dir string
		mod time.Time
	}
	var (
		matches = make([]match, 0, len(p.paths))
		seen    = make(map[string]bool, len(p.paths))
		infos   []fs.FileInfo
	)
	for _, dir := range p.paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		var mod time.Time
		if info, err := os.Lstat(dir); err == nil {
			if slices.ContainsFunc(infos, func(o fs.FileInfo) bool { return os.SameFile(o, info) }) {
				continue
			}
			infos = append(infos, info)
			mod = info.ModTime()
		}
		matches = append(matches, match{dir: dir, mod: mod})
	}
	slices.SortStableFunc(matches, func(a, b match) int { return a.mod.Compare(b.mod) })

	dirs := make([]string, len(matches))
	for i, m := range matches {
		dirs[i] = m.dir
	}
	return dirs, nil
}

// sweep is the state of one Clean pass.
type sweep struct {
	p       *DirPatternProvider
	dirs    []string
	links   *freedLinks
	files   map[string]int64
	snap    openSnapshot
	removed []string
	next    int
	dryRun  bool
}

// openSnapshot is one listing of open files and when it was taken.
type openSnapshot struct {
	at   time.Time
	open map[string]bool
	err  error
}

// openReason returns why dir has open files, or "". It reuses the pass's
// listing while it is younger than maxAge; a dry run keeps one throughout. The
// listing covers the directories not yet handled, so one machine-wide scan
// serves many of them. A failed listing counts every directory as open.
func (sw *sweep) openReason(ctx context.Context, dir string, maxAge time.Duration) string {
	if !sw.p.skipIfOpen {
		return ""
	}
	if sw.snap.at.IsZero() || (!sw.dryRun && sw.p.now().Sub(sw.snap.at) > maxAge) {
		probeCtx, cancel := context.WithTimeout(ctx, openCheckTimeout)
		defer cancel()
		sw.snap.open, sw.snap.err = sw.p.openMany(probeCtx, sw.dirs[sw.next:])
		sw.snap.at = sw.p.now()
	}
	switch {
	case sw.snap.err != nil:
		return "open-file check failed: " + sw.snap.err.Error()
	case sw.snap.open[dir]:
		return "has open files"
	}
	return ""
}

// insideRemoved reports whether dir lies below a directory already removed
// (or, in a dry run, already counted): that match's own scan covered it.
func (sw *sweep) insideRemoved(dir string) bool {
	self := resolveParent(dir)
	return slices.ContainsFunc(sw.removed, func(root string) bool {
		return isProperAncestor(root, self) || isProperAncestor(root, dir)
	})
}

// dropNested takes back the dry-run entries below dir, which dir's own scan
// counts again.
func (sw *sweep) dropNested(res *CleanResult, dir string) {
	root := resolveParent(dir)
	res.Entries = slices.DeleteFunc(res.Entries, func(e Entry) bool {
		if !isProperAncestor(root, resolveParent(e.Path)) {
			return false
		}
		res.BytesCleaned -= e.Size
		res.FilesDeleted -= sw.files[e.Path]
		return true
	})
}

func isProperAncestor(ancestor, path string) bool {
	rel, err := filepath.Rel(ancestor, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// recheckBeforeRemoval repeats every check that can change while other
// directories are being processed, open files included.
func (p *DirPatternProvider) recheckBeforeRemoval(ctx context.Context, dir string, before dirScan, sw *sweep) (sc dirScan, skipReason string) {
	sc, skipReason = p.recheck(ctx, dir, before)
	if skipReason != "" {
		return sc, skipReason
	}
	if reason := p.ownerReason(ctx, dir); reason != "" {
		return sc, reason
	}
	return sc, sw.openReason(ctx, dir, openSnapshotTTL)
}

// freedLinks tracks hard-linked inodes across the removed directories. An
// inode counts as freed once, in the removal that takes its last link: the
// links removed so far equal the link count first seen.
type freedLinks struct {
	nlink   map[osshim.FileID]uint64
	removed map[osshim.FileID]uint64
}

func newFreedLinks() *freedLinks {
	return &freedLinks{nlink: make(map[osshim.FileID]uint64), removed: make(map[osshim.FileID]uint64)}
}

// remove records that sc's directory is gone and returns the bytes of the
// shared inodes that this removal frees.
func (f *freedLinks) remove(sc dirScan) int64 {
	var n int64
	for id, li := range sc.links {
		if _, known := f.nlink[id]; !known {
			f.nlink[id] = li.nlink
		}
		before := f.removed[id]
		f.removed[id] += li.seen
		if before < f.nlink[id] && f.removed[id] >= f.nlink[id] {
			n += li.size
		}
	}
	return n
}

// evaluate decides whether dir may be removed. A non-empty reason means skip.
// Cheap checks run first; the expensive open-files check runs last.
func (p *DirPatternProvider) evaluate(ctx context.Context, dir string, sw *sweep) (sc dirScan, skipReason string) {
	if p.isProtected(dir) || p.isProtected(resolveParent(dir)) {
		return sc, "protected path"
	}
	if p.containsKept(dir) || p.containsKept(resolveParent(dir)) {
		return sc, "contains the temp dir in use"
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return sc, "cannot stat: " + err.Error()
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return sc, "symlink"
	}
	if !info.IsDir() {
		return sc, "not a directory"
	}

	if reason := p.ownerReason(ctx, dir); reason != "" {
		return sc, reason
	}

	if p.skipIfGitWorktree && IsGitMarkerName(filepath.Base(dir)) {
		return sc, "is a .git directory"
	}

	sc, skipReason = scanDir(ctx, dir, info.ModTime(), p.idleCutoff())
	if skipReason != "" {
		return sc, skipReason
	}

	if reason := p.gitReason(dir, sc); reason != "" {
		return sc, reason
	}

	if idle := p.now().Sub(sc.newest); idle < p.minIdle {
		return sc, fmt.Sprintf("modified %s ago, min_idle %s", idle.Round(time.Second), p.minIdle)
	}

	// The marker scan is the slowest check, so only idle directories get it.
	if p.guard != nil {
		if reason := p.guard(dir); reason != "" {
			return sc, reason
		}
	}

	if reason := sw.openReason(ctx, dir, openEvalTTL); reason != "" {
		return sc, reason
	}

	return p.recheck(ctx, dir, sc)
}

// idleCutoff is the time after which a modification makes a directory too
// recent to remove.
func (p *DirPatternProvider) idleCutoff() time.Time { return p.now().Add(-p.minIdle) }

// recheck rescans after the slow checks so activity that started meanwhile
// (new writes, a fresh .git) still blocks removal.
func (p *DirPatternProvider) recheck(ctx context.Context, dir string, before dirScan) (sc dirScan, skipReason string) {
	sc, skipReason = scanDir(ctx, dir, before.newest, p.idleCutoff())
	if skipReason != "" {
		return sc, skipReason
	}
	if sc.newest.After(before.newest) {
		return sc, "modified during checks"
	}
	return sc, p.gitReason(dir, sc)
}

// resolveParent returns dir with symlinks in its parent chain resolved, so a
// pattern reaching home or root through a symlink is still recognised. The
// final component stays unresolved: a symlink match is skipped separately.
func resolveParent(dir string) string {
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return dir
	}
	return filepath.Join(parent, filepath.Base(dir))
}

// isProtected guards against patterns that resolve to the filesystem root,
// the home directory, one of its ancestors, or a direct child of root or home.
func (p *DirPatternProvider) isProtected(dir string) bool {
	clean := filepath.Clean(dir)
	parent := filepath.Dir(clean)
	if !filepath.IsAbs(clean) || parent == clean || filepath.Dir(parent) == parent {
		return true
	}
	for _, prot := range p.protected {
		prot = filepath.Clean(prot)
		if clean == prot || filepath.Dir(clean) == prot || strings.HasPrefix(prot, clean+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// containsKept reports whether dir is, or is an ancestor of, a kept directory.
func (p *DirPatternProvider) containsKept(dir string) bool {
	clean := filepath.Clean(dir)
	for _, k := range p.keep {
		k = filepath.Clean(k)
		if clean == k || strings.HasPrefix(k, clean+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// scanDir walks dir without following symlinks, collecting the newest mtime,
// the first .git entry and size totals. Any unreadable entry yields a skip
// reason: an incomplete scan cannot prove the directory is idle.
//
// A non-zero cutoff stops the walk at the first entry modified after it: the
// directory is too recent to remove, so the rest of its size is moot.
func scanDir(ctx context.Context, dir string, rootMod, cutoff time.Time) (sc dirScan, skipReason string) {
	sc = dirScan{newest: rootMod}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		var (
			id     osshim.FileID
			nlink  uint64
			shared bool
		)
		if info.Mode().IsRegular() {
			id, nlink, shared = osshim.SharedFileID(path, info)
			if shared {
				// Directory entries of hard-linked files can carry stale
				// times on Windows; the open above refreshes them.
				if fresh, statErr := os.Lstat(path); statErr == nil {
					info = fresh
				}
			}
		}
		if info.ModTime().After(sc.newest) {
			sc.newest = info.ModTime()
		}
		if !cutoff.IsZero() && info.ModTime().After(cutoff) {
			return errRecentEntry
		}

		if IsGitMarkerName(d.Name()) && path != dir {
			sc.gitMarkers = append(sc.gitMarkers, path)
		}

		if info.Mode().IsRegular() {
			sc.files++
			if shared {
				if sc.links == nil {
					sc.links = make(map[osshim.FileID]linkInfo)
				}
				li := sc.links[id]
				li.size, li.nlink = info.Size(), nlink
				li.seen++
				sc.links[id] = li
				return nil
			}
			sc.size += info.Size()
		}
		return nil
	})
	if err != nil && !errors.Is(err, errRecentEntry) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sc, "scan interrupted: " + ctxErr.Error()
		}
		return sc, "scan failed: " + err.Error()
	}

	return sc, ""
}
