package provider

import (
	"context"
	"time"
)

// CleanMode determines cleaning strategy.
type CleanMode int

// CleanMode constants.
const (
	CleanModeFull  CleanMode = iota // Delete everything (via command or all files)
	CleanModeSmart                  // Smart clean: delete files older than max_age, then LRU until under max_size
)

// Provider defines the interface for cache providers.
type Provider interface {
	// Name returns the provider's identifier.
	Name() string

	// Paths returns the expanded paths this provider manages.
	Paths() []string

	// CurrentSize returns the total size of cached files in bytes.
	// The scan stops early if ctx is cancelled.
	CurrentSize(ctx context.Context) (int64, error)

	// MaxSize returns the configured maximum size in bytes.
	MaxSize() int64

	// MaxAge returns the configured maximum file age.
	MaxAge() time.Duration

	// Clean removes cached files to bring size under limit.
	Clean(ctx context.Context, opts CleanOptions) (CleanResult, error)

	// Available returns whether the provider can be used.
	// For most providers this is always true. Docker checks if daemon is running.
	Available() bool
}

// DiskSizer is implemented by providers that track a VM disk image separately from actual data usage.
type DiskSizer interface {
	DiskImageSize(ctx context.Context) (int64, error)
}

// Recoverer is implemented by providers that can tell how many bytes a clean
// would free right now, which is less than their size when part of it is in
// use or too recent to remove.
type Recoverer interface {
	Recoverable(ctx context.Context) (int64, error)
}

// ProtectionAware is implemented by providers that scan for their own
// targets below broad roots and so enforce the protected paths themselves.
// The caller hands them the absolute protected paths, which add to the
// built-in ones.
type ProtectionAware interface {
	SetProtected(paths []string)
}

// PathGuarded is implemented by providers that work through many matched
// paths and can skip single ones. The guard returns why a path is off
// limits, or "" when it is clear. A guarded provider skips the protected
// matches and still cleans the rest, instead of being skipped whole.
type PathGuarded interface {
	SetPathGuard(guard func(path string) string)
}

// Pressure is the disk-pressure level a clean runs under. Providers that
// scale an idle threshold read it; the rest ignore it.
type Pressure int

// Pressure levels, lowest to highest.
const (
	PressureNone Pressure = iota
	PressureCritical
	PressureEmergency
)

// CleanOptions configures cleaning behavior.
type CleanOptions struct {
	DryRun bool
	Mode   CleanMode
	// Pressure shortens project-artifacts' idle threshold at the critical and
	// emergency tiers. The zero value applies the configured threshold.
	Pressure Pressure
	// Timeout bounds Docker's prune command when positive. Other providers
	// keep their own clean_timeout. Zero leaves Docker unbounded.
	Timeout time.Duration
	// Recovered is set under disk pressure. Providers that can free space in
	// small steps call it with the bytes freed so far and stop once it
	// reports true. Providers that cannot ignore it.
	Recovered func(freed int64) bool
}

// CleanResult contains cleaning operation results.
type CleanResult struct {
	Output string
	// SkipReason is set when the provider declined to clean, for example
	// because its tool is busy. A skip is not an error.
	SkipReason string
	// Entries lists what was removed, or what a dry-run would remove, so
	// callers can summarize without parsing Output. Providers that act
	// through an external command leave it empty.
	Entries []Entry
	// SkippedEntries counts candidates a provider examined and left alone.
	SkippedEntries int
	// Warnings are problems that did not fail the clean but need the user's
	// eye, such as a leftover that could not be removed.
	Warnings     []string
	BytesCleaned int64
	FilesDeleted int64
}
