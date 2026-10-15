package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/cache"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

const (
	uvEnvDirPrefix = "environments-v"
	uvArchiveDir   = "archive-v0"
	uvVenvMarker   = "pyvenv.cfg"
)

// uvEnv is one cached environment: a symlink under environments-v*/<bucket>
// that names a virtual environment in archive-v0. uvx runs a server from such
// an environment, and the server imports its modules from it for as long as it
// lives, so the directory must stay while any process uses it.
type uvEnv struct {
	link    string
	target  string
	payload string
	size    int64
}

// uvxEvidence is everything the sweep knows about running processes.
type uvxEvidence struct {
	procs []osshim.Process
	envs  []osshim.ProcessEnv
	open  map[string]bool
	// resolver and resolved hold the paths processes name with every symlink
	// resolved, so any alias of an environment compares equal to it.
	resolver *pathResolver
	resolved []string
	seen     map[string]bool
	// texts holds every command line and environment, normalized for path
	// matching.
	texts []string
}

// sweepEnvironments is the clean while uvx servers hold uv's cache lock. It
// never runs uv: `uv cache prune` would wait on that lock and `--force`
// removes the environments the servers run from. Instead it removes the
// cached environments that no process uses, the part of the cache uv itself
// would drop as stale, and leaves everything else to the next run without a
// uvx server. An environment is kept when any process names it on its command
// line or in its environment, has a file in it open, mapped or as its working
// directory, or when the sweep cannot tell which environment a uvx server
// runs from.
func (p *UVProvider) sweepEnvironments(ctx context.Context, opts CleanOptions, aged []osshim.Process, seen map[int]bool) (CleanResult, error) {
	now := time.Now()
	running := describeAged(aged, now)
	envs, err := p.cachedEnvironments()
	if err != nil {
		return skipResult(fmt.Sprintf("%s; cannot list cached environments: %v", running, err)), nil
	}
	if len(envs) == 0 {
		return skipResult(running + "; no cached environment to remove, uv cache prune waits for it"), nil
	}

	ev, err := p.gatherEvidence(ctx, envs)
	if err != nil {
		return skipResult(fmt.Sprintf("%s; cannot tell which cached environments it uses: %v", running, err)), nil
	}
	if pid := newUVProcess(seen, ev.procs, p.busy.processes); pid != 0 {
		return skipResult(fmt.Sprintf("%s; uv started (pid %d) while checking", running, pid)), nil
	}
	if reason := p.unattributed(aged, ev); reason != "" {
		return skipResult(running + "; " + reason), nil
	}

	unused, used := p.split(envs, ev)
	if len(unused) == 0 {
		return skipResult(fmt.Sprintf("%s; all %d cached environments are in use", running, len(used))), nil
	}
	var total int64
	for i := range unused {
		res, sizeErr := cache.CalculateSizeContext(ctx, []string{unused[i].payload})
		if sizeErr != nil {
			return CleanResult{}, sizeErr
		}
		unused[i].size = res.Size
		total += res.Size
	}

	if opts.DryRun {
		return CleanResult{
			Entries:      envEntries(unused),
			BytesCleaned: total,
			Output: fmt.Sprintf("would remove %d unused cached environments (%s) and keep %d in use; %s, so uv cache prune is not run\n",
				len(unused), size.FormatSize(total), len(used), running),
		}, nil
	}
	return p.removeEnvironments(ctx, unused, len(used), seen, running)
}

