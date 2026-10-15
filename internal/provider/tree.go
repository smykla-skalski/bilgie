package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/smykla-skalski/bilgie/internal/cache"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// treeTrashPrefix names a directory that eviction moved aside before deleting it.
// A leftover is garbage and the next run removes it.
const treeTrashPrefix = ".bilgie-trash-"

// treeSpec says which parts below a provider path may be deleted and how.
// Everything else (indexes, lock files, bookkeeping) is never touched.
//
// files lists directories whose files are independent (content-addressed or
// archive files), so they are trimmed one by one. entries lists slash-separated
// patterns; each match is a complete unit (installed package, extracted crate)
// removed whole, never file by file. modules makes every directory named like
// name@version, except below the top-level cache directory, a whole entry.
// verify runs after files were trimmed to repair what the trim left dangling;
// its failure is reported, not fatal. imaged lists tools whose running
// process holds every tree where only image names are visible (Windows), so
// a tree cannot be matched by path.
type treeSpec struct {
	files   []string
	entries []string
	verify  []string
	modules bool
	imaged  []string
}

// treeSpecs lists providers whose cache holds trees that are only valid
// complete. Their smart clean never deletes inside such a tree.
var treeSpecs = map[string]treeSpec{
	"npm": {
		files:   []string{"_cacache"},
		entries: []string{"_npx/*"},
		verify:  []string{"npm", "cache", "verify"},
		imaged:  []string{"node", "npm", "npx"},
	},
	"cargo": {
		files:   []string{"cache"},
		entries: []string{"src/*/*", "checkouts/*/*", "db/*"},
	},
	"yarn":              {entries: []string{"v*/*"}, imaged: []string{"node", "yarn"}},
	"gradle":            {entries: []string{"*"}, imaged: []string{"java"}},
	"go-mod":            {modules: true},
	"xcode-deriveddata": {entries: []string{"*"}},
	"xcode-archives":    {entries: []string{"*/*"}},
}

// TreeProvider trims caches that mix independent files with trees valid only
// whole. Independent files go by age, then oldest first. Trees go whole,
// oldest first, only while over the limit: mtime records install time, not
// use, so an age rule would evict trees in daily use. The newest tree of each
// pattern, trees modified recently and trees named by a running process are
// kept. Full mode runs clean_cmd when set. max_size counts only what this
// provider may delete.
type TreeProvider struct {
	*BaseProvider
	spec     treeSpec
	cleanCmd string
	cmdArgs  []string
	timeout  time.Duration

	now       func() time.Time
	procLines func(ctx context.Context) ([]string, error)
	evict     func(path string) error
	imageOnly bool
}

// NewTreeProvider creates a provider that never deletes inside a whole-unit tree.
func NewTreeProvider(name string, cfg config.Provider, spec treeSpec) (*TreeProvider, error) {
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}
	args, err := shellquote.Split(cfg.CleanCmd)
	if err != nil {
		return nil, fmt.Errorf("invalid clean_cmd: %w", err)
	}
	timeout, err := parseCleanTimeout(cfg.CleanTimeout)
	if err != nil {
		return nil, err
	}
	return &TreeProvider{
		BaseProvider: base,
		spec:         spec,
		cleanCmd:     cfg.CleanCmd,
		cmdArgs:      args,
		timeout:      timeout,
		now:          time.Now,
		procLines:    processLister(nil),
		evict:        evictTree,
		imageOnly:    runtime.GOOS == "windows",
	}, nil
}

// Available reports whether clean_cmd's executable is on PATH; without a
// command the provider needs no tool.
func (p *TreeProvider) Available() bool {
	if len(p.cmdArgs) == 0 {
		return true
	}
	_, err := exec.LookPath(p.cmdArgs[0])
	return err == nil
}

// treeUnit is one deletable thing: a single file or a whole tree. A non-empty
// hold is why a tree must stay.
type treeUnit struct {
	modTime time.Time
	path    string
	hold    string
	size    int64
	files   int64
	group   int
	isTree  bool
	garbage bool
}

