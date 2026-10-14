package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

const cargoLockName = ".cargo-lock"

// unitHash matches the 16-digit metadata hash cargo appends to unit names.
var unitHash = regexp.MustCompile(`-([0-9a-f]{16})(?:\.|$)`)

// trimResult is what trimming one Rust target removed, or would remove.
type trimResult struct {
	bytes int64
	files int64
	units int
}

// trimReason is why a target cannot be trimmed, empty when it can.
// trimSkip is why a profile cannot be trimmed.
type trimSkip string

// trimmable reports whether an artifact that is too active to remove whole may
// have its stale build units trimmed instead.
func (ps *pass) trimmable(c *candidate, reason string) bool {
	p := ps.p
	if c.art.Kind != kindRust || p.trimIdle <= 0 || c.proj.problem != "" {
		return false
	}
	if !strings.HasPrefix(reason, "project active") && !strings.HasPrefix(reason, "artifact in use") {
		return false
	}
	return p.protectedReason(c.proj.Dir) == "" && p.protectedReason(c.art.Path) == ""
}

// trimTarget removes build units of target that were not used within trimIdle.
// A profile whose .cargo-lock is held, or cannot be probed, is skipped. With
// dryRun nothing is removed.
func (p *ProjectArtifactsProvider) trimTarget(ctx context.Context, target string, dryRun bool) (trimResult, []trimSkip, error) {
	var (
		total trimResult
		skips []trimSkip
	)
	cutoff := p.now().Add(-p.trimIdle)
	for _, profile := range rustProfiles(target) {
		if err := ctx.Err(); err != nil {
			return total, skips, err
		}
		release, skip := lockProfile(profile)
		if skip != "" {
			skips = append(skips, skip)
			continue
		}
		res, err := trimProfile(ctx, profile, cutoff, dryRun)
		release()
		total.bytes += res.bytes
		total.files += res.files
		total.units += res.units
		if err != nil {
			return total, skips, err
		}
	}
	return total, skips, nil
}

// lockProfile probes the profile's .cargo-lock without waiting and keeps it
// held while the profile is trimmed, so cargo cannot start in between.
func lockProfile(profile string) (func(), trimSkip) {
	lock := filepath.Join(profile, cargoLockName)
	if _, err := os.Lstat(lock); errors.Is(err, fs.ErrNotExist) {
		return func() {}, ""
	} else if err != nil {
		return nil, trimSkip(fmt.Sprintf("cannot probe %s: %v", lock, err))
	}
	release, acquired, err := osshim.TryLock(lock)
	switch {
	case err != nil:
		return nil, trimSkip(fmt.Sprintf("cannot probe %s: %v", lock, err))
	case !acquired:
		return nil, trimSkip(fmt.Sprintf("cargo holds %s", lock))
	}
	return release, ""
}

// rustProfiles lists the profile directories of target: <target>/<profile> and
// <target>/<triple>/<profile>. A directory counts when it has a .fingerprint.
func rustProfiles(target string) []string {
	var profiles []string
	top, err := os.ReadDir(target)
	if err != nil {
		return nil
	}
	for _, e := range top {
		dir := filepath.Join(target, e.Name())
		if !e.IsDir() || !isRealDir(dir) {
			continue
		}
		if isRealDir(filepath.Join(dir, ".fingerprint")) {
			profiles = append(profiles, dir)
			continue
		}
		sub, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, s := range sub {
			child := filepath.Join(dir, s.Name())
			if s.IsDir() && isRealDir(filepath.Join(child, ".fingerprint")) {
				profiles = append(profiles, child)
			}
		}
	}
	return profiles
}

// lastUse is the newest mtime or atime among the files below path and path
// itself. A build reads the fingerprint of every unit it needs, so atime is
// what keeps an unchanged unit fresh; on a noatime mount only mtime counts.
func lastUse(ctx context.Context, path string) (time.Time, error) {
	var newest time.Time
	note := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if errors.Is(infoErr, fs.ErrNotExist) {
				return nil
			}
			return infoErr
		}
		note(info.ModTime())
		if info.Mode().IsRegular() {
			note(fileAtime(info))
		}
		return nil
	})
	return newest, err
}

