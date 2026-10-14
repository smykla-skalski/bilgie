package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/smykla-skalski/bilgie/internal/cache"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

const miseInstallsDir = "installs"

// MiseProvider cleans mise through its own `mise prune`, which decides what
// is unused: only versions no tracked config references. bilgie lists what a
// dry-run reports, refuses to act while mise installs, upgrades, prunes or switches tools,
// then lets mise delete. Downloads and cache files go by age. The plugin git
// clones below the data directory are never touched, so it enforces the
// protected paths itself and `auto` skips the generic checkout scan.
type MiseProvider struct {
	*BaseProvider
	cleanCmd string
	cmdArgs  []string
	timeout  time.Duration
	home     string

	procLines func(ctx context.Context) ([]string, error)

	mu        sync.Mutex
	protected []string
}

// NewMiseProvider creates the mise provider. clean_cmd must be a mise prune
// command; it defaults to "mise prune".
func NewMiseProvider(name string, cfg config.Provider) (*MiseProvider, error) {
	if strings.TrimSpace(cfg.CleanCmd) == "" {
		cfg.CleanCmd = "mise prune"
	}
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}
	args, err := shellquote.Split(cfg.CleanCmd)
	if err != nil {
		return nil, fmt.Errorf("invalid clean_cmd: %w", err)
	}
	if len(args) < 2 || args[1] != "prune" {
		return nil, fmt.Errorf("clean_cmd must be a mise prune command, got %q", cfg.CleanCmd)
	}
	for _, a := range args[2:] {
		if strings.HasPrefix(a, "-") {
			return nil, fmt.Errorf("clean_cmd takes only tool names after prune, got %q", a)
		}
	}
	timeout, err := parseCleanTimeout(cfg.CleanTimeout)
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	p := &MiseProvider{
		BaseProvider: base,
		cleanCmd:     cfg.CleanCmd,
		cmdArgs:      args,
		timeout:      timeout,
		home:         home,
		procLines:    processLister(nil),
	}
	p.protected = expandProtected(config.DefaultProtected(), home)
	p.protected = append(p.protected, config.BuiltinProtectedRoots(home)...)
	return p, nil
}

// SetProtected implements ProtectionAware. The paths add to the built-in ones.
func (p *MiseProvider) SetProtected(paths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, path := range paths {
		if path = filepath.Clean(path); !slices.Contains(p.protected, path) {
			p.protected = append(p.protected, path)
		}
	}
}

// Available reports whether the mise executable is on PATH.
func (p *MiseProvider) Available() bool {
	_, err := exec.LookPath(p.cmdArgs[0])
	return err == nil
}

// prunable is one installed tool version that mise reports as unused.
type prunable struct {
	label string
	path  string
	extra []string
	size  int64
}

// Clean implements Provider. Smart and full mode do the same: mise prune
// plus the age trim of downloads and cache files.
func (p *MiseProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	if reason := p.guardReason(); reason != "" {
		return skipResult(reason), nil
	}
	if reason := p.busyNow(ctx); reason != "" {
		return skipResult(reason), nil
	}

	list, reason := p.listPrunable(ctx)
	if reason != "" {
		return skipResult(reason), nil
	}

	var (
		res CleanResult
		out strings.Builder
	)
	if opts.DryRun {
		for _, v := range list {
			fmt.Fprintf(&out, "would prune: %s (%s)\n", v.label, sizeText(v.size))
			res.BytesCleaned += v.size
			res.Entries = append(res.Entries, Entry{Path: v.path, Size: v.size, Detail: v.label})
		}
	} else if len(list) > 0 {
		if reason := p.busyNow(ctx); reason != "" {
			return skipResult(reason), nil
		}
		pruneErr := p.runPrune(ctx)
		p.account(list, &res, &out)
		if pruneErr != nil {
			res.Output = strings.TrimSpace(out.String())
			return res, pruneErr
		}
	}
	if len(list) == 0 {
		out.WriteString("no unused tool versions\n")
	}

	trim, err := p.trimFiles(ctx, opts.DryRun, list)
	if err != nil {
		res.Output = strings.TrimSpace(out.String())
		return res, err
	}
	for _, f := range trim.Removed {
		if opts.DryRun {
			fmt.Fprintf(&out, "would delete: %s (%s)\n", f.Path, size.FormatSize(f.Size))
		}
	}
	if !opts.DryRun && trim.DeletedCount > 0 {
		fmt.Fprintf(&out, "deleted %d download/cache files\n", trim.DeletedCount)
	}
	res.BytesCleaned += trim.FreedBytes
	res.FilesDeleted += trim.DeletedCount
	res.Entries = append(res.Entries, entriesFromFiles(trim.Removed)...)
	res.Output = strings.TrimSpace(out.String())
	return res, nil
}

