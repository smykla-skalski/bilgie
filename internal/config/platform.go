package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Operating systems with their own default providers.
const (
	OSDarwin  = "darwin"
	OSLinux   = "linux"
	OSWindows = "windows"
)

// Platform is the OS and environment the default providers are built for.
// It is injectable so every OS is testable from any host. Paths below Home
// are written as ~/... so a saved config follows the user between machines.
type Platform struct {
	OS           string
	Home         string
	XDGCacheHome string
	XDGDataHome  string
	LocalAppData string
	TempDir      string
	// SystemTempDir is the fixed system-wide temp dir, which differs from
	// TempDir when TMPDIR is unset or per-user. Empty where there is none.
	SystemTempDir string
}

// SystemTempDirFor is the fixed system temp dir of goos: /private/tmp on
// macOS, /tmp on other Unixes and none on Windows.
func SystemTempDirFor(goos string) string {
	switch goos {
	case OSDarwin:
		return "/private/tmp"
	case OSWindows:
		return ""
	default:
		return "/tmp"
	}
}

// CurrentPlatform describes the running OS and environment.
func CurrentPlatform() Platform {
	home, _ := os.UserHomeDir()
	return Platform{
		OS:           runtime.GOOS,
		Home:         home,
		XDGCacheHome: os.Getenv("XDG_CACHE_HOME"),
		XDGDataHome:  os.Getenv("XDG_DATA_HOME"),
		LocalAppData: os.Getenv("LOCALAPPDATA"),
		TempDir:      os.TempDir(),

		SystemTempDir: SystemTempDirFor(runtime.GOOS),
	}
}

func (p Platform) home(rel string) string {
	return "~/" + rel
}

// cache spells a path below the OS cache root: ~/Library/Caches on macOS,
// $XDG_CACHE_HOME or ~/.cache on Linux, %LOCALAPPDATA% on Windows.
func (p Platform) cache(rel string) string {
	switch p.OS {
	case OSDarwin:
		return p.home("Library/Caches/" + rel)
	case OSWindows:
		return p.under(p.LocalAppData, "AppData/Local", rel)
	default:
		return p.under(p.XDGCacheHome, ".cache", rel)
	}
}

// data spells a path below the per-user data root: $XDG_DATA_HOME or
// ~/.local/share on Linux, always ~/.local/share on macOS because the launchd
// agent does not inherit the shell's XDG variables, %LOCALAPPDATA% on Windows.
func (p Platform) data(rel string) string {
	switch p.OS {
	case OSWindows:
		return p.under(p.LocalAppData, "AppData/Local", rel)
	case OSDarwin:
		return p.home(".local/share/" + rel)
	default:
		return p.under(p.XDGDataHome, ".local/share", rel)
	}
}

// under joins rel to root, or to the home-relative fallback when root is
// unset. A root inside the home directory collapses to the ~/ spelling.
func (p Platform) under(root, fallback, rel string) string {
	if root == "" || !IsAbsPortable(root) {
		return p.home(fallback + "/" + rel)
	}
	if sub, ok := cutDir(root, p.Home, p.OS == OSWindows); ok {
		return p.home(strings.TrimPrefix(sub+"/"+rel, "/"))
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// cutDir returns the slash-separated remainder of path below dir.
func cutDir(path, dir string, fold bool) (string, bool) {
	path = strings.TrimRight(strings.ReplaceAll(path, `\`, "/"), "/")
	dir = strings.TrimRight(strings.ReplaceAll(dir, `\`, "/"), "/")
	if dir == "" {
		return "", false
	}
	p, d := path, dir
	if fold {
		p, d = strings.ToLower(p), strings.ToLower(d)
	}
	if p == d {
		return "", true
	}
	if strings.HasPrefix(p, d+"/") {
		return path[len(dir)+1:], true
	}
	return "", false
}

// tempGlobs spells pattern below the OS temp dir and the fixed system temp
// dir, once per distinct directory after symlinks are resolved.
func (p Platform) tempGlobs(pattern string) []string {
	dir := p.TempDir
	if !IsAbsPortable(dir) {
		dir = "/tmp"
	}
	globs, seen := make([]string, 0, 2), make([]string, 0, 2)
	for _, d := range []string{dir, p.SystemTempDir} {
		if d == "" {
			continue
		}
		resolved := resolvedDir(d)
		if slices.Contains(seen, resolved) {
			continue
		}
		seen = append(seen, resolved)
		globs = append(globs, filepath.Join(escapeGlob(d), pattern))
	}
	return globs
}

// resolvedDir follows symlinks when dir exists here, else only cleans it.
func resolvedDir(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return filepath.Clean(dir)
}

// escapeGlob wraps glob metacharacters in a bracket class, the one escape
// that works with both separators, so a temp dir such as C:\Users\a [PC]\Temp
// matches itself.
func escapeGlob(dir string) string {
	return strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(dir)
}

func hasDrive(path string) bool {
	return len(path) >= 2 && path[1] == ':' &&
		(path[0] >= 'a' && path[0] <= 'z' || path[0] >= 'A' && path[0] <= 'Z')
}

// IsAbsPortable reports whether path is absolute on any supported OS, so a
// config written on one OS validates on another.
func IsAbsPortable(path string) bool {
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) || filepath.IsAbs(path) {
		return true
	}
	return hasDrive(path) && len(path) > 2 && (path[2] == '/' || path[2] == '\\')
}

// providerOSes lists the OSes a built-in provider applies to. Providers not
// listed apply everywhere.
var providerOSes = map[string][]string{
	"homebrew":          {OSDarwin},
	"xcode-deriveddata": {OSDarwin},
	"xcode-archives":    {OSDarwin},
	"ios-simulator":     {OSDarwin},
	"vscode-shipit":     {OSDarwin},
	"lima":              {OSDarwin, OSLinux},
	"gh":                {OSDarwin, OSLinux},
	"sail-e2e-dirs":     {OSDarwin},
	"app-updaters":      {OSDarwin},
	"copilot":           {OSDarwin},
	"golangci-lint":     {OSDarwin},
	"node-gyp":          {OSDarwin},
}

// AppliesOn reports whether the named provider applies on goos. Custom
// providers apply everywhere.
func AppliesOn(name, goos string) bool {
	oses, ok := providerOSes[name]
	if !ok {
		return true
	}
	return slices.Contains(oses, goos)
}

// BuiltinProtectedRoots returns the absolute locations under home that auto
// never deletes from, whichever provider points at them.
func BuiltinProtectedRoots(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, "Downloads"),
		filepath.Join(home, ".local", "share", "opencode"),
		filepath.Join(home, ".config", "opencode"),
		filepath.Join(home, ".opencode"),
		filepath.Join(home, ".cache", "opencode"),
		filepath.Join(home, "Library", "Caches", "opencode"),
		filepath.Join(home, "Library", "Developer", "Xcode", "Archives"),
	}
}