// Clean implements Provider.
func (p *TreeProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	if skipped, ok := p.skipIfBusy(ctx); ok {
		return skipped, nil
	}
	if opts.Mode == CleanModeFull && len(p.cmdArgs) > 0 {
		if opts.DryRun {
			return CleanResult{Output: "would run: " + p.cleanCmd}, nil
		}
		return runMeasuredCleanTimeout(ctx, p.name, p.cmdArgs, p.BaseProvider.CurrentSize, p.timeout)
	}
	return p.trim(ctx, opts)
}

func (p *TreeProvider) trim(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	units, warnings, err := p.collect(ctx)
	if err != nil {
		return CleanResult{}, err
	}
	p.markHeld(ctx, units)

	plan := p.plan(units, opts.Mode == CleanModeSmart)
	if len(plan) == 0 {
		res := CleanResult{Output: p.noopOutput(units, warnings), SkippedEntries: countHeld(units)}
		if strings.HasPrefix(res.Output, overLimitPrefix) {
			res.SkipReason = "over limit but nothing is evictable"
		}
		return res, nil
	}
	return p.execute(ctx, plan, units, warnings, opts)
}

func countHeld(units []treeUnit) int {
	var n int
	for i := range units {
		if units[i].hold != "" {
			n++
		}
	}
	return n
}

const overLimitPrefix = "over limit"

// noopOutput explains an empty plan. A cache still over max_size had nothing
// to evict: every candidate is held or the newest of its pattern.
func (p *TreeProvider) noopOutput(units []treeUnit, warnings int) string {
	var total int64
	for i := range units {
		total += units[i].size
	}
	var out strings.Builder
	if total > p.maxSize {
		fmt.Fprintf(&out, overLimitPrefix+" (%s > %s) but nothing is evictable", size.FormatSize(total), size.FormatSize(p.maxSize))
	} else {
		out.WriteString("already under limit")
	}
	if warnings > 0 {
		fmt.Fprintf(&out, " (%d unreadable)", warnings)
	}
	for i := range units {
		if units[i].hold != "" {
			fmt.Fprintf(&out, "\nskip: %s (%s)", units[i].path, units[i].hold)
		}
	}
	return out.String()
}

// CurrentSize implements Provider. It counts the files and trees the provider
// manages (held or newest ones included), so untouchable bulk such as indexes
// and logs cannot keep it over max_size.
func (p *TreeProvider) CurrentSize(ctx context.Context) (int64, error) {
	units, _, err := p.collect(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for i := range units {
		total += units[i].size
	}
	return total, nil
}

func (p *TreeProvider) collect(ctx context.Context) (units []treeUnit, warnings int, err error) {
	seen := map[string]bool{}
	add := func(u treeUnit) {
		if !seen[u.path] {
			seen[u.path] = true
			units = append(units, u)
		}
	}

	for _, configured := range p.paths {
		root := filepath.Clean(configured)
		for _, rel := range p.spec.files {
			dir := filepath.Join(root, filepath.FromSlash(rel))
			if info, statErr := os.Lstat(dir); statErr != nil || !info.IsDir() {
				continue
			}
			listing, listErr := cache.ListFilesContext(ctx, []string{dir})
			if listErr != nil {
				return nil, 0, listErr
			}
			warnings += len(listing.Warnings)
			for _, f := range listing.Files {
				add(treeUnit{path: f.Path, size: f.Size, modTime: f.ModTime, files: 1, group: -1})
			}
		}

		found, trash, groups := p.findTrees(root)
		for _, dir := range found {
			u, unreadable, ok, measureErr := measureEntry(ctx, dir, groups[dir])
			if measureErr != nil {
				return nil, 0, measureErr
			}
			warnings += unreadable
			if ok {
				add(u)
			}
		}
		for _, dir := range trash {
			u, _, ok, measureErr := measureEntry(ctx, dir, -1)
			if measureErr != nil {
				return nil, 0, measureErr
			}
			if ok {
				u.garbage = true
				add(u)
			}
		}
	}
	return units, warnings, nil
}

func (p *TreeProvider) findTrees(root string) (found, trash []string, groups map[string]int) {
	groups = map[string]int{}
	for i, pattern := range p.spec.entries {
		matches, junk := matchEntries(root, strings.Split(pattern, "/"))
		for _, m := range matches {
			groups[m] = i
		}
		found = append(found, matches...)
		trash = append(trash, junk...)
	}
	if p.spec.modules {
		matches, junk := moduleEntries(root)
		for _, m := range matches {
			groups[m] = len(p.spec.entries)
		}
		found = append(found, matches...)
		trash = append(trash, junk...)
	}
	return found, trash, groups
}

// measureEntry sizes dir and dates it by its newest file, because a directory
// mtime misses writes below it. A symlink or a non-directory is not a tree.
// A tree with unreadable parts is held: its size and date cover only the
// readable part, so it cannot be proven idle.
func measureEntry(ctx context.Context, dir string, group int) (u treeUnit, unreadable int, ok bool, err error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return treeUnit{}, 0, false, nil
	}
	if err != nil {
		return treeUnit{}, 0, false, err
	}
	if !info.IsDir() {
		return treeUnit{}, 0, false, nil
	}
	listing, err := cache.ListFilesContext(ctx, []string{dir})
	if err != nil {
		return treeUnit{}, 0, false, err
	}
	u = treeUnit{path: dir, modTime: info.ModTime(), group: group, isTree: true}
	u.size = cache.UniqueSize(listing.Files)
	for _, f := range listing.Files {
		u.files++
		if f.ModTime.After(u.modTime) {
			u.modTime = f.ModTime
		}
	}
	if n := len(listing.Warnings); n > 0 {
		u.hold = fmt.Sprintf("%d unreadable entries", n)
	}
	return u, len(listing.Warnings), true, nil
}

