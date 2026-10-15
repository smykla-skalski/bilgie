package osshim

import (
	"context"
	"time"
)

// ProcessStart reports whether pid is a running process and when it started.
// A zero start with running true means the process exists but its start time
// is unknown; an error means the answer is unknown. The start time is only as
// precise as the OS reports it, about a second.
func ProcessStart(ctx context.Context, pid int) (start time.Time, running bool, err error) {
	return processStart(ctx, pid)
}