// removeEnvironments unpublishes each unused environment by removing its
// symlink, so no uv that starts now can pick it, then looks again for a
// process that uses it, and only then deletes the directory. A use that
// appeared in between puts the symlink back.
func (p *UVProvider) removeEnvironments(ctx context.Context, unused []uvEnv, inUse int, seen map[int]bool, running string) (CleanResult, error) {
	var unpublished []uvEnv
	for _, env := range unused {
		if err := os.Remove(env.link); err != nil && !errors.Is(err, fs.ErrNotExist) {
			warnings := p.restore(unpublished)
			return CleanResult{SkipReason: fmt.Sprintf("%s; cannot unpublish %s: %v", running, env.link, err), Warnings: warnings,
				Output: "skipped: cannot unpublish " + env.link}, nil
		}
		unpublished = append(unpublished, env)
	}

	after, err := p.gatherEvidence(ctx, unpublished)
	if err != nil || ctx.Err() != nil {
		warnings := p.restore(unpublished)
		if err == nil {
			err = ctx.Err()
		}
		return CleanResult{SkipReason: fmt.Sprintf("%s; cannot recheck the cached environments: %v", running, err), Warnings: warnings,
			Output: "skipped: cannot recheck the cached environments"}, nil
	}
	if pid := newUVProcess(seen, after.procs, p.busy.processes); pid != 0 {
		warnings := p.restore(unpublished)
		reason := fmt.Sprintf("%s; uv started (pid %d) while sweeping", running, pid)
		return CleanResult{SkipReason: reason, Warnings: warnings, Output: "skipped: " + reason}, nil
	}

	var (
		removed  []uvEnv
		restored []uvEnv
		freed    int64
		failures []string
	)
	_, usedNow := p.split(unpublished, after)
	inUseNow := map[string]bool{}
	for _, env := range usedNow {
		inUseNow[env.payload] = true
	}
	for _, env := range unpublished {
		if inUseNow[env.payload] {
			restored = append(restored, env)
			continue
		}
		if err := os.RemoveAll(env.payload); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", env.payload, err))
			continue
		}
		removed = append(removed, env)
		freed += env.size
	}
	warnings := p.restore(restored)
	for _, f := range failures {
		warnings = append(warnings, "could not remove "+f)
	}

	out := fmt.Sprintf("%s; uv cache prune not run. Removed %d unused cached environments (%s), kept %d in use",
		running, len(removed), size.FormatSize(freed), inUse+len(restored))
	res := CleanResult{Output: out + "\n", Entries: envEntries(removed), BytesCleaned: freed, Warnings: warnings}
	if len(removed) == 0 && len(failures) == 0 {
		res.SkipReason = running + "; every cached environment is in use"
		res.Output = "skipped: " + res.SkipReason
	}
	return res, nil
}

// restore puts back the symlinks of environments that stay and returns a
// warning for each one it could not.
func (p *UVProvider) restore(envs []uvEnv) []string {
	var warnings []string
	for _, env := range envs {
		if err := os.Symlink(env.target, env.link); err != nil && !errors.Is(err, fs.ErrExist) {
			warnings = append(warnings, fmt.Sprintf("could not restore %s: %v", env.link, err))
		}
	}
	return warnings
}

// split divides envs into unused and used. Links that share a payload stand
// or fall together: the directory goes only when no link of it is in use.
func (p *UVProvider) split(envs []uvEnv, ev uvxEvidence) (unused, used []uvEnv) {
	inUse := map[string]bool{}
	for _, env := range envs {
		if p.referenced(env, ev) {
			inUse[env.payload] = true
		}
	}
	for _, env := range envs {
		if inUse[env.payload] {
			used = append(used, env)
		} else {
			unused = append(unused, env)
		}
	}
	return unused, used
}

func envEntries(envs []uvEnv) []Entry {
	entries := make([]Entry, len(envs))
	for i, env := range envs {
		entries[i] = Entry{Path: env.payload, Size: env.size, Detail: "uvx cached environment, no process uses it"}
	}
	return entries
}

// cachedEnvironments lists the environment symlinks the sweep recognizes: a
// symlink under environments-v*/<bucket> to a direct child of archive-v0 that
// holds a pyvenv.cfg. Any other layout is left alone.
func (p *UVProvider) cachedEnvironments() ([]uvEnv, error) {
	roots, err := os.ReadDir(p.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	archive := filepath.Join(p.dir, uvArchiveDir)
	resolvedArchive, archiveResolves := newPathResolver().resolve(archive)
	var envs []uvEnv
	for _, root := range roots {
		if !root.IsDir() || !strings.HasPrefix(root.Name(), uvEnvDirPrefix) {
			continue
		}
		buckets, err := os.ReadDir(filepath.Join(p.dir, root.Name()))
		if err != nil {
			return nil, err
		}
		for _, bucket := range buckets {
			if !bucket.IsDir() {
				continue
			}
			dir := filepath.Join(p.dir, root.Name(), bucket.Name())
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Type()&fs.ModeSymlink == 0 {
					continue
				}
				link := filepath.Join(dir, entry.Name())
				target, err := os.Readlink(link)
				if err != nil {
					continue
				}
				payload := target
				if !filepath.IsAbs(payload) {
					payload = filepath.Join(dir, payload)
				}
				payload = filepath.Clean(payload)
				if filepath.Dir(payload) != archive {
					parent, ok := newPathResolver().resolve(filepath.Dir(payload))
					if !archiveResolves || !ok || parent != resolvedArchive {
						continue
					}
					payload = filepath.Join(archive, filepath.Base(payload))
				}
				if _, err := os.Stat(filepath.Join(payload, uvVenvMarker)); err != nil {
					continue
				}
				envs = append(envs, uvEnv{link: link, target: target, payload: payload})
			}
		}
	}
	return envs, nil
}

