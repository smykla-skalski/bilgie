package provider

import (
	"path/filepath"
	"runtime"
	"strings"
)

const (
	archiveMarker = "/" + uvArchiveDir + "/"
	envMarker     = "/" + uvEnvDirPrefix
	maxRootStarts = 16
	envEntryParts = 3
	archiveParts  = 2
)

// pathResolver resolves symlinks once per path for one evidence snapshot. A
// path that does not resolve has no entry, so callers keep its raw spelling.
type pathResolver struct {
	memo map[string]string
}

func newPathResolver() *pathResolver {
	return &pathResolver{memo: map[string]string{}}
}

func (r *pathResolver) resolve(path string) (string, bool) {
	if r == nil {
		resolved, err := filepath.EvalSymlinks(path)
		return resolved, err == nil
	}
	if got, ok := r.memo[path]; ok {
		return got, got != ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = ""
	}
	r.memo[path] = resolved
	return resolved, resolved != ""
}

// cacheRef is a cache entry a process names: the symlink-free cache root and
// the entry below it ("archive-v0/<id>" or "environments-vN/<bucket>/<name>").
// Both are normalized for comparison.
type cacheRef struct {
	root string
	rest string
}

// cacheRefs finds every archive-v0/<id> and environments-v*/<bucket>/<name>
// path in text and resolves the directory in front of it, whatever alias the
// process spelled it with. The raw spellings are matched separately, so a
// root that no longer resolves loses nothing.
func (r *pathResolver) cacheRefs(text string) []cacheRef {
	var refs []cacheRef
	for _, marker := range []string{archiveMarker, envMarker} {
		for from := 0; ; {
			i := indexPathMarker(text, marker, from)
			if i < 0 {
				break
			}
			from = i + len(marker)
			rest := entryAfter(text, i+1, marker == envMarker)
			if rest == "" {
				continue
			}
			for _, start := range rootStarts(text, i) {
				root, ok := r.resolve(text[start:i])
				if !ok {
					continue
				}
				refs = append(refs, cacheRef{root: normalizeText(root), rest: normalizeText(rest)})
			}
		}
	}
	return refs
}

// entryAfter reads the cache entry that begins at text[from]: archive-v0/<id>
// or environments-v*/<bucket>/<name>. It returns "" when the entry is
// incomplete.
func entryAfter(text string, from int, env bool) string {
	parts := archiveParts
	if env {
		parts = envEntryParts
	}
	end := from
	for n := range parts {
		if n > 0 {
			if end >= len(text) || text[end] != '/' {
				return ""
			}
			end++
		}
		start := end
		for end < len(text) && idByte(text[end]) {
			end++
		}
		if end == start {
			return ""
		}
	}
	return text[from:end]
}

// rootStarts returns the offsets before marker where an absolute path may
// begin: a slash at the start of the text or after a delimiter. A path with
// spaces starts after the delimiter in front of it, so every candidate is
// tried and those that do not resolve are dropped.
func rootStarts(text string, marker int) []int {
	var starts []int
	for j := marker - 1; j >= 0 && len(starts) < maxRootStarts; j-- {
		if text[j] == '\n' || text[j] == 0 {
			break
		}
		if text[j] == '/' && (j == 0 || pathDelimiter(text[j-1])) {
			starts = append(starts, j)
		}
	}
	return starts
}

func pathDelimiter(b byte) bool {
	switch b {
	case ' ', '\t', ':', '=', '"', '\'', '(', ';', ',', '[', '<', '>':
		return true
	}
	return false
}

// indexPathMarker finds marker in text from an offset, ignoring case where
// the file system does. The markers are ASCII, so offsets stay valid.
func indexPathMarker(text, marker string, from int) int {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		i := strings.Index(text[from:], marker)
		if i < 0 {
			return -1
		}
		return from + i
	}
	for i := from; i+len(marker) <= len(text); i++ {
		if text[i] == '/' && strings.EqualFold(text[i:i+len(marker)], marker) {
			return i
		}
	}
	return -1
}
