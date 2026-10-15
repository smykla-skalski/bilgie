package provider

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
)

type artifactKind string

const (
	kindRust   artifactKind = "rust"
	kindNode   artifactKind = "node"
	kindPython artifactKind = "python"
)

const (
	trashPrefix = ".bilgie-trash-"
	sampleLimit = 3000
	sizeWorkers = 4

	cacheDirTagSignature = "Signature: 8a477f597d28d172789f06886806bc55"
)

// trashPattern is the exact shape of the directories removeAside creates.
var trashPattern = regexp.MustCompile(`^\.bilgie-trash-(target|node_modules|venv)-[0-9a-z]{8}$`)

// ownsTrash reports whether name, found in dir, is a leftover of ours: it has
// the exact trash shape and dir is a project of the matching kind.
func ownsTrash(dir, name string) bool {
	m := trashPattern.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	switch m[1] {
	case "target":
		return isRegular(filepath.Join(dir, "Cargo.toml"))
	case "node_modules":
		return isRegular(filepath.Join(dir, "package.json"))
	default:
		return hasPythonProjectFile(dir)
	}
}

// ownedArtifact reports whether the directory called name inside dir is a
// recognised artifact (valid marker beside its project file), the only place
// those names are skipped: src/target is source.
func ownedArtifact(dir, name string) bool {
	var kind artifactKind
	switch foldName(name) {
	case "target":
		kind = kindRust
	case "node_modules":
		kind = kindNode
	case ".venv", "venv":
		kind = kindPython
	default:
		return false
	}
	return markersValid(&artifactDir{Path: filepath.Join(dir, name), Kind: kind})
}

// neverDescend are directory names the project search skips by name, marker
// or not. The idleness sample skips them only where they are artifacts.
var neverDescend = map[string]bool{
	".git":         true,
	"node_modules": true,
	"target":       true,
	".venv":        true,
	"venv":         true,
}

type artifactDir struct {
	Path  string
	Kind  artifactKind
	Size  int64
	Files int64

	unreadable bool

	used      time.Time
	usageErr  error
	usageRead bool
}

type project struct {
	Dir       string
	root      string
	alias     string
	artifacts []*artifactDir

	measured bool
	problem  string
	repoRoot string
	// gitDir and gitTime are the git admin directory and its activity time
	// as first read, so the final check can tell new git activity.
	gitDir  string
	gitTime time.Time
	newest  time.Time
	sampled time.Time
	// ownRoot is the project directory's mtime after our own removals could
	// not restore it; that bump is not an edit.
	ownRoot time.Time
}

type artifactScan struct {
	projects []*project
	trash    []string
	partial  bool
	// refused lists the configured roots that were not scanned and why.
	refused []string
	// roots counts the usable roots that were walked.
	roots int
}

func (s *artifactScan) total() int64 {
	var total int64
	for _, proj := range s.projects {
		for _, art := range proj.artifacts {
			total += art.Size
		}
	}
	return total
}

// foldName lowercases names on the OSes whose default volumes ignore case, so
// a Target directory is the target directory there; elsewhere names are exact.
func foldName(name string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}

func trashName(base string) string {
	return trashPrefix + strings.ToLower(strings.TrimPrefix(base, ".")) + "-" + strings.ToLower(cryptorand.Text()[:8])
}

