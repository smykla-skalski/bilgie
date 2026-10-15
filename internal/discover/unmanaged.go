package discover

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

const (
	customMaxSize = "5G"
	customMinIdle = "30d"
	appleCachePfx = "com.apple."
	globChars     = "*?[\\"
	classLeaders  = "]^-"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func withDefaults(opts Options) Options {
	if opts.Load == nil {
		opts.Load = provider.LoadProvider
	}
	if opts.NewProvider == nil {
		opts.NewProvider = provider.NewProvider
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.UnmanagedMin <= 0 {
		opts.UnmanagedMin = auto.DefaultUnmanagedMin
	}
	if opts.UnmanagedBudget <= 0 {
		opts.UnmanagedBudget = auto.DefaultUnmanagedBudget
	}
	if opts.Roots == nil {
		opts.Roots = DefaultRoots(opts.Home)
	}
	return opts
}

// DefaultRoots are the directories the unmanaged scan reads: the roots auto
// already treats as holding untracked caches, plus ~/.cache, where tools put
// their cache on every OS.
func DefaultRoots(home string) []string {
	roots := auto.UnmanagedRoots(home)
	if home != "" {
		roots = append(roots, filepath.Join(home, ".cache"))
	}
	return roots
}

// unmanaged lists the large directories no provider covers and classifies
// each. A directory in a cache location is a regenerable suggestion with a
// ready dir-pattern entry; anywhere else it may hold state, so it is opt-in.
// A directory the protection checks reject, or cannot verify, is protected.
func unmanaged(ctx context.Context, opts Options, report *Report) []Candidate {
	scan := auto.ScanUnmanaged(ctx, auto.ScanOptions{
		Roots:    opts.Roots,
		Covered:  auto.CoveredPaths(opts.Cfg),
		Top:      opts.Unmanaged,
		MinBytes: opts.UnmanagedMin,
		Budget:   opts.UnmanagedBudget,
	})
	if scan.Incomplete {
		report.Incomplete = true
	}

	protected := auto.ProtectedPaths(opts.Cfg, opts.Home)
	taken := make(map[string]bool, len(opts.Cfg.Providers))
	for name := range opts.Cfg.Providers {
		taken[name] = true
	}

	out := make([]Candidate, 0, len(scan.Dirs))
	for _, dir := range scan.Dirs {
		c := Candidate{
			Name: dir.Path, Kind: KindUnmanaged, Paths: []string{dir.Path}, Present: true,
			Bytes: dir.Bytes, Size: size.FormatSize(dir.Bytes), Partial: dir.Partial,
		}
		if info, err := os.Stat(dir.Path); err == nil {
			mod := info.ModTime()
			c.Modified = &mod
			c.AgeSeconds = max(int64(opts.Now.Sub(mod).Seconds()), 0)
		}
		classifyDir(ctx, &c, opts, protected, taken)
		out = append(out, c)
	}
	return out
}

func classifyDir(ctx context.Context, c *Candidate, opts Options, protected []string, taken map[string]bool) {
	reason, err := auto.PathProtection(ctx, c.Paths[0], opts.Home, protected)
	switch {
	case err != nil:
		protect(c, "protection check cancelled: not verified")
	case reason != "":
		protect(c, reason)
	case isCacheDir(c.Paths[0]) && !isAppleOwned(c.Paths[0]):
		c.Tier = TierRegenerable
		c.Reason = "no provider covers it; it sits in a cache directory"
	case isAppleOwned(c.Paths[0]):
		c.Tier = TierOptIn
		c.Reason = "owned by macOS or an Apple app"
	default:
		c.Tier = TierOptIn
		c.Reason = "no provider covers it and it is outside a cache directory, so it may hold state"
	}
	c.Action = actionFor(c)
	if c.Tier == TierProtected {
		return
	}
	c.Name = customName(c.Paths[0], taken)
	def, ok := customProvider(c.Paths[0], opts.Home)
	if !ok {
		c.Action = ActionNone
		c.Reason += "; its name has glob characters, so no dir-pattern entry can match it exactly"
		return
	}
	c.Define = &def
	taken[c.Name] = true
}

// isCacheDir reports whether dir is a direct child of a cache location.
func isCacheDir(dir string) bool {
	parent := filepath.Dir(dir)
	base := strings.ToLower(filepath.Base(parent))
	if base == "caches" || base == ".cache" {
		return true
	}
	xdg := os.Getenv("XDG_CACHE_HOME")
	return filepath.IsAbs(xdg) && filepath.Clean(xdg) == parent
}

func isAppleOwned(dir string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.Base(dir)), appleCachePfx)
}

// customName derives a provider name from a directory that is not already
// taken: custom-<slug>, numbered when two directories slug alike.
func customName(dir string, taken map[string]bool) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(filepath.Base(dir)), "-"), "-")
	if slug == "" {
		slug = "dir"
	}
	name := "custom-" + slug
	for n := 2; taken[name]; n++ {
		name = "custom-" + slug + "-" + strconv.Itoa(n)
	}
	return name
}

// customProvider builds the dir-pattern entry that removes dir once it has
// been idle for customMinIdle. The pattern wraps the first character of the
// name in a bracket class, the only way to make an exact path a valid glob;
// names with glob characters get no entry because no escape is portable.
func customProvider(dir, home string) (config.Provider, bool) {
	base := filepath.Base(dir)
	first, width := utf8.DecodeRuneInString(base)
	if base == "" || first == utf8.RuneError || strings.ContainsAny(base, globChars) || strings.ContainsRune(classLeaders, first) {
		return config.Provider{}, false
	}
	pattern := filepath.Join(filepath.Dir(dir), "["+string(first)+"]"+base[width:])
	if rel, ok := belowHome(pattern, home); ok {
		pattern = "~/" + filepath.ToSlash(rel)
	}
	return config.Provider{
		Enabled: true,
		Type:    config.TypeDirPattern,
		Paths:   []string{pattern},
		MaxSize: customMaxSize,
		MinIdle: customMinIdle,
	}, true
}

// belowHome returns path relative to home, trying the symlink-resolved
// spelling of home too, because the scan reports resolved paths.
func belowHome(path, home string) (string, bool) {
	if home == "" {
		return "", false
	}
	homes := []string{home}
	if resolved, err := filepath.EvalSymlinks(home); err == nil && resolved != home {
		homes = append(homes, resolved)
	}
	for _, h := range homes {
		rel, err := filepath.Rel(h, path)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return rel, true
		}
	}
	return "", false
}
