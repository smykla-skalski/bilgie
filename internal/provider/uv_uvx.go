package provider

import (
	"context"
	"fmt"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
)

// uvxVerdict is what the uv busy check found. busy is a skip reason; aged
// lists the long-running uvx processes that hold the cache. While aged is
// not empty uv must not run at all: `uv cache prune` waits for their lock and
// `--force` removes the environments they run from.
type uvxVerdict struct {
	busy string
	aged []osshim.Process
}

// classifyUV runs the uv busy check. A uv process that is not a uvx server
// older than minAge, a held lock nobody explains and any failed check still
// block, as they always did. A uvx server older than minAge no longer blocks
// the provider when it holds this cache's lock: it is reported in aged so the
// caller can sweep around it. One that does not hold the lock uses another
// cache and is ignored, since uv keeps its cache lock for as long as it runs.
func (g *busyGuard) classifyUV(ctx context.Context, minAge time.Duration) uvxVerdict {
	if g == nil {
		return uvxVerdict{}
	}
	heldPath := ""
	for _, path := range g.locks {
		held, err := g.lockHeld(path)
		if err != nil {
			return uvxVerdict{busy: fmt.Sprintf("cannot check lock %s: %v", path, err)}
		}
		if held && heldPath == "" {
			heldPath = path
		}
	}
	procs, err := g.listProcesses(ctx)
	if err != nil {
		return uvxVerdict{busy: "cannot list processes: " + err.Error()}
	}
	now := time.Now()
	if g.now != nil {
		now = g.now()
	}
	var servers []osshim.Process
	for i := range procs {
		tool := matchProcess(procs[i].CommandLine, g.processes)
		if tool == "" {
			continue
		}
		if uvxAgedOut(&procs[i], now, minAge) {
			servers = append(servers, procs[i])
			continue
		}
		if heldPath != "" {
			return uvxVerdict{busy: "lock held: " + heldPath}
		}
		return uvxVerdict{busy: describeBusy(tool, &procs[i], now)}
	}
	if heldPath == "" {
		return uvxVerdict{}
	}
	if len(servers) == 0 {
		return uvxVerdict{busy: "lock held: " + heldPath}
	}
	holders, err := g.lockHolders(ctx, heldPath)
	if err != nil {
		return uvxVerdict{busy: fmt.Sprintf("lock held: %s; cannot tell who holds it: %v", heldPath, err)}
	}
	var aged []osshim.Process
	for _, pid := range holders {
		i := slices.IndexFunc(servers, func(p osshim.Process) bool { return p.PID == pid })
		if i < 0 {
			return uvxVerdict{busy: "lock held: " + heldPath}
		}
		aged = append(aged, servers[i])
	}
	if len(aged) == 0 {
		return uvxVerdict{busy: "lock held: " + heldPath}
	}
	return uvxVerdict{aged: aged}
}

// uvxAgedOut reports whether p is a uvx server that has run for minAge and
// whose command line runs uv only as uvx. An unknown start time keeps it busy.
func uvxAgedOut(p *osshim.Process, now time.Time, minAge time.Duration) bool {
	if p.Started.IsZero() || now.Sub(p.Started) < minAge {
		return false
	}
	return isUvxCommand(commandText(p), runtime.GOOS == "windows")
}

// isUvxCommand reports whether every uv invocation on a command line is
// `uvx ...`, `uv tool uvx ...` or `uv tool run ...`, and there is at least
// one. Any other uv command, or one that does not parse, is not a uvx server.
func isUvxCommand(commandLine string, windows bool) bool {
	fields := strings.Fields(separatorWords.Replace(commandLine))
	found, inArgs := false, false
	for i, field := range fields {
		if inArgs {
			inArgs = !endsCommand(field)
			continue
		}
		if fieldTool(field, windows) != "uv" {
			continue
		}
		switch {
		case isUvxBinary(field, windows):
		case i+2 < len(fields) && trimField(fields[i+1]) == "tool" &&
			slices.Contains([]string{"uvx", "run"}, trimField(fields[i+2])):
		default:
			return false
		}
		found, inArgs = true, true
	}
	return found
}

func isUvxBinary(field string, windows bool) bool {
	field = trimField(field)
	if windows {
		field = strings.ToLower(strings.ReplaceAll(field, `\`, "/"))
		field = strings.TrimSuffix(field, ".exe")
	}
	return path.Base(field) == "uvx"
}

// describeAged names the aged uvx processes for a skip or an output line.
func describeAged(aged []osshim.Process, now time.Time) string {
	parts := make([]string, 0, len(aged))
	for i := range aged {
		p := &aged[i]
		part := "pid " + strconv.Itoa(p.PID)
		if !p.Started.IsZero() {
			part += ", up " + now.Sub(p.Started).Round(time.Second).String()
		}
		parts = append(parts, part)
	}
	return "uvx is running (" + strings.Join(parts, "; ") + ")"
}
