package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

const codexRolloutGlob = "rollout-*.jsonl"

// CodexSessionsProvider removes Codex session logs (rollout-*.jsonl) older
// than max_age. Removing one drops that session from "codex resume", so the
// provider is opt-in and ignores max_size.
type CodexSessionsProvider struct {
	*BaseProvider
	now      func() time.Time
	openMany func(ctx context.Context, paths []string) (map[string]bool, error)
}

// NewCodexSessionsProvider creates a provider that prunes old Codex rollouts.
func NewCodexSessionsProvider(name string, cfg config.Provider) (*CodexSessionsProvider, error) {
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}
	if base.maxAge <= 0 {
		return nil, errors.New("max_age is required and must be positive")
	}
	return &CodexSessionsProvider{BaseProvider: base, now: time.Now, openMany: osshim.OpenFilesUnder}, nil
}

type rolloutFile struct {
	modTime time.Time
	path    string
	size    int64
}

// CurrentSize implements Provider; only rollout files count.
func (p *CodexSessionsProvider) CurrentSize(ctx context.Context) (int64, error) {
	files, err := p.listRollouts(ctx)
	var total int64
	for _, f := range files {
		total += f.size
	}
	return total, err
}

func (p *CodexSessionsProvider) listRollouts(ctx context.Context) ([]rolloutFile, error) {
	var files []rolloutFile
	for _, root := range p.paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if ok, _ := filepath.Match(codexRolloutGlob, d.Name()); !ok {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			files = append(files, rolloutFile{path: path, size: info.Size(), modTime: info.ModTime()})
			return nil
		})
		if err != nil {
			return files, err
		}
	}
	return files, nil
}

// Clean implements Provider. Both modes apply the same retention.
func (p *CodexSessionsProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	files, err := p.listRollouts(ctx)
	if err != nil {
		return CleanResult{}, err
	}
	cutoff := p.now().Add(-p.maxAge)
	var old []rolloutFile
	for _, f := range files {
		if f.modTime.Before(cutoff) {
			old = append(old, f)
		}
	}
	if len(old) == 0 {
		return CleanResult{Output: "no sessions older than retention"}, nil
	}
	sort.Slice(old, func(i, j int) bool { return old[i].path < old[j].path })

	paths := make([]string, len(old))
	for i, f := range old {
		paths[i] = f.path
	}
	open, openErr := p.openMany(ctx, paths)
	if err := ctx.Err(); err != nil {
		return CleanResult{Output: "interrupted"}, err
	}

	var (
		res     CleanResult
		output  strings.Builder
		failed  int
		skipped int
	)
	for _, f := range old {
		if openErr != nil || open[f.path] {
			skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			res.Output = "interrupted"
			return res, err
		}
		if opts.DryRun {
			fmt.Fprintf(&output, "would remove: %s (%s)\n", f.path, size.FormatSize(f.size))
		} else if err := os.Remove(f.path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			fmt.Fprintf(&output, "error removing %s: %v\n", f.path, err)
			failed++
			continue
		}
		res.BytesCleaned += f.size
		res.FilesDeleted++
		res.Entries = append(res.Entries, Entry{Path: f.path, Size: f.size})
	}

	if skipped > 0 {
		reason := "open in a process"
		if openErr != nil {
			reason = "open-file check failed: " + openErr.Error()
		}
		fmt.Fprintf(&output, "kept %d sessions: %s\n", skipped, reason)
	}
	if output.Len() > 0 {
		res.Output = strings.TrimSpace(output.String())
	} else {
		res.Output = fmt.Sprintf("removed %d sessions", res.FilesDeleted)
	}
	if failed > 0 {
		return res, fmt.Errorf("%d sessions could not be removed", failed)
	}
	return res, nil
}
