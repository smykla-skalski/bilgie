package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

const (
	uvBinary          = "uv"
	uvDefaultCleanCmd = "uv cache clean"
	uvSmartCleanCmd   = "uv cache prune"
	uvLockName        = "uv.lock"
)

// UVProvider cleans the uv cache with uv itself. Files inside the cache
// reference each other (the wheels-v* pointers name archive-v0/<id>), so
// deleting any of them breaks the next install; only uv may remove them.
// bilgie never deletes below the cache directory, with one exception: while
// a long-running uvx server holds the cache lock, uv cannot run safely, and
// the provider removes only the cached environments no process uses (see
// sweepEnvironments). Smart mode runs `uv cache prune` (unused entries), full
// mode runs `uv cache clean`.
type UVProvider struct {
	*BaseProvider
	dir      string
	lookPath func(string) (string, error)
	cleanCmd string
	fullArgs []string
	timeout  time.Duration
	// uvxMinAge is how old a uvx server must be before it stops blocking uv.
	uvxMinAge time.Duration

	// processEnvs and openUnder read what running processes use; tests replace them.
	processEnvs func(context.Context) ([]osshim.ProcessEnv, error)
	openUnder   func(context.Context, []string) (map[string]bool, error)

	// lockPath is bilgie's own lock on running uv; empty uses the state dir.
	lockPath string

	mu        sync.Mutex
	protected []string
}

// NewUVProvider creates the uv provider. clean_cmd is optional and, when
// set, must be `uv cache clean`, `uv cache prune` or `uv cache prune --ci`;
// it replaces the full-mode command. paths must hold exactly one directory.
func NewUVProvider(name string, cfg config.Provider) (*UVProvider, error) {
	cmd := strings.TrimSpace(cfg.CleanCmd)
	if cmd == "" {
		cmd = uvDefaultCleanCmd
	}
	fullArgs, err := parseUVCleanCmd(cmd)
	if err != nil {
		return nil, err
	}
	timeout, err := parseCleanTimeout(cfg.CleanTimeout)
	if err != nil {
		return nil, err
	}
	uvxMinAge, err := config.ParseUvxMinAge(cfg.UvxMinAge)
	if err != nil {
		return nil, fmt.Errorf("parse uvx_min_age: %w", err)
	}
	dir, err := config.ResolveUVCacheDir(cfg.Paths)
	if err != nil {
		return nil, err
	}
	base, err := newBaseProviderWithPaths(name, cfg, []string{dir})
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	p := &UVProvider{
		BaseProvider: base,
		dir:          dir,
		lookPath:     exec.LookPath,
		cleanCmd:     cmd,
		fullArgs:     fullArgs,
		timeout:      timeout,
		uvxMinAge:    uvxMinAge,
		processEnvs:  osshim.ProcessEnvironments,
		openUnder:    osshim.OpenFilesUnder,
	}
	p.protected = expandProtected(config.DefaultProtected(), home)
	p.protected = append(p.protected, config.BuiltinProtectedRoots(home)...)
	return p, nil
}

func parseUVCleanCmd(cmd string) ([]string, error) {
	words, err := shellquote.Split(cmd)
	if err != nil {
		return nil, fmt.Errorf("invalid clean_cmd: %w", err)
	}
	ok := len(words) >= 3 && words[0] == uvBinary && words[1] == "cache"
	switch {
	case ok && words[2] == "clean":
		ok = len(words) == 3
	case ok && words[2] == "prune":
		ok = len(words) == 3 || (len(words) == 4 && words[3] == "--ci")
	default:
		ok = false
	}
	if !ok {
		return nil, fmt.Errorf(
			`clean_cmd must be "uv cache clean", "uv cache prune" or "uv cache prune --ci" `+
				`(bilgie runs uv itself and never deletes files in the uv cache), got %q; `+
				`remove clean_cmd to use the default`, cmd)
	}
	return words[1:], nil
}

// SetProtected implements ProtectionAware. The paths add to the built-in ones.
func (p *UVProvider) SetProtected(paths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, path := range paths {
		if path = filepath.Clean(path); !slices.Contains(p.protected, path) {
			p.protected = append(p.protected, path)
		}
	}
}

func (p *UVProvider) findUV() (string, bool) {
	bin, err := p.lookPath(uvBinary)
	return bin, err == nil
}

// Available is always true: a missing uv is reported as a skip with its
// reason on the provider's own line, not as a vanished provider.
func (p *UVProvider) Available() bool {
	return true
}