// markHeld flags trees that must stay: modified within the idle window, or
// named on the command line of a running process. When running processes
// cannot be listed no tree can be proven idle, so none is evictable.
func (p *TreeProvider) markHeld(ctx context.Context, units []treeUnit) {
	var (
		lines     []string
		listErr   error
		listed    bool
		idleSince = p.now().Add(-defaultMinIdle)
	)
	for i := range units {
		u := &units[i]
		if !u.isTree || u.garbage || u.hold != "" {
			continue
		}
		if u.modTime.After(idleSince) {
			u.hold = fmt.Sprintf("modified %s ago", p.now().Sub(u.modTime).Round(time.Second))
			continue
		}
		if !listed {
			lines, listErr = p.procLines(ctx)
			listed = true
		}
		if listErr != nil {
			u.hold = "cannot list processes: " + listErr.Error()
			continue
		}
		if p.imageOnly {
			if proc := firstMatch(lines, p.spec.imaged); proc != "" {
				u.hold = proc + " is running"
			}
			continue
		}
		for _, line := range lines {
			if matchesTree(line, u.path) {
				u.hold = "in use by a running process"
				break
			}
		}
	}
}

// plan returns the units to delete, oldest first with path as tie-break.
// The newest tree of each pattern is likely in use and is never a candidate.
func (p *TreeProvider) plan(units []treeUnit, smart bool) []treeUnit {
	sorted := append([]treeUnit(nil), units...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].modTime.Equal(sorted[j].modTime) {
			return sorted[i].modTime.Before(sorted[j].modTime)
		}
		return sorted[i].path < sorted[j].path
	})

	newest := map[int]string{}
	for i := range sorted {
		if sorted[i].isTree && !sorted[i].garbage {
			newest[sorted[i].group] = sorted[i].path
		}
	}

	var (
		plan      []treeUnit
		remaining int64
		target    = p.maxSize
		cutoff    = p.now().Add(-p.maxAge)
		picked    = map[string]bool{}
	)
	if smart {
		target = int64(float64(p.maxSize) * trimBufferFactor)
	}

	for _, u := range sorted {
		switch {
		case u.garbage:
			plan = append(plan, u)
			picked[u.path] = true
		case smart && p.maxAge > 0 && !u.isTree && u.modTime.Before(cutoff):
			plan = append(plan, u)
			picked[u.path] = true
		default:
			remaining += u.size
		}
	}

	if remaining <= p.maxSize {
		return plan
	}
	for _, u := range sorted {
		if remaining <= target {
			break
		}
		if picked[u.path] || u.hold != "" || (u.isTree && newest[u.group] == u.path) {
			continue
		}
		plan = append(plan, u)
		picked[u.path] = true
		remaining -= u.size
	}
	return plan
}

// trimBufferFactor keeps 10% headroom below max_size, as the file trim does.
const trimBufferFactor = 0.9

