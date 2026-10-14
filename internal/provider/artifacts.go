package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// Project artifact defaults.
const (
	defaultArtifactMinIdle    = 7 * 24 * time.Hour
	defaultCriticalMinIdle    = 2 * 24 * time.Hour
	defaultEmergencyMinIdle   = 24 * time.Hour
	defaultArtifactBudget     = 10 * time.Second
	defaultArtifactPassBudget = 30 * time.Second
	openSnapshotTTL           = 2 * time.Second
	removalSnapshotTTL        = 250 * time.Millisecond
	defaultArtifactDepth      = 4
	openCheckTimeout          = time.Minute
	scanCacheTTL              = 2 * time.Minute
)

// ProjectArtifactsProvider removes whole build artifact directories (Rust
// target, node_modules, Python virtualenvs) of idle projects below the
// configured roots. A directory counts only when its marker file and the
// sibling project file both exist. Projects are cleaned oldest first and only
// as far as needed; an artifact is never removed in part.
type ProjectArtifactsProvider struct {
	*BaseProvider

	minIdle    time.Duration
	tierIdle   map[Pressure]time.Duration
	budget     time.Duration
	passBudget time.Duration
	maxDepth   int
	kinds      map[artifactKind]bool
	skipDirty  bool
	skipIfOpen bool
	home       string

	now       func() time.Time
	openCheck func(ctx context.Context, dir string) (bool, error)
	// openMany answers for many directories from one listing of open files;
	// when set it replaces the per-directory openCheck.
	openMany  func(ctx context.Context, dirs []string) (map[string]bool, error)
	git       gitRunner
	processes processLookup

	mu        sync.Mutex
	protected []string
	cached    *artifactScan
	cachedAt  time.Time
}

// NewProjectArtifactsProvider creates a provider from a project-artifacts config.
func NewProjectArtifactsProvider(name string, cfg config.Provider) (*ProjectArtifactsProvider, error) {
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}

	minIdle, err := durationOr(cfg.MinIdle, defaultArtifactMinIdle, "min_idle", config.ParseDuration)
	if err != nil {
		return nil, err
	}
	critical, err := durationOr(cfg.CriticalMinIdle, defaultCriticalMinIdle, "critical_min_idle", config.ParseDuration)
	if err != nil {
		return nil, err
	}
	emergency, err := durationOr(cfg.EmergencyMinIdle, defaultEmergencyMinIdle, "emergency_min_idle", config.ParseDuration)
	if err != nil {
		return nil, err
	}
	var tierIdle map[Pressure]time.Duration
	if boolOr(cfg.TierScaling, true) {
		tierIdle = map[Pressure]time.Duration{PressureCritical: critical, PressureEmergency: emergency}
	}
	budget, err := durationOr(cfg.ScanBudget, defaultArtifactBudget, "scan_budget", config.ParseBudget)
	if err != nil {
		return nil, err
	}
	passBudget, err := durationOr(cfg.PassBudget, defaultArtifactPassBudget, "pass_budget", config.ParseBudget)
	if err != nil {
		return nil, err
	}
	depth := cfg.MaxDepth
	switch {
	case depth < 0:
		return nil, fmt.Errorf("max_depth must be at least 1, got %d", depth)
	case depth > config.MaxProjectDepth:
		return nil, fmt.Errorf("max_depth must be at most %d, got %d", config.MaxProjectDepth, depth)
	case depth == 0:
		depth = defaultArtifactDepth
	}

	home, _ := os.UserHomeDir()
	p := &ProjectArtifactsProvider{
		BaseProvider: base,
		minIdle:      minIdle,
		tierIdle:     tierIdle,
		budget:       budget,
		passBudget:   passBudget,
		maxDepth:     depth,
		kinds: map[artifactKind]bool{
			kindRust:   boolOr(cfg.Rust, true),
			kindNode:   boolOr(cfg.Node, true),
			kindPython: boolOr(cfg.Python, false),
		},
		skipDirty:  boolOr(cfg.SkipIfDirty, true),
		skipIfOpen: boolOr(cfg.SkipIfOpen, true),
		home:       home,
		now:        time.Now,
		openCheck:  osshim.HasOpenFiles,
		openMany:   osshim.OpenFilesUnder,
		git:        runGit,
		processes:  lookupToolProcesses,
	}
	p.protected = expandProtected(config.DefaultProtected(), home)
	p.protected = append(p.protected, config.BuiltinProtectedRoots(home)...)
	return p, nil
}

