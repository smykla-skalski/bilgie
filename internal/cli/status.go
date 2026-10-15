package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/term"
	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/smykla-skalski/bilgie/pkg/size"
	"github.com/spf13/cobra"
)

// ProviderStatus holds scan result for a single provider.
type ProviderStatus struct {
	Name           string `json:"name"`
	CurrentFmt     string `json:"current"`
	MaxFmt         string `json:"max"`
	Error          string `json:"error,omitempty"`
	DiskImageFmt   string `json:"disk_image,omitempty"`
	RecoverableFmt string `json:"recoverable,omitempty"`
	Current        int64  `json:"current_bytes"`
	Max            int64  `json:"max_bytes"`
	DiskImageBytes int64  `json:"disk_image_bytes,omitempty"`
	Recoverable    int64  `json:"recoverable_bytes,omitempty"`
	OverLimit      bool   `json:"over_limit"`
	// Databases lists each SQLite database a compaction provider manages.
	Databases []DatabaseStatus `json:"databases,omitempty"`
}

// DatabaseStatus is one SQLite database with the free-page bytes a VACUUM would return.
type DatabaseStatus struct {
	Path        string `json:"path"`
	Reclaimable string `json:"reclaimable"`
	Bytes       int64  `json:"bytes"`
	Reclaim     int64  `json:"reclaimable_bytes"`
	InUse       bool   `json:"in_use"`
}

// StatusOutput holds full status output for JSON serialization.
type StatusOutput struct {
	Total      string                `json:"total"`
	Providers  []ProviderStatus      `json:"providers"`
	Unmanaged  *auto.UnmanagedReport `json:"unmanaged,omitempty"`
	Protected  *auto.ProtectedReport `json:"protected,omitempty"`
	TotalBytes int64                 `json:"total_bytes"`
}

// unmanagedScan finds large directories no provider covers; nil skips the scan.
type unmanagedScan func(ctx context.Context, cfg *config.Config) auto.UnmanagedReport

// protectedScan measures protected data that needs a human; nil skips the scan.
type protectedScan func(ctx context.Context, cfg *config.Config) auto.ProtectedReport

// StatusCmd shows cache status for all enabled providers.
var StatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show cache status for all providers",
	RunE:         runStatus,
	SilenceUsage: true,
}

func init() {
	StatusCmd.Flags().Bool("json", false, "Output in JSON format")
	StatusCmd.Flags().Int("unmanaged", auto.DefaultUnmanagedTop, "Number of large directories no provider covers to list (0 disables the scan)")
}

func runStatus(cmd *cobra.Command, _ []string) error {
	jsonFlag, _ := cmd.Flags().GetBool("json")
	top, _ := cmd.Flags().GetInt("unmanaged")
	if top < 0 {
		return fmt.Errorf("--unmanaged must not be negative, got %d", top)
	}
	var scan unmanagedScan
	if top > 0 {
		scan = defaultUnmanagedScan(top)
	}
	return runStatusWithLoader(config.NewLoader(), jsonFlag, scan, defaultProtectedScan)
}

func defaultProtectedScan(ctx context.Context, cfg *config.Config) auto.ProtectedReport {
	home, _ := os.UserHomeDir()
	return auto.ScanProtected(ctx, cfg, home, auto.DefaultProtectedBudget)
}

func defaultUnmanagedScan(top int) unmanagedScan {
	return func(ctx context.Context, cfg *config.Config) auto.UnmanagedReport {
		home, _ := os.UserHomeDir()
		return auto.ScanUnmanaged(ctx, auto.ScanOptions{
			Roots:    auto.UnmanagedRoots(home),
			Covered:  auto.CoveredPaths(cfg),
			Top:      top,
			MinBytes: auto.DefaultUnmanagedMin,
			Budget:   auto.DefaultUnmanagedBudget,
		})
	}
}