// discover finds every project artifact below the roots and measures it. The
// time budget bounds the search; sizing is bounded by ctx alone.
func (p *ProjectArtifactsProvider) discover(ctx context.Context) (*artifactScan, error) {
	scan := &artifactScan{}
	searchCtx, cancel := context.WithDeadline(ctx, p.now().Add(p.budget))
	defer cancel()

	seen := map[string]bool{}
	for _, root := range p.paths {
		resolved, refusal, ok := p.usableRoot(root)
		if !ok {
			if refusal != "" {
				scan.refused = append(scan.refused, root+" "+refusal)
			}
			continue
		}
		if seen[foldPathText(resolved)] {
			continue
		}
		seen[foldPathText(resolved)] = true
		scan.roots++
		w := &walker{p: p, scan: scan, root: resolved, raw: filepath.Clean(root), seen: seen}
		w.walk(searchCtx, resolved, 0)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scan.partial = searchCtx.Err() != nil

	sort.Slice(scan.projects, func(i, j int) bool { return scan.projects[i].Dir < scan.projects[j].Dir })
	sort.Strings(scan.trash)

	if err := sizeArtifacts(ctx, scan); err != nil {
		return nil, err
	}
	return scan, nil
}

// usableRoot resolves a configured root and rejects any that is missing, not
// a directory, the filesystem root, home or a parent of home: scanning those
// would wander into places no project artifact lives. A missing root is
// rejected without a refusal text, since a root that is not there is normal;
// every other rejection says why.
func (p *ProjectArtifactsProvider) usableRoot(root string) (resolved, refusal string, ok bool) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", false
	}
	info, err := os.Stat(resolved)
	switch {
	case err != nil:
		return "", "", false
	case !info.IsDir():
		return "", "is not a directory", false
	case filepath.Dir(resolved) == resolved:
		return "", "is the filesystem root", false
	}
	for _, home := range homeSpellings(p.home) {
		if pathWithin(home, resolved) {
			return "", "is home or a parent of home", false
		}
	}
	return resolved, "", true
}

func homeSpellings(home string) []string {
	if home == "" {
		return nil
	}
	out := []string{filepath.Clean(home)}
	if resolved, err := filepath.EvalSymlinks(home); err == nil && resolved != out[0] {
		out = append(out, resolved)
	}
	return out
}

type walker struct {
	p    *ProjectArtifactsProvider
	scan *artifactScan
	root string
	raw  string
	seen map[string]bool
}

func (w *walker) walk(ctx context.Context, dir string, depth int) {
	if ctx.Err() != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	byName := make(map[string]fs.DirEntry, len(entries))
	for _, e := range entries {
		byName[foldName(e.Name())] = e
	}

	if arts := w.p.detect(dir, byName); len(arts) > 0 {
		w.scan.projects = append(w.scan.projects, &project{Dir: dir, root: w.root, alias: w.alias(dir), artifacts: arts})
	}

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, name)
		switch {
		case trashPattern.MatchString(name):
			if ownsTrash(dir, name) {
				w.scan.trash = append(w.scan.trash, child)
			}
		case neverDescend[strings.ToLower(name)], depth >= w.p.maxDepth, lexicallyProtected(child), w.seen[foldPathText(child)]:
		default:
			w.seen[foldPathText(child)] = true
			w.walk(ctx, child, depth+1)
		}
	}
}

// alias spells dir through the configured root when that differs from the
// resolved one, because a process command line may use either spelling.
func (w *walker) alias(dir string) string {
	if w.raw == w.root {
		return ""
	}
	rel, err := filepath.Rel(w.root, dir)
	if err != nil {
		return ""
	}
	return filepath.Join(w.raw, rel)
}

// detect returns the artifact directories of one project directory. Every
// kind needs its marker inside the artifact and its project file beside it.
func (p *ProjectArtifactsProvider) detect(dir string, byName map[string]fs.DirEntry) []*artifactDir {
	var out []*artifactDir
	add := func(kind artifactKind, name string) {
		art := &artifactDir{Path: filepath.Join(dir, byName[name].Name()), Kind: kind}
		if markersValid(art) {
			out = append(out, art)
		}
	}
	isDir := func(name string) bool {
		e, ok := byName[name]
		return ok && e.IsDir()
	}
	if p.kinds[kindRust] && isDir("target") {
		add(kindRust, "target")
	}
	if p.kinds[kindNode] && isDir("node_modules") {
		add(kindNode, "node_modules")
	}
	if p.kinds[kindPython] {
		for _, name := range []string{".venv", "venv"} {
			if isDir(name) {
				add(kindPython, name)
			}
		}
	}
	return out
}

