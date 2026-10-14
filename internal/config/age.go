package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// MinAgeThreshold is the shortest accepted max_age or min_idle. A threshold of
// seconds is nearly always a unit typo and would delete almost everything.
const MinAgeThreshold = time.Minute

// AgeFloorError reports an age threshold under MinAgeThreshold.
type AgeFloorError struct {
	Provider string
	Field    string
	Value    string
}

func (e *AgeFloorError) Error() string {
	msg := fmt.Sprintf("provider %q: %s %s is under the %s minimum",
		e.Provider, e.Field, e.Value, "1m")
	if s := e.suggestion(); s != "" {
		msg += "; did you mean " + s + "?"
	}
	return msg
}

func (e *AgeFloorError) suggestion() string {
	m := durationRegex.FindStringSubmatch(strings.TrimSpace(e.Value))
	if len(m) < 3 || strings.Trim(m[1], "0") == "" {
		return ""
	}
	switch strings.ToLower(m[2]) {
	case "ms":
		return m[1] + "m"
	case "", "s":
		return m[1] + "d"
	default:
		return ""
	}
}

// ValidateAges rejects max_age and min_idle values under MinAgeThreshold in
// every provider, enabled or not, so a typo cannot lie dormant. Empty and
// unparseable values are left to the providers, which report them.
func (c *Config) ValidateAges() error {
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := c.Providers[name]
		for _, f := range []struct{ field, value string }{
			{"max_age", p.MaxAge},
			{"min_idle", p.MinIdle},
			{"critical_min_idle", p.CriticalMinIdle},
			{"emergency_min_idle", p.EmergencyMinIdle},
		} {
			if strings.TrimSpace(f.value) == "" {
				continue
			}
			d, err := ParseBudget(f.value)
			if err != nil {
				continue
			}
			if d < MinAgeThreshold {
				return &AgeFloorError{Provider: name, Field: f.field, Value: strings.TrimSpace(f.value)}
			}
		}
	}
	return nil
}
