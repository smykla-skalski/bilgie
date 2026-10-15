package provider

import (
	"path/filepath"
	"strings"
)

const (
	maxCandidate = 1024
	maxMisses    = 8
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

// resolvedPaths returns every absolute path text names and every directory
// along it, resolved through all symlinks and normalized for comparison. Any
// alias of a directory therefore compares equal to the directory itself, a
// symlink to it too, and a leaf that links elsewhere (a venv's bin/python)
// does not hide the directory in front of it. The raw spellings are matched
// separately, so a path that does not resolve loses nothing.
func (r *pathResolver) resolvedPaths(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		if key := normalizeText(path); !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	for i := range len(text) {
		if text[i] == '/' && pathStart(text, i) {
			r.walk(text[i:candidateEnd(text, i)], add)
		}
	}
	return out
}

// walk resolves each leading part of candidate that ends at a slash or a
// character that may end a path, and reports it. The part past the longest
// one that exists is kept as written: a link the sweep has just removed or a
// file not created yet still names the directory it lies in.
func (r *pathResolver) walk(candidate string, add func(string)) {
	deepest, deepestResolved, misses := "", "", 0
	for end := 1; end <= len(candidate) && misses < maxMisses; end++ {
		slash := end < len(candidate) && candidate[end] == '/'
		if !slash && end != len(candidate) && !pathEnd(candidate[end]) {
			continue
		}
		prefix := strings.TrimRight(candidate[:end], " ")
		if len(prefix) < 2 {
			continue
		}
		if resolved, ok := r.resolve(prefix); ok {
			add(resolved)
			deepest, deepestResolved, misses = prefix, resolved, 0
			continue
		}
		if slash {
			break
		}
		misses++
	}
	if deepest != "" && len(deepest) < len(candidate) {
		add(deepestResolved + candidate[len(deepest):])
	}
}

// pathStart reports whether the slash at text[i] begins a path rather than
// continues one: it follows a delimiter, or a short flag such as -I or -L.
func pathStart(text string, i int) bool {
	if i == 0 {
		return true
	}
	prev := text[i-1]
	switch {
	case prev >= 'a' && prev <= 'z' || prev >= 'A' && prev <= 'Z':
		return i >= 2 && text[i-2] == '-' && (i == 2 || !idByte(text[i-3]))
	case idByte(prev) || prev == '.' || prev == '+' || prev == '%' || prev == '/':
		return false
	}
	return true
}

// candidateEnd is where the path that starts at text[from] stops: at the end
// of the line or at the length cap. Everything else may be part of a path.
func candidateEnd(text string, from int) int {
	end := from + 1
	for end < len(text) && end-from < maxCandidate && text[end] != '\n' && text[end] != 0 {
		end++
	}
	return end
}

func pathEnd(b byte) bool {
	switch b {
	case ' ', ':', ';', ',', '"', '\'', '=', '(', ')', '<', '>', '|', '`', '$', '&', '[', ']', '{', '}':
		return true
	}
	return false
}