func durationOr(value string, def time.Duration, field string, parse func(string) (time.Duration, error)) (time.Duration, error) {
	if value == "" {
		return def, nil
	}
	d, err := parse(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", field, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", field, value)
	}
	return d, nil
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// idleFor is the idle threshold under a pressure level: the tier value when
// scaling is on and it is shorter than min_idle, else min_idle.
func (p *ProjectArtifactsProvider) idleFor(pressure Pressure) time.Duration {
	if d, ok := p.tierIdle[pressure]; ok {
		return min(d, p.minIdle)
	}
	return p.minIdle
}

// SetProtected implements ProtectionAware. The paths add to the built-in ones.
func (p *ProjectArtifactsProvider) SetProtected(paths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, path := range paths {
		if path = filepath.Clean(path); !slices.Contains(p.protected, path) {
			p.protected = append(p.protected, path)
		}
	}
}

// CurrentSize implements Provider: the size of every artifact found, idle or not.
func (p *ProjectArtifactsProvider) CurrentSize(ctx context.Context) (int64, error) {
	scan, err := p.scan(ctx)
	if err != nil {
		return 0, err
	}
	return scan.total(), nil
}

// Recoverable implements Recoverer. It applies every check a clean applies
// except the open-file probe, which is too slow for a status listing.
func (p *ProjectArtifactsProvider) Recoverable(ctx context.Context) (int64, error) {
	scan, err := p.scan(ctx)
	if err != nil {
		return 0, err
	}
	pass := p.newPass(ctx, scan)
	var total int64
	for _, c := range pass.candidates(ctx) {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if reason := pass.guard(ctx, c); reason == "" {
			total += c.art.Size
		}
	}
	return total, ctx.Err()
}

// Clean implements Provider. Smart mode stops once the artifacts are under
// max_size, or, under disk pressure, once opts.Recovered reports recovery.
// Full mode removes every eligible artifact.
func (p *ProjectArtifactsProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	scan, err := p.scan(ctx)
	if err != nil {
		return CleanResult{}, err
	}
	if !opts.DryRun {
		p.invalidate()
	}

	var (
		out    strings.Builder
		result CleanResult
		errs   []error
	)
	finish := func(err error) (CleanResult, error) {
		result.Output = strings.TrimRight(out.String(), "\n")
		return result, err
	}

	if !opts.DryRun {
		result.Warnings = p.sweepTrash(scan, &out)
	}
	if noteRefused(scan, &out, &result) {
		return finish(nil)
	}
	if scan.partial {
		fmt.Fprintf(&out, "note: scan stopped at its %s budget, some projects may be missing\n", p.budget)
	}

	pass := p.newPass(ctx, scan)
	pass.minIdle = p.idleFor(opts.Pressure)
	pass.cands = pass.candidates(ctx)
	pass.dryRun = opts.DryRun
	remaining := scan.total()
	var freed int64
	started := p.now()
	skip := func(c *candidate, reason string) {
		pass.noteSkip(reason)
		skipLine(&out, &result, c, reason)
	}

	for _, c := range pass.cands {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		if reason := pass.cheapReason(c); reason != "" {
			skip(c, reason)
			continue
		}
		if p.satisfied(opts, remaining-freed, freed) {
			break
		}
		if p.passBudget > 0 && p.now().Sub(started) > p.passBudget {
			skip(c, "pass time budget")
			continue
		}
		reason := pass.guard(ctx, c)
		if reason == "" {
			reason = pass.openReason(ctx, c)
		}
		if reason == "" {
			reason = p.finalReason(ctx, c, pass.minIdle)
		}
		if reason == "" && !opts.DryRun {
			reason = pass.openReasonWithin(ctx, c, removalSnapshotTTL)
		}
		if reason == "" {
			if !opts.DryRun {
				pass.procs, pass.procsErr = p.processes(ctx)
			}
			reason = pass.busyReason(c)
		}
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		if reason != "" {
			skip(c, reason)
			continue
		}

		detail := c.detail(p.now(), pass.minIdle)
		if !opts.DryRun {
			gone, partial, rmErr := p.removeCandidate(ctx, c)
			switch {
			case !gone:
				skip(c, "cannot rename aside: "+rmErr.Error())
				continue
			case rmErr != nil:
				errs = append(errs, recordPartialRemoval(&out, &result, c, detail, partial, rmErr))
				freed += partial.bytes
				continue
			}
		}
		recordRemoved(&out, &result, c, detail, opts.DryRun)
		freed += c.art.Size
	}

	if len(errs) == 0 {
		result.SkipReason = pass.allSkippedReason(result.SkippedEntries)
	}
	return finish(errors.Join(errs...))
}

