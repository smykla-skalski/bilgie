package auto

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
)

// protectedRoots returns the home-relative locations auto never touches.
func protectedRoots(home string) []string {
	return config.BuiltinProtectedRoots(home)
}

// protectedElement matches path elements that mark user data in the path of
// a directory-pattern sweep. Only sweeps of user directories use it: a name
// says nothing about a provider's own cache tree (mise keeps a downloads/opencode
// directory of tool downloads), where exact roots and git markers decide.
func protectedElement(part string) bool {
	return strings.EqualFold(part, "Downloads") || strings.EqualFold(part, "opencode") || strings.EqualFold(part, "worktrees")
}

// hasProtectedName reports whether any element of a sweep path is protected by name.
func hasProtectedName(path string) bool {
	for part := range strings.SplitSeq(filepath.Clean(path), string(filepath.Separator)) {
		if protectedElement(part) {
			return true
		}
	}
	return false
}

// isProtected reports whether path is, is inside, or contains protected data.
func isProtected(path, home string, scanTree bool) bool {
	return isProtectedWith(path, home, nil, scanTree)
}

// ProtectedPaths returns the absolute protected locations: the built-in
// defaults and the configured entries, with ~ expanded.
func ProtectedPaths(cfg *config.Config, home string) []string {
	var out []string
	for _, entry := range config.MergeProtected(cfg.Protected) {
		if runtime.GOOS == "windows" || strings.HasPrefix(entry, `~\`) {
			entry = strings.ReplaceAll(entry, `\`, "/")
		}
		if rest, ok := strings.CutPrefix(entry, "~/"); ok {
			if home == "" {
				continue
			}
			entry = filepath.Join(home, filepath.FromSlash(rest))
		}
		if !filepath.IsAbs(entry) {
			continue
		}
		out = append(out, filepath.Clean(entry))
	}
	return out
}

// insideGitCheckout reports whether path is a git checkout or worktree root
// or lies inside one. The walk stops at home and the filesystem root, so a
// dotfiles repository in home does not protect everything below it.
func insideGitCheckout(path, home string) bool {
	var homes []string
	if home != "" {
		homes = withoutDataAlias([]string{filepath.Clean(home)})
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			homes = append(homes, withoutDataAlias([]string{resolved})...)
		}
	}
	isHome := func(dir string) bool {
		spellings := []string{dir}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			spellings = append(spellings, resolved)
		}
		return slices.ContainsFunc(withoutDataAlias(spellings), func(d string) bool {
			return slices.ContainsFunc(homes, func(h string) bool { return strings.EqualFold(h, d) })
		})
	}
	for _, start := range withoutDataAlias([]string{filepath.Clean(path)}) {
		for dir := start; ; dir = filepath.Dir(dir) {
			if dir == string(filepath.Separator) || dir == "." || isHome(dir) {
				break
			}
			if hasGitMarker(dir) {
				return true
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return false
}

// isProtectedWith is isProtected plus extra protected roots, such as the
// configured protected list. A tree the marker scan cannot verify counts as
// protected.
func isProtectedWith(path, home string, extra []string, scanTree bool) bool {
	return checkProtected(context.Background(), path, home, extra, scanTree).kind != verdictClear
}

type verdictKind int

const (
	verdictClear verdictKind = iota
	verdictProtected
	verdictUnverified
)

// verdict is the outcome of checkProtected. An unverified verdict means the
// marker scan could not finish (too large, unreadable, cancelled). detail
// is a phrase for the skip reason: the whole reason when unverified, the
// marker location when a scan found a checkout.
type verdict struct {
	detail    string
	kind      verdictKind
	cancelled bool
}

// checkProtected decides whether auto may touch path. Protection is by exact
// location, never by a directory's name: the built-in and configured roots
// and anything inside or above them, Xcode archives, checkouts that contain
// path, and, with scanTree, a git marker within a bounded reach below path.
func checkProtected(ctx context.Context, path, home string, extra []string, scanTree bool) verdict {
	candidates := []string{filepath.Clean(path)}
	resolved, resolveErr := filepath.EvalSymlinks(path)
	if resolveErr == nil {
		candidates = append(candidates, resolved)
	}
	candidates = withoutDataAlias(candidates)

	var roots []string
	for _, root := range append(protectedRoots(home), extra...) {
		roots = append(roots, root)
		if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
			roots = append(roots, resolvedRoot)
		}
	}
	roots = withoutDataAlias(roots)

	for _, p := range candidates {
		if filepath.Dir(p) == p || isXcodeArchives(p) || insideGitCheckout(p, home) {
			return verdict{kind: verdictProtected}
		}
		for _, root := range roots {
			if within(strings.ToLower(p), strings.ToLower(root)) || within(strings.ToLower(root), strings.ToLower(p)) {
				return verdict{kind: verdictProtected}
			}
		}
	}
	if !scanTree {
		return verdict{}
	}

	target := candidates[0]
	if resolveErr == nil {
		target = resolved
	}
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		return verdict{}
	}
	res := findGitMarker(ctx, target, markerLimits)
	switch res.outcome {
	case markerNone:
		return verdict{}
	case markerFound:
		return verdict{kind: verdictProtected, detail: "git checkout at " + res.detail}
	case markerTooLarge:
		return verdict{kind: verdictUnverified, detail: "too large to verify: " + path}
	case markerTimedOut:
		return verdict{kind: verdictUnverified, detail: "scan timed out, cannot verify: " + path}
	case markerUnreadable:
		return verdict{kind: verdictUnverified, detail: res.detail}
	case markerCancelled:
		return verdict{kind: verdictUnverified, cancelled: true}
	}
	return verdict{kind: verdictUnverified, detail: "cannot verify: " + path}
}

// dataVolumeAlias is the firmlink that exposes the user data volume under
// the same paths as the root filesystem.
const dataVolumeAlias = "/System/Volumes/Data"

// withoutDataAlias adds the alias-free spelling of every path under the
// data volume firmlink, so both spellings of home compare equal.
func withoutDataAlias(paths []string) []string {
	out := slices.Clone(paths)
	for _, p := range paths {
		slashed := filepath.ToSlash(p)
		if len(slashed) < len(dataVolumeAlias) || !strings.EqualFold(slashed[:len(dataVolumeAlias)], dataVolumeAlias) {
			continue
		}
		rest := slashed[len(dataVolumeAlias):]
		if rest == "" {
			out = append(out, string(filepath.Separator))
		} else if strings.HasPrefix(rest, "/") {
			out = append(out, filepath.FromSlash(rest))
		}
	}
	return out
}

// within reports whether path equals root or lies under it. The filesystem
// root contains every path.
func within(path, root string) bool {
	root = strings.TrimRight(root, string(filepath.Separator))
	return root == "" || path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// ancestorListCap bounds the names read from one ancestor when looking for a
// differently cased marker. Past it the ancestor counts as holding none: the
// exact .git Lstat already settled the common case.
var ancestorListCap = 100000

// ancestorMemoTTL is how long a marker answer for an ancestor is reused, so
// many providers under one parent list it once per run.
const ancestorMemoTTL = time.Minute

type ancestorAnswer struct {
	at    time.Time
	found bool
}

var ancestorMemo sync.Map

var openAncestor = func(path string) (dirReader, error) { return os.Open(path) }

// hasGitMarker reports whether dir holds a .git entry in any letter case.
// An exact Lstat settles the common hit cheaply; a capped name listing
// catches a differently cased marker on a case-sensitive volume. An
// unreadable dir without an exact marker is not a checkout root. Answers are
// memoized per directory for ancestorMemoTTL.
func hasGitMarker(dir string) bool {
	if v, ok := ancestorMemo.Load(dir); ok {
		if a, ok := v.(ancestorAnswer); ok && time.Since(a.at) < ancestorMemoTTL {
			return a.found
		}
	}
	found, settled := probeGitMarker(dir)
	if settled {
		ancestorMemo.Store(dir, ancestorAnswer{at: time.Now(), found: found})
	}
	return found
}

// probeGitMarker answers for dir; settled is false when the listing failed,
// so the answer is not worth remembering.
func probeGitMarker(dir string) (found, settled bool) {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return true, true
	}
	f, err := openAncestor(dir)
	if err != nil {
		return false, false
	}
	defer func() { _ = f.Close() }()
	read := 0
	for {
		batch, readErr := f.ReadDir(listBatch)
		for _, e := range batch {
			if provider.IsGitMarkerName(e.Name()) {
				return true, true
			}
		}
		read += len(batch)
		if read >= ancestorListCap || readErr != nil {
			return false, readErr == nil || errors.Is(readErr, io.EOF)
		}
	}
}