// trimProfile removes the stale units of one profile directory: the unit's
// .fingerprint and build entries, its files in deps, and stale incremental
// caches. Units newer than cutoff are left whole.
func trimProfile(ctx context.Context, profile string, cutoff time.Time, dryRun bool) (trimResult, error) {
	var (
		res    trimResult
		doomed []string
	)
	stale := map[string]bool{}
	fresh := map[string]bool{}
	fingerprints, err := os.ReadDir(filepath.Join(profile, ".fingerprint"))
	if err != nil {
		return res, err
	}
	for _, e := range fingerprints {
		hash := unitSuffix(e.Name())
		if hash == "" {
			continue
		}
		path := filepath.Join(profile, ".fingerprint", e.Name())
		used, err := lastUse(ctx, path)
		if err != nil {
			return res, err
		}
		if used.Before(cutoff) {
			stale[hash] = true
			res.units++
			doomed = append(doomed, path, filepath.Join(profile, "build", e.Name()))
		} else {
			fresh[hash] = true
		}
	}

	deps, err := os.ReadDir(filepath.Join(profile, "deps"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	for _, e := range deps {
		m := unitHash.FindStringSubmatch(e.Name())
		if len(m) < 2 || fresh[m[1]] {
			continue
		}
		path := filepath.Join(profile, "deps", e.Name())
		if !stale[m[1]] {
			used, err := lastUse(ctx, path)
			if err != nil {
				return res, err
			}
			if !used.Before(cutoff) {
				continue
			}
		}
		doomed = append(doomed, path)
	}

	incremental, err := os.ReadDir(filepath.Join(profile, "incremental"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	for _, e := range incremental {
		path := filepath.Join(profile, "incremental", e.Name())
		used, err := lastUse(ctx, path)
		if err != nil {
			return res, err
		}
		if used.Before(cutoff) {
			doomed = append(doomed, path)
		}
	}

	for _, path := range doomed {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		bytes, files, ok := measureTrimEntry(ctx, path)
		if !ok {
			continue
		}
		if !dryRun {
			if err := os.RemoveAll(path); err != nil {
				return res, fmt.Errorf("trim %s: %w", path, err)
			}
		}
		res.bytes += bytes
		res.files += files
	}
	return res, nil
}

// unitSuffix returns the hash of a <name>-<hash> unit name, or "".
func unitSuffix(name string) string {
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return ""
	}
	if m := unitHash.FindStringSubmatch(name[i:]); m != nil {
		return m[1]
	}
	return ""
}

// measureTrimEntry sizes a file or directory; ok is false when it is gone.
func measureTrimEntry(ctx context.Context, path string) (bytes, files int64, ok bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, false
	}
	if !info.IsDir() {
		return info.Size(), 1, true
	}
	bytes, files, _ = measureTree(ctx, path)
	return bytes, files, true
}

var errNothingTrimmed = errors.New("nothing to trim")

// recordTrim trims one active Rust target and books the result. It returns
// errNothingTrimmed when no unit was stale or the target had to be skipped.
func (p *ProjectArtifactsProvider) recordTrim(ctx context.Context, c *candidate, dryRun bool, out *strings.Builder, result *CleanResult) (int64, error) {
	res, skips, err := p.trimTarget(ctx, c.art.Path, dryRun)
	for _, s := range skips {
		fmt.Fprintf(out, "skip: %s (%s)\n", c.art.Path, s)
	}
	if res.bytes > 0 || res.files > 0 {
		verb := "trimmed"
		if dryRun {
			verb = "would trim"
		}
		detail := fmt.Sprintf("%d stale units, unused for %s, project %s", res.units, p.trimIdle, filepath.Base(c.proj.Dir))
		fmt.Fprintf(out, "%s: %s (%s; %s)\n", verb, c.art.Path, size.FormatSize(res.bytes), detail)
		result.BytesCleaned += res.bytes
		result.FilesDeleted += res.files
		result.Entries = append(result.Entries, Entry{Path: c.art.Path, Size: res.bytes, Detail: detail})
	}
	if err != nil {
		return res.bytes, err
	}
	if res.bytes == 0 && res.files == 0 {
		return 0, errNothingTrimmed
	}
	return res.bytes, nil
}