type removedPart struct{ bytes, files int64 }

// recordRemoved books one artifact as removed, or as one a dry-run would remove.
func recordRemoved(out *strings.Builder, result *CleanResult, c *candidate, detail string, dryRun bool) {
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	fmt.Fprintf(out, "%s: %s (%s; %s)\n", verb, c.art.Path, size.FormatSize(c.art.Size), detail)
	result.BytesCleaned += c.art.Size
	result.FilesDeleted += c.art.Files
	result.Entries = append(result.Entries, Entry{Path: c.art.Path, Size: c.art.Size, Detail: detail})
}

// recordPartialRemoval books what a failed delete did remove and returns the
// error to report.
func recordPartialRemoval(out *strings.Builder, result *CleanResult, c *candidate, detail string, partial removedPart, rmErr error) error {
	fmt.Fprintf(out, "error: %s (%v)\n", c.art.Path, rmErr)
	result.BytesCleaned += partial.bytes
	result.FilesDeleted += partial.files
	if partial.bytes > 0 || partial.files > 0 {
		result.Entries = append(result.Entries, Entry{Path: c.art.Path, Size: partial.bytes, Detail: detail + ", partly removed"})
	}
	return fmt.Errorf("remove %s: %w", c.art.Path, rmErr)
}

// noteRefused reports the roots that were not scanned: as a warning beside
// the roots that were, as the skip reason when no root was usable. It returns
// true when nothing was scanned.
func noteRefused(scan *artifactScan, out *strings.Builder, result *CleanResult) bool {
	for _, refusal := range scan.refused {
		fmt.Fprintf(out, "skip: root %s\n", refusal)
		result.Warnings = append(result.Warnings, "root not scanned: "+refusal)
	}
	if scan.roots > 0 || len(scan.refused) == 0 {
		return false
	}
	result.SkipReason = "no usable root: " + strings.Join(scan.refused, "; ")
	return true
}

// removeCandidate renames the artifact aside and deletes it. When the delete
// fails after the rename, partial says how much did go, measured from what is
// left in the trash directory.
func (p *ProjectArtifactsProvider) removeCandidate(ctx context.Context, c *candidate) (gone bool, partial removedPart, err error) {
	trash, gone, err := removeAside(c.art.Path)
	p.settleProject(c.proj)
	if !gone || err == nil {
		return gone, partial, err
	}
	left, leftFiles, _ := measureTree(ctx, trash)
	return true, removedPart{bytes: max(c.art.Size-left, 0), files: max(c.art.Files-leftFiles, 0)}, err
}