// Clean implements Provider.
func (p *UVProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	if reason := p.guardReason(); reason != "" {
		return skipResult(reason), nil
	}
	if reason := p.RootProblem(); reason != "" {
		return skipResult(reason), nil
	}
	if !opts.DryRun {
		release, reason := p.acquireLock()
		if reason != "" {
			return skipResult(reason), nil
		}
		defer release()
	}
	verdict := p.busy.classifyUV(ctx, p.uvxMinAge)
	if verdict.busy != "" && ctx.Err() == nil {
		return skipResult(verdict.busy), nil
	}
	if ctx.Err() == nil {
		aged := verdict.aged
		if len(verdict.others) > 0 {
			users, err := p.cacheUsers(ctx, verdict.others)
			if err != nil {
				return skipResult(fmt.Sprintf("cannot tell whether %s uses this cache: %v", describeAged(verdict.others, time.Now()), err)), nil
			}
			aged = append(slices.Clone(aged), users...)
		}
		if len(aged) > 0 {
			return p.sweepEnvironments(ctx, opts, aged, verdict.seen)
		}
	}
	bin, found := p.findUV()
	if !found {
		return skipResult("uv not found on PATH"), nil
	}

	args, display := p.fullArgs, p.cleanCmd
	if opts.Mode == CleanModeSmart && args[1] != "prune" {
		args, display = []string{"cache", "prune"}, uvSmartCleanCmd
	}

	if opts.DryRun {
		cacheSize := "size unknown"
		if current, sizeErr := p.CurrentSize(ctx); sizeErr == nil {
			cacheSize = size.FormatSize(current)
		}
		return CleanResult{
			Output: fmt.Sprintf("would run: %s (cache %s, uv has no dry-run)", display, cacheSize),
		}, nil
	}

	res, err := runMeasuredCleanEnv(ctx, p.name, uvCommand(bin, args), p.childEnv(), p.CurrentSize, p.timeout)
	if err != nil {
		if ctx.Err() != nil {
			return CleanResult{Output: res.Output}, ctx.Err()
		}
		failure := fmt.Sprintf("%s failed: %v", display, err)
		if line := firstLine(res.Output); line != "" {
			failure += ": " + line
		}
		if res.BytesCleaned > 0 {
			// uv removed entries before failing: the cache is half-cleaned, which
			// a skip would hide.
			return CleanResult{
				Output:       fmt.Sprintf("%s (uv freed %s before failing)\n", failure, sizeText(res.BytesCleaned)),
				BytesCleaned: res.BytesCleaned,
			}, errors.New(failure)
		}
		return skipResult(failure), nil
	}
	return res, nil
}

// acquireLock takes bilgie's own lock on running uv, so two bilgie runs never
// start uv together. uv's own cache lock does not cover the gap between the
// busy check and uv taking it.
func (p *UVProvider) acquireLock() (release func(), reason string) {
	lock := p.lockPath
	if lock == "" {
		dir, err := config.StateDir()
		if err == nil {
			err = os.MkdirAll(dir, 0o750)
		}
		if err != nil {
			return nil, "cannot take the uv lock: " + err.Error()
		}
		lock = filepath.Join(dir, uvLockName)
	}
	release, acquired, err := osshim.TryLock(lock)
	switch {
	case err != nil:
		return nil, "cannot take the uv lock: " + err.Error()
	case !acquired:
		return nil, "another bilgie run is cleaning uv"
	}
	return release, ""
}

// RootProblem implements RootGuarded. It refuses a non-empty directory that uv
// did not create: uv cache clean empties whatever directory it is given, and a
// uv cache always holds a CACHEDIR.TAG. A missing or empty directory has
// nothing to lose.
func (p *UVProvider) RootProblem() string {
	info, statErr := os.Stat(p.dir)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return ""
	case statErr != nil:
		return fmt.Sprintf("cannot verify %s is a uv cache: %v", p.dir, statErr)
	case !info.IsDir():
		return fmt.Sprintf("cannot verify %s is a uv cache: not a directory", p.dir)
	}
	entries, err := os.ReadDir(p.dir)
	switch {
	case err != nil:
		return fmt.Sprintf("cannot verify %s is a uv cache: %v", p.dir, err)
	case len(entries) == 0:
		return ""
	case !validCacheDirTag(filepath.Join(p.dir, "CACHEDIR.TAG")):
		return "not a uv cache (no valid CACHEDIR.TAG): " + p.dir +
			"; if a failed uv run removed the tag, delete the directory by hand"
	}
	return ""
}

// childEnv pins uv to the directory bilgie measured and drops UV_NO_CACHE,
// which would send uv to a throwaway directory.
func (p *UVProvider) childEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(key, "UV_CACHE_DIR") || strings.EqualFold(key, "UV_NO_CACHE") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "UV_CACHE_DIR="+p.dir)
}

// guardReason refuses a cache directory that is, lies inside or contains a
// protected location: uv cache clean empties whatever directory it is given.
func (p *UVProvider) guardReason() string {
	p.mu.Lock()
	protected := slices.Clone(p.protected)
	p.mu.Unlock()
	for _, cand := range pathSpellings(p.dir) {
		if lexicallyProtected(cand) {
			return "protected path " + p.dir
		}
		for _, root := range protected {
			for _, spelling := range rootSpellings(root) {
				if pathWithin(cand, spelling) || pathWithin(spelling, cand) {
					return "protected path " + root
				}
			}
		}
	}
	return ""
}

var (
	_ ProtectionAware = (*UVProvider)(nil)
	_ RootGuarded     = (*UVProvider)(nil)
)

// uvCommand ignores uv config files: a no-cache or cache-dir setting there
// would send uv to a directory other than the one bilgie measured.
func uvCommand(bin string, args []string) []string {
	return append(append([]string{bin}, args...), "--no-config")
}
