package osshim

import (
	"context"
	"time"
)

// Process is one row of a process listing.
type Process struct {
	PID         int
	PPID        int
	CommandLine string
	// Args is the command line without any appended kernel process name.
	Args string
	// Started is when the process began; zero when the platform listing does
	// not report it.
	Started time.Time
}

// ProcessCommandLines returns the command line of every running process.
func ProcessCommandLines(ctx context.Context) ([]string, error) {
	procs, err := ProcessTable(ctx)
	if err != nil {
		return nil, err
	}
	return commandLines(procs), nil
}

func commandLines(procs []Process) []string {
	lines := make([]string, len(procs))
	for i := range procs {
		lines[i] = procs[i].CommandLine
	}
	return lines
}
