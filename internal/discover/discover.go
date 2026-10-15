// Package discover finds the providers a machine needs. It only reads: it
// stats paths, measures sizes and runs the protection checks a clean pass
// runs. It never deletes anything and never changes the config; turning a
// suggestion on is the caller's explicit step.
package discover

import (
	"context"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

// Candidate kinds.
const (
	KindProvider  = "provider"
	KindUnmanaged = "unmanaged"
)

const sizeWorkers = 4

// Entry is the dir-pattern entry --write adds for an unmanaged directory.
type Entry struct {
	Type    string   `json:"type"`
	MaxSize string   `json:"max_size"`
	MinIdle string   `json:"min_idle"`
	Paths   []string `json:"paths"`
}

// Candidate is one provider or unmanaged directory discover looked at.
type Candidate struct {
	Modified   *time.Time       `json:"modified,omitempty"`
	Define     *config.Provider `json:"-"`
	Entry      *Entry           `json:"entry,omitempty"`
	Name       string           `json:"name"`
	Kind       string           `json:"kind"`
	Tier       Tier             `json:"tier"`
	Action     Action           `json:"action"`
	Reason     string           `json:"reason,omitempty"`
	Tool       string           `json:"tool,omitempty"`
	Size       string           `json:"size"`
	Error      string           `json:"error,omitempty"`
	Paths      []string         `json:"paths"`
	Bytes      int64            `json:"bytes"`
	AgeSeconds int64            `json:"age_seconds,omitempty"`
	Enabled    bool             `json:"enabled"`
	Present    bool             `json:"present"`
	ToolFound  bool             `json:"tool_found"`
	Partial    bool             `json:"partial,omitempty"`
}

// Detected reports whether the machine has the provider's cache or tool.
func (c Candidate) Detected() bool { return c.Present || c.ToolFound }

// Suggested reports whether discover offers the candidate for enabling.
func (c Candidate) Suggested() bool { return c.Action == ActionEnable || c.Action == ActionOptIn }

// Report is the outcome of Run. Incomplete means a time budget or the
// context ended a scan early, so sizes may be lower bounds.
type Report struct {
	Candidates []Candidate `json:"candidates"`
	Incomplete bool        `json:"incomplete,omitempty"`
}

// Suggestions returns the candidates discover offers for enabling.
func (r Report) Suggestions() []Candidate {
	var out []Candidate
	for i := range r.Candidates {
		if r.Candidates[i].Suggested() {
			out = append(out, r.Candidates[i])
		}
	}
	return out
}

// Options configures one discovery run. Load, NewProvider and LookPath
// default to provider.LoadProvider, provider.NewProvider and exec.LookPath.
// Roots are the directories the unmanaged scan reads and default to the
// roots of this OS. Sizes measures each detected provider; without it sizes
// stay zero. All keeps providers the machine has neither cache nor tool for.
// Unmanaged is how many unmanaged directories to list; zero skips the scan.
type Options struct {
	Cfg             *config.Config
	Platform        config.Platform
	Now             time.Time
	Load            func(name string, cfg *config.Config) (provider.Provider, error)
	NewProvider     func(name string, cfg config.Provider) (provider.Provider, error)
	LookPath        func(string) (string, error)
	Home            string
	Roots           []string
	Unmanaged       int
	UnmanagedMin    int64
	UnmanagedBudget time.Duration
	Sizes           bool
	All             bool
}

// Run looks at every built-in provider that applies on the OS, and, when
// Unmanaged is set, at the largest directories no provider covers.
func Run(ctx context.Context, opts Options) Report {
	opts = withDefaults(opts)
	builtins := config.DefaultProvidersFor(opts.Platform)

	var names []string
	for name := range opts.Cfg.Providers {
		if _, ok := builtins[name]; ok && opts.Cfg.Applies(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	cands := make([]Candidate, 0, len(names))
	for _, name := range names {
		cands = append(cands, inspect(name, opts.Cfg.Providers[name], builtins[name], opts))
	}

	var report Report
	classify(ctx, cands, opts, &report)
	if opts.Sizes {
		measure(ctx, cands, opts, &report)
	}

	for i := range cands {
		if opts.All || cands[i].Detected() {
			report.Candidates = append(report.Candidates, cands[i])
		}
	}
	if opts.Unmanaged > 0 {
		report.Candidates = append(report.Candidates, unmanaged(ctx, opts, &report)...)
	}
	sortCandidates(report.Candidates)
	if report.Candidates == nil {
		report.Candidates = []Candidate{}
	}
	return report
}

// inspect reads what the machine has for one provider, without measuring it.
func inspect(name string, pc, builtin config.Provider, opts Options) Candidate {
	c := Candidate{Name: name, Kind: KindProvider, Enabled: pc.Enabled, Paths: []string{}, Size: size.FormatSize(0)}
	c.Present = config.ProviderPathsExist(name, pc)
	if c.Present {
		c.Paths, c.Modified = existing(name, pc, opts.Now)
		if c.Modified != nil {
			c.AgeSeconds = int64(opts.Now.Sub(*c.Modified).Seconds())
		}
	}
	if tool := toolFor(name, pc); tool != "" {
		c.Tool = tool
		_, err := opts.LookPath(tool)
		c.ToolFound = err == nil
	}
	c.Tier, c.Reason = tierFor(name, builtin)
	return c
}

// existing lists the provider's locations that exist and the newest change
// among them.
func existing(name string, pc config.Provider, now time.Time) ([]string, *time.Time) {
	paths := pc.Paths
	if name == "uv" {
		if dir, err := config.ResolveUVCacheDir(pc.Paths); err == nil {
			paths = []string{dir}
		}
	}
	expanded, err := config.ExpandPaths(paths)
	if err != nil {
		return []string{}, nil
	}
	out := []string{}
	var newest *time.Time
	for _, p := range expanded {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		out = append(out, p)
		if mod := info.ModTime(); !mod.After(now) && (newest == nil || mod.After(*newest)) {
			newest = &mod
		}
	}
	return out, newest
}

// classify applies the protection checks of a clean pass to every detected
// candidate and then settles each one's action. A check that cannot finish
// leaves the candidate protected: discover never suggests what it could not
// verify.
func classify(ctx context.Context, cands []Candidate, opts Options, report *Report) {
	probe := *opts.Cfg
	probe.Providers = make(map[string]config.Provider, len(cands))
	for i := range cands {
		if !cands[i].Detected() {
			continue
		}
		pc := opts.Cfg.Providers[cands[i].Name]
		pc.Enabled = true
		probe.Providers[cands[i].Name] = pc
	}

	conflicts := auto.ProtectionConflicts(ctx, &probe, opts.Home, opts.NewProvider)
	reasons := make(map[string]string, len(conflicts))
	cutoff := ""
	for _, conflict := range conflicts {
		if conflict.Incomplete {
			cutoff = conflict.Provider
			report.Incomplete = true
			continue
		}
		reasons[conflict.Provider] = conflict.Reason
	}

	for i := range cands {
		c := &cands[i]
		switch {
		case auto.NeverRuns(c.Name, opts.Cfg.Providers[c.Name]):
			protect(c, "auto never cleans this provider")
		case reasons[c.Name] != "":
			protect(c, reasons[c.Name])
		case cutoff != "" && c.Detected() && c.Name >= cutoff:
			protect(c, "protection check incomplete: not verified")
		}
		c.Action = actionFor(c)
	}
}

func protect(c *Candidate, reason string) {
	c.Tier, c.Reason = TierProtected, reason
}

func actionFor(c *Candidate) Action {
	switch {
	case c.Tier == TierProtected:
		return ActionNever
	case c.Enabled || !c.Detected():
		return ActionNone
	case c.Tier == TierOptIn:
		return ActionOptIn
	default:
		return ActionEnable
	}
}

// measure sizes every detected candidate with the provider's own scan, a few
// at a time.
func measure(ctx context.Context, cands []Candidate, opts Options, report *Report) {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		work = make(chan int)
	)
	for range sizeWorkers {
		wg.Go(func() {
			for i := range work {
				bytes, err := sizeOf(ctx, cands[i].Name, opts)
				mu.Lock()
				c := &cands[i]
				c.Bytes, c.Size = bytes, size.FormatSize(bytes)
				if err != nil {
					c.Error = err.Error()
				}
				mu.Unlock()
			}
		})
	}
	for i := range cands {
		if cands[i].Detected() && ctx.Err() == nil {
			work <- i
		}
	}
	close(work)
	wg.Wait()
	if ctx.Err() != nil {
		report.Incomplete = true
	}
}

func sizeOf(ctx context.Context, name string, opts Options) (int64, error) {
	p, err := opts.Load(name, opts.Cfg)
	if err != nil {
		return 0, err
	}
	return p.CurrentSize(ctx)
}

func sortCandidates(cands []Candidate) {
	rank := map[Action]int{ActionEnable: 0, ActionOptIn: 1, ActionNone: 2, ActionNever: 3}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.Kind != b.Kind {
			return a.Kind == KindProvider
		}
		if rank[a.Action] != rank[b.Action] {
			return rank[a.Action] < rank[b.Action]
		}
		if a.Bytes != b.Bytes {
			return a.Bytes > b.Bytes
		}
		return a.Name < b.Name
	})
}