func runStatusWithLoader(loader *config.Loader, jsonOutput bool, scan unmanagedScan, protect protectedScan) error {
	cfg, err := loader.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	providers := cfg.EnabledProviders()
	if len(providers) == 0 {
		fmt.Println("No enabled providers")
		if protect != nil && !jsonOutput {
			outputProtected(protect(context.Background(), cfg))
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var unmanaged *auto.UnmanagedReport
	var wg sync.WaitGroup
	if scan != nil {
		wg.Go(func() {
			report := scan(ctx, cfg)
			unmanaged = &report
		})
	}
	var protected *auto.ProtectedReport
	if protect != nil {
		wg.Go(func() {
			report := protect(ctx, cfg)
			protected = &report
		})
	}
	statuses := scanProviders(ctx, cfg, providers)
	wg.Wait()

	if jsonOutput {
		return outputJSON(statuses, unmanaged, protected)
	}
	if err := outputTable(statuses); err != nil {
		return err
	}
	if unmanaged != nil {
		outputUnmanaged(*unmanaged)
	}
	if protected != nil {
		outputProtected(*protected)
	}
	return nil
}

func scanProviders(ctx context.Context, cfg *config.Config, names []string) []ProviderStatus {
	statuses := make([]ProviderStatus, len(names))
	var wg sync.WaitGroup

	for i, name := range names {
		wg.Add(1)
		go func(idx int, provName string) {
			defer wg.Done()
			statuses[idx] = scanProvider(ctx, cfg, provName)
		}(i, name)
	}
	wg.Wait()
	return statuses
}

func scanProvider(ctx context.Context, cfg *config.Config, name string) ProviderStatus {
	status := ProviderStatus{Name: name}

	p, err := provider.LoadProvider(name, cfg)
	if err != nil {
		status.Error = err.Error()
		return status
	}

	maxSize := p.MaxSize()
	status.Max = maxSize
	status.MaxFmt = size.FormatSize(maxSize)

	current, err := p.CurrentSize(ctx)
	if err != nil {
		status.Error = fmt.Sprintf("provider %s: get current size: %v", name, err)
		return status
	}

	status.Current = current
	status.CurrentFmt = size.FormatSize(current)
	status.OverLimit = current > maxSize

	if ds, ok := p.(provider.DiskSizer); ok {
		if diskSize, diskErr := ds.DiskImageSize(ctx); diskErr == nil && diskSize > 0 && diskSize != current {
			status.DiskImageBytes = diskSize
			status.DiskImageFmt = size.FormatSize(diskSize)
		}
	}

	if rc, ok := p.(provider.Recoverer); ok {
		if recoverable, recErr := rc.Recoverable(ctx); recErr == nil && recoverable > 0 {
			status.Recoverable = recoverable
			status.RecoverableFmt = size.FormatSize(recoverable)
		}
	}

	if dr, ok := p.(provider.DatabaseReporter); ok {
		for _, db := range dr.Databases(ctx) {
			status.Databases = append(status.Databases, DatabaseStatus{
				Path: db.Path, Bytes: db.FileBytes, Reclaim: db.FreeBytes,
				Reclaimable: size.FormatSize(db.FreeBytes), InUse: db.InUse || !db.InUseKnown,
			})
		}
	}

	return status
}

func outputJSON(statuses []ProviderStatus, unmanaged *auto.UnmanagedReport, protected *auto.ProtectedReport) error {
	var total int64
	for i := range statuses {
		total += statuses[i].Current
	}

	out := StatusOutput{
		Providers:  statuses,
		TotalBytes: total,
		Total:      size.FormatSize(total),
		Unmanaged:  unmanaged,
		Protected:  protected,
	}
	if unmanaged != nil && unmanaged.Dirs == nil {
		unmanaged.Dirs = []auto.UnmanagedDir{}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func outputTable(statuses []ProviderStatus) error {
	rows := make([][]string, 0, len(statuses))
	var total int64

	for i := range statuses {
		s := &statuses[i]
		total += s.Current

		statusText := okStyle.Render("ok")
		if s.Error != "" {
			statusText = errorStyle.Render("error")
		} else if s.OverLimit {
			statusText = overStyle.Render("OVER")
		}

		currentFmt := s.CurrentFmt
		if s.DiskImageFmt != "" {
			currentFmt = fmt.Sprintf("%s (%s on disk)", s.CurrentFmt, s.DiskImageFmt)
		}
		if s.RecoverableFmt != "" {
			currentFmt = fmt.Sprintf("%s (%s recoverable)", currentFmt, s.RecoverableFmt)
		}
		maxFmt := s.MaxFmt
		if s.Error != "" {
			if currentFmt == "" {
				currentFmt = "-"
			}
			if maxFmt == "" {
				maxFmt = "-"
			}
		}
		rows = append(rows, []string{s.Name, currentFmt, maxFmt, statusText})
	}

	width, _, _ := term.GetSize(os.Stdout.Fd())
	if width <= 0 {
		width = 80
	}

	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(dimStyle).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return headerStyle
			}
			return lipgloss.NewStyle()
		}).
		Headers("Provider", "Current", "Max", "Status").
		Rows(rows...).
		Width(width)

	fmt.Println(t)
	for i := range statuses {
		if statuses[i].Error != "" {
			fmt.Println(errorStyle.Render(statuses[i].Error))
		}
	}
	for i := range statuses {
		s := &statuses[i]
		for _, db := range s.Databases {
			state := "idle"
			if db.InUse {
				state = "in use"
			}
			fmt.Printf("  %s: %s reclaimable of %s (%s) %s\n",
				s.Name, db.Reclaimable, size.FormatSize(db.Bytes), state, db.Path)
		}
	}
	fmt.Println()
	fmt.Println(totalStyle.Render(fmt.Sprintf("Total: %s", size.FormatSize(total))))

	return nil
}

func outputUnmanaged(report auto.UnmanagedReport) {
	fmt.Println()
	if len(report.Dirs) == 0 {
		fmt.Println(dimStyle.Render("No unmanaged directories above " + size.FormatSize(auto.DefaultUnmanagedMin)))
	} else {
		fmt.Println(headerStyle.Render("Unmanaged large directories (no provider covers them)"))
		for _, d := range report.Dirs {
			mark := ""
			if d.Partial {
				mark = " (at least)"
			}
			fmt.Printf("  %10s%s  %s\n", size.FormatSize(d.Bytes), mark, d.Path)
		}
	}
	if report.Incomplete {
		fmt.Println(dimStyle.Render("Scan stopped at its time budget; sizes may be incomplete."))
	}
}

func outputProtected(report auto.ProtectedReport) {
	fmt.Println()
	if len(report.Entries) == 0 {
		fmt.Println(dimStyle.Render("Needs a human: no protected data found"))
	} else {
		fmt.Println(headerStyle.Render("Needs a human (protected, never auto-deleted)"))
		for _, e := range report.Entries {
			mark := ""
			if e.Partial {
				mark = " (at least)"
			}
			fmt.Printf("  %10s%s  %s\n", size.FormatSize(e.Bytes), mark, e.Path)
		}
	}
	if report.Incomplete {
		fmt.Println(dimStyle.Render("Scan stopped at its time budget; sizes may be incomplete."))
	}
}
