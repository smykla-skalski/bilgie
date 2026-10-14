package provider

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// gitFileLimit caps the bytes read from a .git file or a gitdir pointer.
const gitFileLimit = 4096

// gitReason returns why the .git entries below dir block its removal, or "".
// Entries whose git data stays inside dir do not block: nothing outside
// depends on them and nothing outside is touched by their removal.
func (p *DirPatternProvider) gitReason(dir string, sc dirScan) string {
	if !p.skipIfGitWorktree || len(sc.gitMarkers) == 0 {
		return ""
	}
	return gitDataOutside(dir, sc.gitMarkers)
}

// gitNote tells that a directory went despite holding .git entries.
func gitNote(sc dirScan) string {
	if len(sc.gitMarkers) == 0 {
		return ""
	}
	noun := "entries"
	if len(sc.gitMarkers) == 1 {
		noun = "entry"
	}
	return fmt.Sprintf(", allowed despite %d .git %s with all git data inside", len(sc.gitMarkers), noun)
}

// gitDataOutside checks every marker below root. It reports the first .git
// entry that reaches outside root, belongs to a repository with a linked
// worktree outside root, or cannot be resolved; the unreadable case fails
// closed.
func gitDataOutside(root string, markers []string) string {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "cannot resolve " + root + ": " + err.Error()
	}
	for _, marker := range markers {
		rel, relErr := filepath.Rel(root, marker)
		if relErr != nil {
			rel = marker
		}
		if reason := checkGitMarker(resolvedRoot, marker, rel); reason != "" {
			return reason
		}
	}
	return ""
}

func checkGitMarker(root, marker, rel string) string {
	info, err := os.Lstat(marker)
	if err != nil {
		return "cannot verify .git at " + rel + ": " + err.Error()
	}
	switch {
	case info.IsDir():
		return checkGitDir(root, marker, rel)
	case info.Mode().IsRegular():
		return checkGitFile(root, marker, rel)
	}
	return "cannot verify .git at " + rel + ": not a directory or file"
}

// checkGitDir finds linked worktrees of the repository that live outside root.
func checkGitDir(root, gitDir, rel string) string {
	entries, err := os.ReadDir(filepath.Join(gitDir, "worktrees"))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "cannot verify .git at " + rel + ": " + err.Error()
	}
	for _, e := range entries {
		pointer := filepath.Join(gitDir, "worktrees", e.Name(), "gitdir")
		target, err := readGitPointer(pointer, "")
		if err != nil {
			return "cannot verify linked worktree " + e.Name() + " of .git at " + rel + ": " + err.Error()
		}
		if resolved := resolveLoose(target); !withinRoot(root, resolved) {
			return "linked worktree outside the root: " + resolved + " (.git at " + rel + ")"
		}
	}
	return ""
}

// checkGitFile follows the gitdir pointer of a .git file and requires its
// target, and the common dir of a linked worktree, to lie inside root.
func checkGitFile(root, file, rel string) string {
	target, err := readGitPointer(file, "gitdir:")
	if err != nil {
		return "cannot verify .git at " + rel + ": " + err.Error()
	}
	resolved := resolveLoose(target)
	if !withinRoot(root, resolved) {
		return "git data outside the root: .git at " + rel + " points to " + resolved
	}

	common, err := readGitPointer(filepath.Join(target, "commondir"), "")
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "cannot verify .git at " + rel + ": " + err.Error()
	}
	if resolvedCommon := resolveLoose(common); !withinRoot(root, resolvedCommon) {
		return "git data outside the root: .git at " + rel + " shares " + resolvedCommon
	}
	return ""
}

// readGitPointer returns the absolute path named by the first line of file,
// after prefix. A relative path is taken from the directory of file.
func readGitPointer(file, prefix string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, gitFileLimit))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	line = strings.TrimSpace(line)
	if prefix != "" {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			return "", errors.New("malformed gitdir line")
		}
		line = strings.TrimSpace(rest)
	}
	if line == "" {
		return "", errors.New("empty gitdir line")
	}

	path := filepath.FromSlash(line)
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(file), path)
	}
	return path, nil
}

// resolveLoose resolves symlinks in path, including a path that does not
// exist yet: the nearest existing ancestor is resolved and the rest appended.
func resolveLoose(path string) string {
	path = filepath.Clean(path)
	var tail []string
	for cur := path; ; {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			parts := append([]string{resolved}, tail...)
			return filepath.Join(parts...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

func withinRoot(root, path string) bool {
	return path == root || isProperAncestor(root, path)
}