func (p *ProjectArtifactsProvider) satisfied(opts CleanOptions, left, freed int64) bool {
	switch {
	case opts.Recovered != nil:
		return opts.Recovered(freed)
	case opts.Mode == CleanModeSmart:
		return left <= p.maxSize
	default:
		return false
	}
}

func skipLine(out *strings.Builder, result *CleanResult, c *candidate, reason string) {
	fmt.Fprintf(out, "skip: %s (%s)\n", c.art.Path, reason)
	result.SkippedEntries++
}

type pass struct {
	p        *ProjectArtifactsProvider
	scan     *artifactScan
	procs    []toolProcess
	procsErr error
	dirty    map[string]string
	// minIdle is the idle threshold this pass applies, after pressure scaling.
	minIdle time.Duration

	cands     []*candidate
	firstSkip string
	dryRun    bool
	openSnap  map[string]bool
	openErr   error
	openAt    time.Time
	openDone  time.Time
}

// noteSkip remembers the first reason an artifact was skipped.
func (ps *pass) noteSkip(reason string) {
	if ps.firstSkip == "" {
		ps.firstSkip = reason
	}
}

// allSkippedReason is the skip reason of a pass that left every candidate
// alone, so the concise output says why instead of only counting skips. It is
// empty when anything was removed or the pass ended before reaching them all.
func (ps *pass) allSkippedReason(skipped int) string {
	if len(ps.cands) == 0 || skipped != len(ps.cands) {
		return ""
	}
	return fmt.Sprintf("all %d artifacts skipped, first: %s", len(ps.cands), ps.firstSkip)
}

// openReason runs the slow open-file probe. Windows cannot list handles, but
// it also refuses to rename a directory with open files, so the rename that
// follows is the check there. One listing of open files answers for every
// candidate and is reused for openSnapshotTTL.
func (ps *pass) openReason(ctx context.Context, c *candidate) string {
	return ps.openReasonWithin(ctx, c, openSnapshotTTL)
}

// openReasonWithin is openReason with a chosen limit on the age of the
// listing it may reuse. The check right before a removal passes a short one.
func (ps *pass) openReasonWithin(ctx context.Context, c *candidate, maxAge time.Duration) string {
	p := ps.p
	if !p.skipIfOpen {
		return ""
	}
	var (
		open bool
		err  error
	)
	if p.openMany != nil {
		open, err = ps.openInSnapshot(ctx, c.art.Path, maxAge)
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, openCheckTimeout)
		defer cancel()
		open, err = p.openCheck(probeCtx, c.art.Path)
	}
	return openVerdict(open, err)
}

// openVerdict turns an open-file answer into a skip reason. Where handles
// cannot be listed (Windows) the probe is no check at all: the rename-aside
// that follows is, because Windows refuses to rename a directory holding open
// files, and the process table is read again just before it.
func openVerdict(open bool, err error) string {
	switch {
	case errors.Is(err, osshim.ErrOpenFilesUnsupported):
		return ""
	case err != nil:
		return "open-file check failed: " + err.Error()
	case open:
		return "has open files"
	}
	return ""
}

// openAge reports whether the open-file listing is too old for maxAge. The
// pass limit counts from the start of the listing; the shorter limit before a
// removal counts from its end, so a slow listing is not run twice in a row.
func (ps *pass) openAge(maxAge time.Duration) bool {
	if maxAge < openSnapshotTTL {
		return ps.p.now().Sub(ps.openDone) > maxAge
	}
	return ps.p.now().Sub(ps.openAt) > maxAge
}