// account reports which listed versions are gone after a prune run. A
// version without a known path cannot be verified and is not counted.
func (p *MiseProvider) account(list []prunable, res *CleanResult, out *strings.Builder) {
	for _, v := range list {
		if v.path == "" {
			fmt.Fprintf(out, "unverified: %s (not found below installs)\n", v.label)
			continue
		}
		if _, err := os.Lstat(v.path); err == nil {
			fmt.Fprintf(out, "kept: %s (still in use)\n", v.label)
			res.SkippedEntries++
			continue
		}
		fmt.Fprintf(out, "pruned: %s (%s)\n", v.label, sizeText(v.size))
		res.BytesCleaned += v.size
		res.FilesDeleted++
		res.Entries = append(res.Entries, Entry{Path: v.path, Size: v.size, Detail: v.label})
	}
}

func skipResult(reason string) CleanResult {
	return CleanResult{SkipReason: reason, Output: "skipped: " + reason}
}

func sizeText(n int64) string {
	if n <= 0 {
		return "size unknown"
	}
	return size.FormatSize(n)
}

// guardReason refuses a mise directory that is, lies inside or contains a
// protected location.
func (p *MiseProvider) guardReason() string {
	p.mu.Lock()
	protected := slices.Clone(p.protected)
	p.mu.Unlock()
	for _, path := range p.paths {
		for _, cand := range pathSpellings(path) {
			if lexicallyProtected(cand) {
				return "protected path " + path
			}
			for _, root := range protected {
				for _, spelling := range rootSpellings(root) {
					if pathWithin(cand, spelling) || pathWithin(spelling, cand) {
						return "protected path " + root
					}
				}
			}
		}
	}
	return ""
}

func pathSpellings(path string) []string {
	out := []string{filepath.Clean(path)}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != out[0] {
		out = append(out, resolved)
	}
	return out
}

// miseMutatingCommands are the mise subcommands (and their aliases) that
// change installed tools while they run. Any other mise process, such as
// "mise dev" or a mise-managed binary, leaves the installs alone.
var miseMutatingCommands = map[string]bool{
	"install": true, "i": true,
	"upgrade": true, "up": true,
	"prune": true,
	"use":   true, "u": true,
}

// miseValueFlags are the global flags that take a separate value argument.
var miseValueFlags = map[string]bool{
	"-C": true, "--cd": true,
	"-E": true, "--env": true,
	"-j": true, "--jobs": true,
	"--output": true,
}

// busyNow reports why mise cannot be pruned right now: a mise process
// running install, upgrade, prune or use. A failed process listing counts as
// busy.
func (p *MiseProvider) busyNow(ctx context.Context) string {
	lines, err := p.procLines(ctx)
	if err != nil {
		return "cannot list processes: " + err.Error()
	}
	for _, line := range lines {
		if cmd := miseMutatingCommand(line); cmd != "" {
			return "mise " + cmd + " is running"
		}
	}
	return ""
}

// miseMutatingCommand returns the mutating subcommand of a mise command line,
// or "" when the process is not mise or runs another subcommand.
func miseMutatingCommand(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 || matchProcess(fields[0], []string{"mise"}) == "" {
		return ""
	}
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		switch {
		case miseValueFlags[f]:
			i++
		case strings.HasPrefix(f, "-"):
		case miseMutatingCommands[f]:
			return f
		default:
			return ""
		}
	}
	return ""
}

func (p *MiseProvider) installRoots() []string {
	var roots []string
	for _, path := range p.paths {
		for _, s := range pathSpellings(path) {
			roots = append(roots, filepath.Join(s, miseInstallsDir))
		}
	}
	return roots
}

var (
	ansiEscape  = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	pruneAtLine = regexp.MustCompile(`^(?:mise\s+)?(?:would prune:?\s+)?([^\s@/\\]+)@(\S+)$`)
)

// runMise runs the configured prune command with extra flags and returns
// stdout. Stderr is kept apart so a warning never reads as a listing.
func (p *MiseProvider) runMise(ctx context.Context, extra ...string) (stdout, stderr string, err error) {
	cmdCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	args := append(slices.Clone(p.cmdArgs[1:]), extra...)
	cmd := exec.CommandContext(cmdCtx, p.cmdArgs[0], args...)
	osshim.KillTreeOnCancel(cmd)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.WaitDelay = cleanWaitDelay
	err = cmd.Run()
	if err != nil && ctx.Err() == nil && errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", p.timeout)
	}
	return so.String(), strings.TrimSpace(se.String()), err
}

