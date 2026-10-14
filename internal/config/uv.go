package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const uvProvider = "uv"

// ResolveUVCacheDir picks the directory bilgie sizes and hands to uv. The
// result is a literal path and so is the input: only a leading ~ is expanded,
// never a glob, so glob characters in a directory name cannot match a sibling.
// An explicit path wins. The per-OS default follows uv's own order:
// UV_CACHE_DIR, then $XDG_CACHE_HOME/uv (not on Windows), then the OS default.
func ResolveUVCacheDir(paths []string) (string, error) {
	if len(paths) != 1 || strings.TrimSpace(paths[0]) == "" {
		return "", fmt.Errorf("paths must name exactly one uv cache directory, got %d", countNonBlank(paths))
	}
	if err := RequireAbsoluteOrTilde(paths[0]); err != nil {
		return "", err
	}
	expanded, err := ExpandTilde(paths[0])
	if err != nil {
		return "", fmt.Errorf("expand paths: %w", err)
	}
	dir := filepath.Clean(expanded)
	// ~\x and \x pass the portable check but name a directory below whatever
	// directory bilgie runs in on Unix.
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("path %q must be absolute or start with ~/", paths[0])
	}
	if !isDefaultUVDir(dir) {
		if looksLikeGlob(dir) && !pathIsDir(dir) {
			return "", fmt.Errorf("paths is a literal directory, not a glob pattern, and %q does not exist", dir)
		}
		return dir, nil
	}
	if env := os.Getenv("UV_CACHE_DIR"); env != "" {
		abs, err := filepath.Abs(env)
		if err != nil {
			return "", fmt.Errorf("resolve UV_CACHE_DIR %q: %w", env, err)
		}
		return abs, nil
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" && runtime.GOOS != "windows" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, uvProvider), nil
	}
	return dir, nil
}

func countNonBlank(paths []string) int {
	n := 0
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			n++
		}
	}
	return n
}

func looksLikeGlob(path string) bool {
	return strings.ContainsAny(path, "*?")
}

func pathIsDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func isDefaultUVDir(dir string) bool {
	defaults := []string{"~/.cache/uv"}
	defaults = append(defaults, DefaultProviders()[uvProvider].Paths...)
	for _, d := range defaults {
		exp, err := ExpandTilde(d)
		if err != nil {
			continue
		}
		if samePath(exp, dir) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// ProviderPathsExist reports whether a provider has paths on disk, or paths
// that loading rejects. The uv cache directory is resolved first, so
// UV_CACHE_DIR counts even when the default directory is absent.
func ProviderPathsExist(name string, p Provider) bool {
	// A relative path stays visible so loading reports it; whether such a
	// directory exists depends on where bilgie runs.
	if p.PathError() != nil {
		return true
	}
	if name == uvProvider {
		dir, err := ResolveUVCacheDir(p.Paths)
		if err != nil {
			return true
		}
		_, statErr := os.Stat(dir)
		return statErr == nil
	}
	return PathsExist(p.Paths)
}

// RequireAbsoluteOrTilde rejects a path that is neither absolute nor
// home-relative: it would resolve against whatever directory bilgie runs in,
// and the protected-path checks compare the spelled path.
func RequireAbsoluteOrTilde(path string) error {
	if IsAbsPortable(path) || path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return nil
	}
	return fmt.Errorf("path %q must be absolute or start with ~/", path)
}

// PathError reports the first path that is neither absolute nor home-relative
// on this OS, or nil. Such a path would resolve against whatever
// directory bilgie runs in.
func (p Provider) PathError() error {
	for _, path := range p.Paths {
		if err := RequireAbsoluteOrTilde(path); err != nil {
			return err
		}
		expanded, err := ExpandTilde(path)
		if err != nil {
			return fmt.Errorf("expand %q: %w", path, err)
		}
		// Portable spellings such as \x or C:/x pass on another OS but stay
		// relative to the working directory on this one.
		if !filepath.IsAbs(expanded) {
			return fmt.Errorf("path %q must be absolute or start with ~/", path)
		}
	}
	return nil
}