// markersValid checks the artifact's marker file and its sibling project
// file on disk. A symlinked directory or marker never counts.
func markersValid(art *artifactDir) bool {
	if !isRealDir(art.Path) {
		return false
	}
	project := filepath.Dir(art.Path)
	switch art.Kind {
	case kindRust:
		return isRegular(filepath.Join(project, "Cargo.toml")) && validCacheDirTag(filepath.Join(art.Path, "CACHEDIR.TAG"))
	case kindNode:
		return isRegular(filepath.Join(project, "package.json"))
	case kindPython:
		return isRegular(filepath.Join(art.Path, "pyvenv.cfg")) && hasPythonProjectFile(project)
	}
	return false
}

func isRealDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func isRegular(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

func hasPythonProjectFile(project string) bool {
	if isRegular(filepath.Join(project, "pyproject.toml")) {
		return true
	}
	matches, err := filepath.Glob(filepath.Join(escapeGlobMeta(project), "requirements*.txt"))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(matches, isRegular)
}

func escapeGlobMeta(dir string) string {
	return strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(dir)
}

// validCacheDirTag accepts only a regular file whose first line is the cache
// directory tag signature cargo writes.
func validCacheDirTag(path string) bool {
	if !isRegular(path) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, len(cacheDirTagSignature)+2)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	first, _, _ := bytes.Cut(buf[:n], []byte("\n"))
	return string(bytes.TrimRight(first, "\r")) == cacheDirTagSignature
}

func sizeArtifacts(ctx context.Context, scan *artifactScan) error {
	work := make(chan *artifactDir)
	var wg sync.WaitGroup
	for range sizeWorkers {
		wg.Go(func() {
			for art := range work {
				art.Size, art.Files, art.unreadable = measureTree(ctx, art.Path)
			}
		})
	}
feed:
	for _, proj := range scan.projects {
		for _, art := range proj.artifacts {
			select {
			case work <- art:
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(work)
	wg.Wait()
	return ctx.Err()
}

// measureTree sums regular file sizes without following symlinks. A
// hard-linked file counts once in the tree; files still counts every path.
// unreadable reports entries that could not be read, which makes the size a
// lower bound and the tree one that cannot be proven removable whole.
func measureTree(ctx context.Context, dir string) (total, files int64, unreadable bool) {
	var links osshim.LinkSet
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			unreadable = unreadable || !errors.Is(err, fs.ErrNotExist)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			unreadable = unreadable || !errors.Is(infoErr, fs.ErrNotExist)
			return nil
		}
		files++
		if id, _, shared := osshim.SharedFileID(path, info); shared && !links.Add(id) {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, files, unreadable
}

// sampleNewest returns the newest mtime among dir and the first limit entries
// below it. See sampleTree.
func sampleNewest(ctx context.Context, dir string, limit int) (newest time.Time, truncated bool, err error) {
	root, below, truncated, err := sampleTree(ctx, dir, limit)
	return latest(root, below), truncated, err
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// sampleTree walks dir breadth first, never following symlinks, and returns
// dir's own mtime and the newest mtime among the first limit entries below
// it. It skips .git, our trash and the recognised artifact directories of each
// directory; a source folder that merely shares an artifact's name is read.
// truncated reports that the limit cut the walk short.
func sampleTree(ctx context.Context, dir string, limit int) (root, below time.Time, truncated bool, err error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	root = info.ModTime()
	level := []string{dir}
	seen := 0
	for len(level) > 0 {
		var next []string
		for _, cur := range level {
			if err := ctx.Err(); err != nil {
				return root, below, false, err
			}
			entries, readErr := os.ReadDir(cur)
			if readErr != nil {
				if errors.Is(readErr, fs.ErrNotExist) {
					continue
				}
				return root, below, false, readErr
			}
			for _, e := range entries {
				if seen >= limit {
					return root, below, true, nil
				}
				name := e.Name()
				if e.IsDir() && (isGitName(name) || ownedArtifact(cur, name) || (trashPattern.MatchString(name) && ownsTrash(cur, name))) {
					continue
				}
				seen++
				fi, infoErr := e.Info()
				if infoErr != nil {
					continue
				}
				below = latest(below, fi.ModTime())
				if e.IsDir() {
					next = append(next, filepath.Join(cur, name))
				}
			}
		}
		level = next
	}
	return root, below, false, nil
}