func (ps *pass) openInSnapshot(ctx context.Context, path string, maxAge time.Duration) (bool, error) {
	p := ps.p
	if ps.openAt.IsZero() || (!ps.dryRun && ps.openAge(maxAge)) {
		seen := map[string]bool{}
		var dirs []string
		for _, c := range ps.cands {
			if !seen[c.art.Path] {
				seen[c.art.Path] = true
				dirs = append(dirs, c.art.Path)
			}
		}
		probeCtx, cancel := context.WithTimeout(ctx, openCheckTimeout)
		defer cancel()
		ps.openAt = p.now()
		ps.openSnap, ps.openErr = p.openMany(probeCtx, dirs)
		ps.openDone = p.now()
	}
	if ps.openErr != nil {
		return false, ps.openErr
	}
	return ps.openSnap[path], nil
}

// finalReason revalidates just before removal: the markers must still be
// there and the project must not have been touched while checks ran.
func (p *ProjectArtifactsProvider) finalReason(ctx context.Context, c *candidate, minIdle time.Duration) string {
	if !markersValid(c.art) {
		return "markers changed"
	}
	root, below, _, err := sampleTree(ctx, c.proj.Dir, sampleLimit)
	if err != nil {
		return "recheck failed: " + err.Error()
	}
	proj := c.proj
	if proj.gitDir != "" {
		gitTime, gitErr := gitActivity(proj.gitDir)
		switch {
		case gitErr != nil:
			return "recheck failed: " + gitErr.Error()
		case gitTime.After(proj.gitTime):
			return "git activity during checks"
		}
	}
	if below.After(proj.sampled) || (root.After(proj.sampled) && !root.Equal(proj.ownRoot)) {
		return "modified during checks"
	}
	return p.useReason(c, true, minIdle)
}

// useReason skips an artifact that was built or run within min_idle, however
// long its sources have been idle: a cron job may still start target/release/x
// or node_modules/.bin/x. fresh recomputes instead of reusing the first reading.
func (p *ProjectArtifactsProvider) useReason(c *candidate, fresh bool, minIdle time.Duration) string {
	art := c.art
	if fresh || !art.usageRead {
		art.used, art.usageErr = artifactUse(art)
		art.usageRead = true
	}
	if art.usageErr != nil {
		return art.usageErr.Error()
	}
	if idle := p.now().Sub(art.used); idle < minIdle {
		return fmt.Sprintf("artifact in use %s ago, min_idle %s", idle.Round(time.Hour), formatIdle(minIdle))
	}
	return ""
}

// removeAside renames dir next to itself and deletes the copy, so a crash
// leaves either the intact directory or a trash directory that no marker
// check accepts, never a half-deleted artifact under its real name. gone
// reports whether the rename happened. The parent keeps its old mtime: the
// removal is not project activity.
func removeAside(dir string) (trash string, gone bool, err error) {
	defer keepParentTime(dir)()
	trash = filepath.Join(filepath.Dir(dir), trashName(filepath.Base(dir)))
	if err := os.Rename(dir, trash); err != nil {
		return "", false, err
	}
	return trash, true, os.RemoveAll(trash)
}

func keepParentTime(path string) func() {
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return func() {}
	}
	return func() { _ = os.Chtimes(parent, info.ModTime(), info.ModTime()) }
}

// settleProject accepts a bump of the project directory's own mtime after a
// removal whose mtime restore failed. The sample itself is never re-baselined,
// so a real edit made during a long removal still blocks the next artifact.
func (p *ProjectArtifactsProvider) settleProject(proj *project) {
	if info, err := os.Lstat(proj.Dir); err == nil && info.ModTime().After(proj.sampled) {
		proj.ownRoot = info.ModTime()
	}
}

// sweepTrash removes our leftover trash directories and returns a warning for
// each one it could not remove.
func (p *ProjectArtifactsProvider) sweepTrash(scan *artifactScan, out *strings.Builder) []string {
	var warnings []string
	for _, trash := range scan.trash {
		if p.protectedReason(trash) != "" {
			continue
		}
		restore := keepParentTime(trash)
		err := os.RemoveAll(trash)
		restore()
		if err != nil {
			fmt.Fprintf(out, "error: sweeping %s (%v)\n", trash, err)
			warnings = append(warnings, fmt.Sprintf("cannot sweep leftover %s: %v", trash, err))
			continue
		}
		fmt.Fprintf(out, "swept: %s\n", trash)
	}
	return warnings
}