// gatherEvidence reads the process table, every process environment and the
// open files under each environment. Any failure is an error: the caller then
// keeps every environment.
func (p *UVProvider) gatherEvidence(ctx context.Context, envs []uvEnv) (uvxEvidence, error) {
	ev := uvxEvidence{resolver: newPathResolver(), seen: map[string]bool{}}
	var err error
	if ev.procs, err = p.busy.listProcesses(ctx); err != nil {
		return ev, fmt.Errorf("list processes: %w", err)
	}
	if ev.envs, err = p.processEnvs(ctx); err != nil {
		return ev, fmt.Errorf("read process environments: %w", err)
	}
	payloads := make([]string, len(envs))
	for i, env := range envs {
		payloads[i] = env.payload
	}
	if ev.open, err = p.openUnder(ctx, payloads); err != nil {
		return ev, fmt.Errorf("list open files: %w", err)
	}
	for i := range ev.procs {
		ev.addText(ev.procs[i].CommandLine)
	}
	for i := range ev.envs {
		ev.addText(ev.envs[i].Text)
	}
	return ev, nil
}

func (ev *uvxEvidence) addText(text string) {
	ev.texts = append(ev.texts, normalizeText(text))
	for _, path := range ev.resolver.resolvedPaths(text) {
		if !ev.seen[path] {
			ev.seen[path] = true
			ev.resolved = append(ev.resolved, path)
		}
	}
}

// referenced reports whether any process uses the environment.
func (p *UVProvider) referenced(env uvEnv, ev uvxEvidence) bool {
	if ev.open[env.payload] {
		return true
	}
	for _, target := range p.envTargets(env, ev.resolver) {
		if anyWithin(ev.resolved, target) {
			return true
		}
	}
	needles := p.envNeedles(env)
	for _, text := range ev.texts {
		if mentionsAny(text, needles) {
			return true
		}
	}
	return false
}

// envNeedles are the spellings of an environment's directory and symlink a
// process may use, with the cache directory spelled both as configured and
// resolved (/var and /private/var on macOS).
func (p *UVProvider) envNeedles(env uvEnv) []string {
	rel, err := filepath.Rel(p.dir, env.payload)
	if err != nil {
		return []string{normalizeText(env.payload)}
	}
	relLink, linkErr := filepath.Rel(p.dir, env.link)
	var out []string
	for _, base := range pathSpellings(p.dir) {
		out = append(out, normalizeText(filepath.Join(base, rel)))
		if linkErr == nil {
			out = append(out, normalizeText(filepath.Join(base, relLink)))
		}
	}
	return out
}

// envTargets are the resolved spellings of an environment: its directory,
// and its link, which no longer resolves once the sweep has removed it.
func (p *UVProvider) envTargets(env uvEnv, resolver *pathResolver) []string {
	targets := []string{p.resolvedOrRaw(resolver, env.payload)}
	parent := p.resolvedOrRaw(resolver, filepath.Dir(env.link))
	return append(targets, normalizeText(filepath.Join(parent, filepath.Base(env.link))))
}

func (p *UVProvider) resolvedOrRaw(resolver *pathResolver, path string) string {
	if resolved, ok := resolver.resolve(path); ok {
		return normalizeText(resolved)
	}
	return normalizeText(path)
}

// anyWithin reports whether any of paths is target or lies inside it. The
// path must end there: an id that merely starts with target's last element
// is another directory.
func anyWithin(paths []string, target string) bool {
	for _, path := range paths {
		if strings.HasPrefix(path, target) &&
			(len(path) == len(target) || !idByte(path[len(target)])) {
			return true
		}
	}
	return false
}

