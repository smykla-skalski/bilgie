package auto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/smykla-skalski/bilgie/internal/fsx"
)

// RunLogName is the append-only JSON Lines file of auto runs.
const RunLogName = "runs.jsonl"

// rotateAt moves the log aside so an unattended agent cannot grow it forever.
const rotateAt = 8 << 20

// ProviderRecord is one provider's outcome in a run record.
type ProviderRecord struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
	FreedBytes int64  `json:"freed_bytes"`
	// Warnings are problems that did not fail the provider, such as a
	// leftover that could not be removed.
	Warnings []string `json:"warnings,omitempty"`
}

// RunRecord is one line of the run log.
type RunRecord struct {
	Time        time.Time        `json:"time"`
	Tier        string           `json:"tier"`
	Error       string           `json:"error,omitempty"`
	Providers   []ProviderRecord `json:"providers"`
	FreeBefore  int64            `json:"free_before_bytes"`
	FreeAfter   int64            `json:"free_after_bytes"`
	TotalBytes  int64            `json:"total_bytes"`
	FreedBytes  int64            `json:"freed_bytes"`
	DryRun      bool             `json:"dry_run"`
	Recovered   bool             `json:"recovered"`
	Notified    bool             `json:"notified"`
	SwapUsedMiB int32            `json:"swap_used_mib,omitempty"`
}

// NewRunRecord summarizes a report. runErr is the error that ended the run
// early, if any.
func NewRunRecord(report Report, now time.Time, runErr error, notified bool) RunRecord {
	rec := RunRecord{
		Time:       now.UTC(),
		Tier:       report.Tier.String(),
		DryRun:     report.DryRun,
		Recovered:  report.Recovered,
		Notified:   notified,
		FreeBefore: report.Start.Free,
		FreeAfter:  report.End.Free,
		TotalBytes: report.Start.Total,
		Providers:  make([]ProviderRecord, 0, len(report.Results)),
	}
	if runErr != nil {
		rec.Error = runErr.Error()
	}
	for i := range report.Results {
		res := &report.Results[i]
		pr := ProviderRecord{Name: res.Name, Status: res.Status, Reason: res.Reason, FreedBytes: res.Freed, Warnings: res.Warnings}
		if res.Err != nil {
			pr.Error = res.Err.Error()
		}
		rec.FreedBytes += res.Freed
		rec.Providers = append(rec.Providers, pr)
	}
	return rec
}

// AppendRun adds rec to the run log as exactly one line, written with a single
// O_APPEND write so concurrent writers cannot interleave. A crash can leave a
// last line without its newline; it is closed first so it cannot swallow this
// record.
func AppendRun(stateDir string, rec RunRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode run record: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	path := filepath.Join(stateDir, RunLogName)
	if info, statErr := os.Lstat(path); statErr == nil && info.Size() >= rotateAt {
		if err := os.Rename(path, path+".1"); err != nil {
			return fmt.Errorf("rotate run log: %w", err)
		}
	}

	f, err := fsx.OpenAppendNoLink(path, 0o600)
	if err != nil {
		return fmt.Errorf("open run log: %w", err)
	}
	line := make([]byte, 0, len(data)+2)
	line = append(line, data...)
	line = append(line, '\n')
	if unterminated(f) {
		line = append([]byte{'\n'}, line...)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("write run record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close run log: %w", err)
	}
	return nil
}

func unterminated(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return false
	}
	return last[0] != '\n'
}

// ReadRuns returns the newest limit records, oldest first (all when limit is
// not positive), and the number of unreadable lines it skipped. A missing log
// is not an error.
func ReadRuns(stateDir string, limit int) ([]RunRecord, int, error) {
	var (
		all     []RunRecord
		corrupt int
	)
	path := filepath.Join(stateDir, RunLogName)
	for _, p := range []string{path + ".1", path} {
		recs, bad, err := readRunFile(p)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, recs...)
		corrupt += bad
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, corrupt, nil
}

func readRunFile(path string) ([]RunRecord, int, error) {
	f, err := fsx.OpenRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("open run log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var (
		recs    []RunRecord
		corrupt int
	)
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var rec RunRecord
			if json.Unmarshal(trimmed, &rec) != nil || rec.Time.IsZero() {
				corrupt++
			} else {
				recs = append(recs, rec)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return recs, corrupt, nil
		}
		if readErr != nil {
			return nil, 0, fmt.Errorf("read run log: %w", readErr)
		}
	}
}