func (p *ProjectArtifactsProvider) invalidate() {
	p.mu.Lock()
	p.cached = nil
	p.mu.Unlock()
}

// scan returns the artifact scan, reusing a recent one so the size probe and
// the clean that follows it do not walk every tree twice.
func (p *ProjectArtifactsProvider) scan(ctx context.Context) (*artifactScan, error) {
	p.mu.Lock()
	if p.cached != nil && time.Since(p.cachedAt) < scanCacheTTL {
		scan := p.cached
		p.mu.Unlock()
		return scan, nil
	}
	p.mu.Unlock()

	scan, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.cached, p.cachedAt = scan, time.Now()
	p.mu.Unlock()
	return scan, nil
}

type candidate struct {
	art  *artifactDir
	proj *project
}

func (c *candidate) detail(now time.Time, minIdle time.Duration) string {
	return fmt.Sprintf("%s, project %s, idle %dd, min_idle %s", c.art.Kind, filepath.Base(c.proj.Dir), idleDays(now, latest(c.proj.newest, c.art.used)), formatIdle(minIdle))
}

// formatIdle prints whole days as Nd, the unit users configure.
func formatIdle(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}

func idleDays(now, then time.Time) int {
	if then.IsZero() || now.Before(then) {
		return 0
	}
	return int(now.Sub(then) / (24 * time.Hour))
}

func (p *ProjectArtifactsProvider) newPass(ctx context.Context, scan *artifactScan) *pass {
	ps := &pass{p: p, scan: scan, minIdle: p.minIdle, dirty: map[string]string{}}
	ps.procs, ps.procsErr = p.processes(ctx)
	return ps
}

// candidates lists every artifact, oldest project first. Ties break on
// project path, then larger artifact first, then path, so order is stable.
func (ps *pass) candidates(ctx context.Context) []*candidate {
	var list []*candidate
	for _, proj := range ps.scan.projects {
		if ctx.Err() != nil {
			return nil
		}
		ps.p.measureActivity(ctx, proj)
		for _, art := range proj.artifacts {
			list = append(list, &candidate{art: art, proj: proj})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		switch {
		case !a.proj.newest.Equal(b.proj.newest):
			return a.proj.newest.Before(b.proj.newest)
		case a.proj.Dir != b.proj.Dir:
			return a.proj.Dir < b.proj.Dir
		case a.art.Size != b.art.Size:
			return a.art.Size > b.art.Size
		default:
			return a.art.Path < b.art.Path
		}
	})
	return list
}

// cheapReason covers the checks that need no process or git work. A
// non-empty reason means skip.
func (ps *pass) cheapReason(c *candidate) string {
	p := ps.p
	if reason := p.protectedReason(c.proj.Dir); reason != "" {
		return reason
	}
	if reason := p.protectedReason(c.art.Path); reason != "" {
		return reason
	}
	if c.proj.problem != "" {
		return c.proj.problem
	}
	if idle := p.now().Sub(c.proj.newest); idle < ps.minIdle {
		return fmt.Sprintf("project active %s ago, min_idle %s", idle.Round(time.Hour), formatIdle(ps.minIdle))
	}
	return p.useReason(c, false, ps.minIdle)
}

// guard runs every check that does not need the slow open-file probe.
func (ps *pass) guard(ctx context.Context, c *candidate) string {
	if reason := ps.cheapReason(c); reason != "" {
		return reason
	}
	if c.art.unreadable {
		return "part of it cannot be read"
	}
	if reason := ps.busyReason(c); reason != "" {
		return reason
	}
	return ps.dirtyReason(ctx, c.proj)
}
