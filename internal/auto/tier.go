// Package auto implements unattended cache trimming driven by free disk space.
package auto

import (
	"fmt"
	"math"
	"os"
	"runtime"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
)

// Tier is the disk-pressure level: TierOK trims only providers over their
// limit, TierLow trims every enabled provider, TierCritical does the same at
// a shorter cooldown and sweeps the stale directories of providers whose
// sweep_tier is critical, TierEmergency sweeps all stale directories.
type Tier int

// Pressure tiers, lowest to highest.
const (
	TierOK Tier = iota
	TierLow
	TierCritical
	TierEmergency
)

func (t Tier) String() string {
	switch t {
	case TierOK:
		return "ok"
	case TierLow:
		return "low"
	case TierCritical:
		return "critical"
	case TierEmergency:
		return "emergency"
	default:
		return "unknown"
	}
}

// ParseTier is the inverse of String; unknown names are TierOK.
func ParseTier(s string) Tier {
	for t := TierOK; t <= TierEmergency; t++ {
		if t.String() == s {
			return t
		}
	}
	return TierOK
}

// FreeSpace is the free and total bytes of a volume.
type FreeSpace struct {
	Free  int64
	Total int64
}

// FreeFunc reports the current free space; tests inject a fixed value.
type FreeFunc func() (FreeSpace, error)

const dataVolume = "/System/Volumes/Data"

// DataVolumePath returns the path whose statfs reflects the user data volume.
// Other hosts fall back to the root, or the system drive on Windows.
func DataVolumePath() string {
	if runtime.GOOS == "windows" {
		return systemDrive()
	}
	if info, err := os.Stat(dataVolume); err == nil && info.IsDir() {
		return dataVolume
	}
	return "/"
}

func systemDrive() string {
	if drive := os.Getenv("SystemDrive"); drive != "" {
		return drive + `\`
	}
	return `C:\`
}

// StatfsFree returns a FreeFunc reading the filesystem space of path. Free
// counts the space available to unprivileged users.
func StatfsFree(path string) FreeFunc {
	return func() (FreeSpace, error) {
		space, err := osshim.QueryDiskSpace(path)
		if err != nil {
			return FreeSpace{}, fmt.Errorf("statfs %s: %w", path, err)
		}
		return FreeSpace{Free: clampInt64(space.Free), Total: clampInt64(space.Total)}, nil
	}
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// Thresholds are the free-space levels, in bytes, at which each tier starts.
type Thresholds struct {
	Low        int64
	Critical   int64
	Emergency  int64
	Hysteresis int64
}

// ThresholdsFor derives the tier levels for a volume of total bytes. The low
// level is max(min_free, min(min_free_pct of total, min_free_cap)): the cap keeps a
// percentage from putting a big disk in "low" forever, while an explicit
// min_free is never lowered. Critical and emergency never exceed the level
// above them.
func ThresholdsFor(total int64, l config.Limits) Thresholds {
	low := l.MinFree
	if l.MinFreePct > 0 && total > 0 {
		pct := int64(float64(total) * l.MinFreePct / 100)
		low = max(low, min(pct, l.MinFreeCap))
	}
	critical := min(l.CriticalFree, low)
	return Thresholds{
		Low:        low,
		Critical:   critical,
		Emergency:  min(l.EmergencyFree, critical),
		Hysteresis: l.Hysteresis,
	}
}

func (t Thresholds) boundary(tier Tier) int64 {
	switch tier {
	case TierLow:
		return t.Low
	case TierCritical:
		return t.Critical
	case TierEmergency:
		return t.Emergency
	default:
		return 0
	}
}

// Raw maps free bytes to a tier with no memory of the earlier tier.
func (t Thresholds) Raw(free int64) Tier {
	switch {
	case free < t.Emergency:
		return TierEmergency
	case free < t.Critical:
		return TierCritical
	case free < t.Low:
		return TierLow
	default:
		return TierOK
	}
}

// Settle applies hysteresis: the tier worsens at once but eases one level at
// a time, and only after free space is Hysteresis above that level's
// threshold. Space hovering at a threshold therefore cannot flap the tier.
func (t Thresholds) Settle(prev Tier, free int64) Tier {
	tier := t.Raw(free)
	if tier >= prev {
		return tier
	}
	tier = prev
	for tier > t.Raw(free) && free >= t.boundary(tier)+t.Hysteresis {
		tier--
	}
	return max(tier, t.Raw(free))
}

// BelowFloor reports whether free space is under the effective low threshold.
func BelowFloor(fs FreeSpace, cfg config.Auto) (bool, error) {
	l, err := cfg.Limits()
	if err != nil {
		return false, err
	}
	return fs.Free < ThresholdsFor(fs.Total, l).Low, nil
}

// ChooseTier maps free space to a pressure tier, without hysteresis.
func ChooseTier(fs FreeSpace, cfg config.Auto) (Tier, error) {
	l, err := cfg.Limits()
	if err != nil {
		return TierOK, err
	}
	return ThresholdsFor(fs.Total, l).Raw(fs.Free), nil
}