func (p *TreeProvider) execute(
	ctx context.Context, plan, all []treeUnit, warnings int, opts CleanOptions,
) (CleanResult, error) {
	dryRun := opts.DryRun
	var (
		res         = CleanResult{SkippedEntries: countHeld(all)}
		out         strings.Builder
		failed      int
		filesFailed int
		filesGone   int64
		treesGone   int64
	)
	for i := range all {
		if all[i].hold != "" {
			fmt.Fprintf(&out, "skip: %s (%s)\n", all[i].path, all[i].hold)
		}
	}

	for _, u := range plan {
		if err := ctx.Err(); err != nil {
			res.Output = "interrupted"
			return res, err
		}
		switch {
		case dryRun && u.isTree:
			fmt.Fprintf(&out, "would remove: %s (%s)\n", u.path, size.FormatSize(u.size))
		case dryRun:
			age := p.now().Sub(u.modTime).Truncate(time.Hour)
			fmt.Fprintf(&out, "would delete: %s (%s, age: %s)\n", u.path, size.FormatSize(u.size), age)
		case u.isTree:
			if err := p.evict(u.path); errors.Is(err, errTreeGone) {
				fmt.Fprintf(&out, "skip: %s (already removed)\n", u.path)
				res.SkippedEntries++
				continue
			} else if err != nil {
				fmt.Fprintf(&out, "error removing %s: %v\n", u.path, err)
				failed++
				continue
			}
		default:
			if err := os.Remove(u.path); err != nil {
				fmt.Fprintf(&out, "error deleting %s: %v\n", u.path, err)
				filesFailed++
				continue
			}
		}
		res.BytesCleaned += u.size
		res.FilesDeleted += u.files
		res.Entries = append(res.Entries, Entry{Path: u.path, Size: u.size})
		if u.isTree {
			treesGone++
		} else {
			filesGone++
		}
		if opts.Recovered != nil && opts.Recovered(res.BytesCleaned) {
			break
		}
	}

	if !dryRun && filesGone > 0 && len(p.spec.verify) > 0 {
		out.WriteString(p.verify(ctx))
	}
	if !dryRun {
		fmt.Fprintf(&out, "removed %d entries, deleted %d files", treesGone, filesGone)
		if filesFailed > 0 {
			fmt.Fprintf(&out, " (%d could not be deleted)", filesFailed)
		}
		if warnings > 0 {
			fmt.Fprintf(&out, " (%d unreadable)", warnings)
		}
	}
	res.Output = strings.TrimSpace(out.String())
	if failed > 0 {
		return res, fmt.Errorf("%d entries could not be removed", failed)
	}
	return res, nil
}

// verify runs the repair command when its tool is installed. The command's
// failure is reported but never fails the clean.
func (p *TreeProvider) verify(ctx context.Context) string {
	args := p.spec.verify
	if _, err := exec.LookPath(args[0]); err != nil {
		return ""
	}
	noSize := func(context.Context) (int64, error) { return 0, nil }
	if _, err := runMeasuredCleanTimeout(ctx, p.name, args, noSize, p.timeout); err != nil {
		return fmt.Sprintf("note: %s failed: %v\n", strings.Join(args, " "), err)
	}
	return "ran: " + strings.Join(args, " ") + "\n"
}

// matchEntries expands slash-separated segments below root. Dot entries are
// skipped; leftover trash directories in the last level come back as junk.
func matchEntries(root string, segs []string) (matches, junk []string) {
	dirents, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	last := len(segs) == 1
	for _, d := range dirents {
		name := d.Name()
		if last && d.IsDir() && strings.HasPrefix(name, treeTrashPrefix) {
			junk = append(junk, filepath.Join(root, name))
			continue
		}
		if strings.HasPrefix(name, ".") || !d.IsDir() {
			continue
		}
		if ok, _ := path.Match(segs[0], name); !ok {
			continue
		}
		child := filepath.Join(root, name)
		if last {
			matches = append(matches, child)
			continue
		}
		m, j := matchEntries(child, segs[1:])
		matches = append(matches, m...)
		junk = append(junk, j...)
	}
	return matches, junk
}