func (p *MiseProvider) runPrune(ctx context.Context) error {
	_, stderr, err := p.runMise(ctx, "--yes")
	if err != nil {
		if stderr != "" {
			return fmt.Errorf("%s --yes: %w: %s", p.cleanCmd, err, firstLine(stderr))
		}
		return fmt.Errorf("%s --yes: %w", p.cleanCmd, err)
	}
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// listPrunable asks mise what it would remove. A failure, a timeout or any
// line it cannot read yields a skip reason, never a guess.
func (p *MiseProvider) listPrunable(ctx context.Context) (list []prunable, skipReason string) {
	stdout, stderr, err := p.runMise(ctx, "--dry-run")
	if err != nil {
		if ctx.Err() != nil {
			return nil, "cancelled"
		}
		reason := fmt.Sprintf("%s --dry-run failed: %v", p.cleanCmd, err)
		if stderr != "" {
			reason += ": " + firstLine(stderr)
		}
		return nil, reason
	}
	st := &listing{items: map[string]*prunable{}}
	bad := p.parseStream(st, stdout, false)
	if bad == "" {
		bad = p.parseStream(st, stderr, true)
	}
	if bad != "" {
		return nil, fmt.Sprintf("cannot parse %s --dry-run output: %q", p.cleanCmd, bad)
	}
	return p.finish(ctx, st), ""
}

// listing collects the versions one dry-run reports, in output order.
type listing struct {
	items map[string]*prunable
	order []*prunable
}

func (l *listing) get(label string) *prunable {
	if v, ok := l.items[label]; ok {
		return v
	}
	v := &prunable{label: label}
	l.items[label] = v
	l.order = append(l.order, v)
	return v
}

func (p *MiseProvider) finish(ctx context.Context, st *listing) []prunable {
	out := make([]prunable, 0, len(st.order))
	for _, item := range st.order {
		v := *item
		for _, dir := range append([]string{v.path}, v.extra...) {
			if dir == "" {
				continue
			}
			if res, err := cache.CalculateSizeContext(ctx, []string{dir}); err == nil {
				v.size += res.Size
			}
		}
		out = append(out, v)
	}
	return out
}

// parseStream feeds every line of one output stream into st and returns the
// first line it cannot accept. A lenient stream (stderr, which carries
// warnings) ignores lines of unknown shape, but a line that looks like a
// dry-run line and does not parse is always an error.
func (p *MiseProvider) parseStream(st *listing, text string, lenient bool) string {
	for raw := range strings.SplitSeq(text, "\n") {
		line := strings.TrimSpace(ansiEscape.ReplaceAllString(raw, ""))
		if line == "" {
			continue
		}
		known, ok := p.parseOutputLine(st, line)
		switch {
		case ok:
		case known || !lenient:
			return line
		}
	}
	return ""
}

// parseOutputLine reads one line of mise's dry-run report, for example:
//
//	mise kubectl@1.37.0 is prunable: kubectl is required at 1.34.3 by ...
//	mise kubectl@1.37.0 [dryrun]   remove ~/.local/share/mise/installs/kubectl/1.37.0, ~/Library/Caches/mise/kubectl/1.37.0
//
// known says the line has the shape of a dry-run line, ok that it parsed.
func (p *MiseProvider) parseOutputLine(st *listing, line string) (known, ok bool) {
	s := strings.TrimPrefix(line, "mise ")
	if strings.HasPrefix(s, "pruned configuration links") {
		return true, true
	}
	if idx := strings.Index(s, " is prunable:"); idx > 0 {
		label := s[:idx]
		if !validLabel(label) {
			return true, false
		}
		st.get(label)
		return true, true
	}
	if idx := strings.Index(s, " [dryrun]"); idx > 0 {
		label := s[:idx]
		rest := strings.TrimSpace(s[idx+len(" [dryrun]"):])
		if !validLabel(label) || st.items[label] == nil {
			return true, false
		}
		switch {
		case rest == "uninstall", strings.HasPrefix(rest, "✓"):
			return true, true
		case strings.HasPrefix(rest, "remove "):
			return true, p.parseRemove(st.items[label], strings.TrimPrefix(rest, "remove "))
		}
		return true, false
	}
	if v, legacy := p.parseLine(line); legacy {
		cur := st.get(v.label)
		if cur.path == "" {
			cur.path = v.path
		}
		return false, true
	}
	return false, false
}

// validLabel accepts tool@version where the tool may hold ':', '@' and '/'
// (npm:@redocly/cli@2.46.1); the version follows the last '@'.
func validLabel(label string) bool {
	at := strings.LastIndex(label, "@")
	if at <= 0 || at == len(label)-1 || strings.ContainsAny(label, " \t") {
		return false
	}
	version := label[at+1:]
	return version != "." && version != ".." && !strings.ContainsAny(version, `/\`)
}

// parseRemove reads the comma-separated paths of a remove line. Exactly one
// must be the install directory <mise dir>/installs/<dir>/<version>; the
// others must be cache directories <...>/mise/<tool>/<version>. Anything
// else fails.
func (p *MiseProvider) parseRemove(v *prunable, list string) bool {
	version := v.label[strings.LastIndex(v.label, "@")+1:]
	for raw := range strings.SplitSeq(list, ", ") {
		target := strings.TrimSpace(raw)
		if strings.HasPrefix(target, "~/") && p.home != "" {
			target = filepath.Join(p.home, target[2:])
		}
		target = filepath.Clean(filepath.FromSlash(filepath.ToSlash(target)))
		if !filepath.IsAbs(target) || filepath.Base(target) != version {
			return false
		}
		if inst, ok := p.installPath(target); ok {
			if v.path != "" && v.path != inst {
				return false
			}
			v.path = inst
			continue
		}
		if filepath.Base(filepath.Dir(filepath.Dir(target))) != "mise" {
			return false
		}
		v.extra = append(v.extra, target)
	}
	return v.path != ""
}

// installPath reports whether target is <installs>/<dir>/<version> below a
// configured mise directory.
func (p *MiseProvider) installPath(target string) (string, bool) {
	for _, root := range p.installRoots() {
		if !pathWithin(target, root) {
			continue
		}
		rel, err := filepath.Rel(root, target)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) == 2 && !slices.Contains(parts, "..") && !slices.Contains(parts, ".") {
			return target, true
		}
	}
	return "", false
}

func (p *MiseProvider) parseLine(line string) (prunable, bool) {
	if rest, ok := strings.CutPrefix(strings.TrimPrefix(line, "mise "), "rm -rf "); ok {
		target := strings.Trim(strings.TrimSpace(rest), `"'`)
		if strings.HasPrefix(target, "~/") && p.home != "" {
			target = filepath.Join(p.home, target[2:])
		}
		target = filepath.Clean(filepath.FromSlash(filepath.ToSlash(target)))
		for _, root := range p.installRoots() {
			rel, err := filepath.Rel(root, target)
			if err != nil || !pathWithin(target, root) {
				continue
			}
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) != 2 || slices.Contains(parts, "..") || slices.Contains(parts, ".") {
				return prunable{}, false
			}
			return prunable{label: parts[0] + "@" + parts[1], path: target}, true
		}
		return prunable{}, false
	}
	m := pruneAtLine.FindStringSubmatch(line)
	if m == nil {
		return prunable{}, false
	}
	if m[1] == "." || m[1] == ".." || m[2] == "." || m[2] == ".." || strings.ContainsAny(m[2], `/\`) {
		return prunable{}, false
	}
	v := prunable{label: m[1] + "@" + m[2]}
	for _, root := range p.installRoots() {
		cand := filepath.Join(root, m[1], m[2])
		if _, err := os.Lstat(cand); err == nil && pathWithin(cand, root) {
			v.path = cand
			break
		}
	}
	return v, true
}

// trimDirs lists where files are trimmed by age: the downloads directory of
// the first path (mise's data directory), and every further path (cache
// directories) as a whole. Installs, plugins and state are never trimmed by file.
func (p *MiseProvider) trimDirs() []string {
	var dirs []string
	for i, path := range p.paths {
		if i == 0 {
			dirs = append(dirs, filepath.Join(path, "downloads"))
		} else {
			dirs = append(dirs, path)
		}
	}
	return dirs
}

// trimFiles ages out downloads and cache files. A dry-run leaves out the
// directories the prune listing already counts: a real run finds them gone,
// so counting them again would over-report and skew the size budget.
func (p *MiseProvider) trimFiles(ctx context.Context, dryRun bool, list []prunable) (cache.TrimResult, error) {
	var existing []string
	for _, d := range p.trimDirs() {
		if info, err := os.Lstat(d); err == nil && info.IsDir() {
			existing = append(existing, d)
		}
	}
	if len(existing) == 0 {
		return cache.TrimResult{}, nil
	}
	opts := cache.TrimOptions{MaxSize: p.maxSize, MaxAge: p.maxAge, DryRun: dryRun}
	if dryRun {
		var listed []string
		for _, v := range list {
			listed = append(listed, v.path)
			listed = append(listed, v.extra...)
		}
		opts.Exclude = func(path string) bool {
			return slices.ContainsFunc(listed, func(d string) bool { return d != "" && pathWithin(path, d) })
		}
	}
	return cache.Trim(ctx, existing, opts)
}

var _ ProtectionAware = (*MiseProvider)(nil)