// unattributed returns a reason when some aged uvx process has no descendant
// that visibly runs from this cache: its environment cannot be named, so
// none may be removed.
func (p *UVProvider) unattributed(aged []osshim.Process, ev uvxEvidence) string {
	prefixes := p.cachePrefixes()
	for i := range aged {
		if !p.runsFromCache(aged[i].PID, ev, prefixes) {
			return fmt.Sprintf("cannot tell which cached environment pid %d runs from, so none is removed", aged[i].PID)
		}
	}
	return ""
}

func procText(procs []osshim.Process, pid int) string {
	for i := range procs {
		if procs[i].PID == pid {
			return procs[i].CommandLine + " " + procs[i].Args
		}
	}
	return ""
}

// descendants returns the pids below root in the process tree.
func descendants(procs []osshim.Process, root int) []int {
	children := make(map[int][]int, len(procs))
	for i := range procs {
		children[procs[i].PPID] = append(children[procs[i].PPID], procs[i].PID)
	}
	var out []int
	seen := map[int]bool{root: true}
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			if child == 0 || seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

// newUVProcess returns the pid of a uv process in procs that the busy check
// did not list, or 0: one that started after the check may already have read
// an environment's symlink.
func newUVProcess(seen map[int]bool, procs []osshim.Process, tools []string) int {
	for i := range procs {
		if procs[i].PID != 0 && !seen[procs[i].PID] && matchProcess(procs[i].CommandLine, tools) != "" {
			return procs[i].PID
		}
	}
	return 0
}

// cacheUsers returns the servers among others that have a process below them
// running from this cache. A failure to read the evidence is an error.
func (p *UVProvider) cacheUsers(ctx context.Context, others []osshim.Process) ([]osshim.Process, error) {
	procs, err := p.busy.listProcesses(ctx)
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	envs, err := p.processEnvs(ctx)
	if err != nil {
		return nil, fmt.Errorf("read process environments: %w", err)
	}
	ev := uvxEvidence{procs: procs, envs: envs, resolver: newPathResolver()}
	prefixes := p.cachePrefixes()
	var users []osshim.Process
	for i := range others {
		if p.runsFromCache(others[i].PID, ev, prefixes) {
			users = append(users, others[i])
		}
	}
	return users, nil
}

// cachePrefixes are the spellings of the cache's archive directory, with a
// separator: a uvx environment lives there and uv puts it on the server's PATH.
func (p *UVProvider) cachePrefixes() []string {
	var prefixes []string
	for _, base := range pathSpellings(p.dir) {
		prefixes = append(prefixes, normalizeText(filepath.Join(base, uvArchiveDir)+string(filepath.Separator)))
	}
	return prefixes
}

// runsFromCache reports whether a process below pid names a path inside the
// cache's archive directory on its command line or in its environment,
// whichever alias it spells it with.
func (p *UVProvider) runsFromCache(pid int, ev uvxEvidence, prefixes []string) bool {
	envText := make(map[int]string, len(ev.envs))
	for i := range ev.envs {
		envText[ev.envs[i].PID] += " " + ev.envs[i].Text
	}
	archive := p.resolvedOrRaw(ev.resolver, filepath.Join(p.dir, uvArchiveDir)) + string(filepath.Separator)
	for _, id := range descendants(ev.procs, pid) {
		text := envText[id] + " " + procText(ev.procs, id)
		if mentionsAny(normalizeText(text), prefixes) {
			return true
		}
		for _, path := range ev.resolver.resolvedPaths(text) {
			if strings.HasPrefix(path, archive) {
				return true
			}
		}
	}
	return false
}

// normalizeText folds case where the file system does, so a path matches
// however a process spelled it.
func normalizeText(s string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(s)
	}
	return s
}

// mentionsAny reports whether text, normalized, names one of the paths,
// normalized. The path must end there: an id that merely starts with the
// path's last element is another directory.
func mentionsAny(text string, paths []string) bool {
	for _, path := range paths {
		for from := 0; ; {
			i := strings.Index(text[from:], path)
			if i < 0 {
				break
			}
			end := from + i + len(path)
			if strings.HasSuffix(path, string(filepath.Separator)) || end == len(text) || !idByte(text[end]) {
				return true
			}
			from = end
		}
	}
	return false
}

func idByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}