// moduleEntries finds name@version directories below a Go module cache root,
// leaving the top-level cache directory (download archives) alone.
func moduleEntries(root string) (matches, junk []string) {
	root = filepath.Clean(root)
	walkRoot := root
	if info, err := os.Lstat(root); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if resolved, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
			walkRoot = resolved
		}
	}
	atConfigured := func(p string) string {
		rel, err := filepath.Rel(walkRoot, p)
		if err != nil {
			return p
		}
		return filepath.Join(root, rel)
	}
	_ = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == walkRoot || !d.IsDir() {
			return nil
		}
		name := d.Name()
		switch {
		case strings.HasPrefix(name, treeTrashPrefix):
			junk = append(junk, atConfigured(p))
			return fs.SkipDir
		case strings.HasPrefix(name, "."):
			return fs.SkipDir
		case name == "cache" && filepath.Dir(p) == walkRoot:
			return fs.SkipDir
		case strings.Contains(name, "@"):
			matches = append(matches, atConfigured(p))
			return fs.SkipDir
		}
		return nil
	})
	return matches, junk
}

// errTreeGone reports that a tree vanished before eviction, so it freed
// nothing for this run and must not be counted.
var errTreeGone = errors.New("tree already removed")

// evictTree moves dir aside in one rename, then deletes it. The original
// path is either intact or gone, never half removed; a failed delete leaves
// only a trash directory that the next run removes.
func evictTree(dir string) error {
	trash := filepath.Join(filepath.Dir(dir), fmt.Sprintf("%s%d-%d", treeTrashPrefix, os.Getpid(), time.Now().UnixNano()))
	if err := os.Rename(dir, trash); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errTreeGone
		}
		return fmt.Errorf("move aside: %w", err)
	}
	return removeTree(trash)
}

// removeTree deletes dir, making directories writable first when a read-only
// tree (Go module cache) blocks the delete.
func removeTree(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil {
		return nil
	}
	if root, openErr := os.OpenRoot(dir); openErr == nil {
		_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
			if walkErr == nil && d.IsDir() {
				_ = root.Chmod(rel, 0o700)
			}
			return nil
		})
		_ = root.Close()
	}
	if retry := os.RemoveAll(dir); retry != nil {
		return errors.Join(err, retry)
	}
	return nil
}

// foldPath makes a path comparable with a process command line: slashes
// are unified, repeated ones and dot-dot elements resolved, and, on the case-insensitive Windows and macOS file systems,
// so is the case.
func foldPath(s string) string {
	return foldCaseOS(foldWords(unifySlashes(s)))
}

// foldPathSpaced is foldPath for a path with a space in a directory name: a
// dot-dot element resolves against the whole directory name before it, not
// only the last word. A command line matches when either fold matches, so this
// can only hold more trees.
func foldPathSpaced(s string) string {
	return foldCaseOS(foldWords(resolveDotDot(unifySlashes(s))))
}

func unifySlashes(s string) string {
	return strings.ReplaceAll(s, `\`, "/")
}

func foldWords(s string) string {
	parts := strings.Split(s, " ")
	for i, part := range parts {
		if at := strings.Index(part, "/"); at >= 0 {
			parts[i] = part[:at] + path.Clean(part[at:])
		}
	}
	return strings.Join(parts, " ")
}

func foldCaseOS(s string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(s)
	}
	return s
}

// resolveDotDot removes "<dir>/.." pairs where the directory name may hold
// spaces. A dot-dot with no directory before it is left for the per-word fold.
func resolveDotDot(s string) string {
	s = collapseSlashes(s)
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], "/..")
		if i < 0 {
			break
		}
		i += from
		end := i + len("/..")
		if end < len(s) && s[end] != '/' && s[end] != ' ' {
			from = end
			continue
		}
		j := strings.LastIndex(s[:i], "/")
		if j < 0 || s[j+1:i] == ".." || s[j+1:i] == "." || j+1 == i {
			from = end
			continue
		}
		s = s[:j] + s[end:]
		from = 0
	}
	return s
}

func collapseSlashes(s string) string {
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

// matchesTree reports whether a process command line names the tree dir.
func matchesTree(line, dir string) bool {
	want := foldPath(dir)
	return strings.Contains(foldPath(line), want) || strings.Contains(foldPathSpaced(line), want)
}

func firstMatch(lines, wanted []string) string {
	for _, line := range lines {
		if proc := matchProcess(line, wanted); proc != "" {
			return proc
		}
	}
	return ""
}
