package provider

import (
	"path/filepath"
	"strings"
)

const maxCandidate = 1024

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

// resolvedPaths returns every absolute path text names, resolved through all
// symlinks and normalized for comparison. Any alias of a directory therefore
// compares equal to the directory itself, and so does a symlink to it. The
// raw spellings are matched separately, so a path that does not resolve
// loses nothing.
func (r *pathResolver) resolvedPaths(text string) []string {
	var out []string
	seen := map[string]bool{}
	for i := range len(text) {
		if text[i] != '/' || !pathStart(text, i) {
			continue
		}
		resolved, ok := r.resolveLongest(text[i:candidateEnd(text, i)])
		if !ok {
			continue
		}
		if key := normalizeText(resolved); !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// resolveLongest resolves the longest leading part of candidate that exists
// and keeps the rest as written: a path a process names may continue past
// the file system (arguments after a space, a file not created yet).
func (r *pathResolver) resolveLongest(candidate string) (string, bool) {
	for end := len(candidate); end > 1; end-- {
		if end < len(candidate) && candidate[end] != '/' && candidate[end] != ' ' {
			continue
		}
		prefix := strings.TrimRight(candidate[:end], " ")
		if len(prefix) < 2 {
			continue
		}
		if resolved, ok := r.resolve(prefix); ok {
			return resolved + candidate[len(prefix):], true
		}
	}
	return "", false
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

// candidateEnd is where the path that starts at text[from] stops: at a
// delimiter that cannot be part of a path, or at the length cap. Spaces stay
// in, because a path may contain them.
func candidateEnd(text string, from int) int {
	end := from + 1
	for end < len(text) && end-from < maxCandidate {
		switch text[end] {
		case '\n', 0, ':', ';', ',', '"', '\'', '=', '(', ')', '<', '>', '|', '`', '$', '&', '[', ']', '{', '}':
			return end
		}
		end++
	}
	return end
}
