package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/smykla-skalski/bilgie/pkg/size"
)

const (
	sqliteBinary        = "sqlite3"
	sqliteHeaderSize    = 100
	sqliteMagic         = "SQLite format 3\x00"
	sqliteBusyTimeoutMS = 1000
	maxDiskBytes        = 1 << 62
)

// SQLiteDatabase describes one listed database: its size and the bytes held
// by free pages, which a VACUUM returns to the filesystem. InUseKnown is
// false when open files could not be listed.
type SQLiteDatabase struct {
	Path       string
	FileBytes  int64
	FreeBytes  int64
	InUse      bool
	InUseKnown bool
}

// sqliteRunner runs the sqlite3 shell on a database with the given SQL.
type sqliteRunner func(ctx context.Context, bin, db, sql string) (string, error)

// SQLiteCompactProvider compacts SQLite databases in place with VACUUM.
// It deletes no file. A database is compacted only when no process has it
// open, its free pages pass max_size and half of the file, and the disk can
// hold a full copy of the live data.
type SQLiteCompactProvider struct {
	*BaseProvider
	run      sqliteRunner
	lookPath func(string) (string, error)
	anyOpen  func(ctx context.Context, paths []string) (bool, error)
	freeDisk func(path string) (uint64, error)
	timeout  time.Duration
}

// NewSQLiteCompactProvider creates the compaction provider. paths are the
// database files; max_size is the free-page size above which a database is
// compacted.
func NewSQLiteCompactProvider(name string, cfg config.Provider) (*SQLiteCompactProvider, error) {
	for _, path := range cfg.Paths {
		if strings.ContainsAny(path, "*?[") {
			return nil, fmt.Errorf("path %q: globs are not allowed, list each database file", path)
		}
	}
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}
	timeout, err := parseCleanTimeout(cfg.CleanTimeout)
	if err != nil {
		return nil, err
	}
	return &SQLiteCompactProvider{
		BaseProvider: base,
		run:          execSQLite,
		lookPath:     exec.LookPath,
		anyOpen:      osshim.AnyFileOpen,
		freeDisk: func(path string) (uint64, error) {
			ds, err := osshim.QueryDiskSpace(path)
			return ds.Free, err
		},
		timeout: timeout,
	}, nil
}

func execSQLite(ctx context.Context, bin, db, sql string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, "-bail", "-cmd",
		fmt.Sprintf(".timeout %d", sqliteBusyTimeoutMS), db, sql)
	cmd.WaitDelay = cleanWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String() + stderr.String()), err
}

// Available is always true: a missing sqlite3 is a skip with its reason.
func (p *SQLiteCompactProvider) Available() bool { return true }

// CurrentSize implements Provider. It is the free-page bytes of all listed
// databases, the part that compaction can return.
func (p *SQLiteCompactProvider) CurrentSize(ctx context.Context) (int64, error) {
	return p.Recoverable(ctx)
}

// Recoverable implements Recoverer: free-page bytes of every listed
// database, in use or not.
func (p *SQLiteCompactProvider) Recoverable(ctx context.Context) (int64, error) {
	var total int64
	for _, db := range p.Databases(ctx) {
		total += db.FreeBytes
	}
	return total, ctx.Err()
}

// Databases reports every listed database that exists and is a SQLite file.
func (p *SQLiteCompactProvider) Databases(ctx context.Context) []SQLiteDatabase {
	var out []SQLiteDatabase
	for _, path := range p.paths {
		if ctx.Err() != nil {
			break
		}
		fileBytes, freeBytes, err := readSQLiteHeader(path)
		if err != nil {
			continue
		}
		open, openErr := p.anyOpen(ctx, sqliteFiles(path))
		out = append(out, SQLiteDatabase{
			Path: path, FileBytes: fileBytes, FreeBytes: freeBytes,
			InUse: open, InUseKnown: openErr == nil,
		})
	}
	return out
}

func sqliteFiles(db string) []string {
	return []string{db, db + "-wal", db + "-shm"}
}

// readSQLiteHeader returns the database size and its free-page bytes from
// the 100-byte header, which needs no lock and no sqlite3 binary. An
// uncheckpointed WAL can make the counts lag.
func readSQLiteHeader(path string) (fileBytes, freeBytes int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()

	hdr := make([]byte, sqliteHeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, 0, fmt.Errorf("read header: %w", err)
	}
	if string(hdr[:len(sqliteMagic)]) != sqliteMagic {
		return 0, 0, errors.New("not a SQLite database")
	}
	pageSize := int64(binary.BigEndian.Uint16(hdr[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	pages := int64(binary.BigEndian.Uint32(hdr[28:32]))
	free := int64(binary.BigEndian.Uint32(hdr[36:40]))
	if pageSize < 512 || pages == 0 {
		return 0, 0, errors.New("invalid SQLite header")
	}
	free = min(free, pages)
	return pages * pageSize, free * pageSize, nil
}

// Clean implements Provider. Smart and full modes behave the same.
func (p *SQLiteCompactProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	bin, lookErr := p.lookPath(sqliteBinary)
	if lookErr != nil && !opts.DryRun {
		return skipResult("sqlite3 not found on PATH"), nil
	}

	var res CleanResult
	var done, skips []string
	for _, db := range p.Databases(ctx) {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if reason := p.skipReason(ctx, db); reason != "" {
			skips = append(skips, db.Path+": "+reason)
			res.SkippedEntries++
			continue
		}
		if opts.DryRun {
			done = append(done, fmt.Sprintf("would compact %s (%s free of %s)",
				db.Path, size.FormatSize(db.FreeBytes), size.FormatSize(db.FileBytes)))
			continue
		}
		freed, line, err := p.compact(ctx, bin, db)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			skips = append(skips, db.Path+": "+err.Error())
			res.SkippedEntries++
			continue
		}
		res.BytesCleaned += freed
		done = append(done, line)
		if strings.Contains(line, "integrity_check") {
			res.Warnings = append(res.Warnings, line)
		}
	}

	lines := done
	for _, s := range skips {
		lines = append(lines, "skipped: "+s)
	}
	res.Output = strings.Join(lines, "\n")
	switch {
	case len(done) == 0 && len(skips) > 0:
		res.SkipReason = strings.Join(skips, "; ")
	case len(lines) == 0:
		res.Output = "no database to compact"
	}
	return res, nil
}

// skipReason explains why db must not be compacted now, or "" when it may.
func (p *SQLiteCompactProvider) skipReason(ctx context.Context, db SQLiteDatabase) string {
	if db.FreeBytes <= p.maxSize || db.FreeBytes*2 <= db.FileBytes {
		return fmt.Sprintf("free pages %s are not above both %s and half of %s",
			size.FormatSize(db.FreeBytes), size.FormatSize(p.maxSize), size.FormatSize(db.FileBytes))
	}
	open, err := p.anyOpen(ctx, sqliteFiles(db.Path))
	if err != nil {
		return "cannot check open files: " + err.Error()
	}
	if open {
		return "in use by another process"
	}
	free, err := p.freeDisk(filepath.Dir(db.Path))
	if err != nil {
		return "cannot read free disk space: " + err.Error()
	}
	live := db.FileBytes - db.FreeBytes
	if avail := int64(min(free, maxDiskBytes)); avail < live {
		return fmt.Sprintf("free disk %s is below the %s of live data VACUUM copies",
			size.FormatSize(avail), size.FormatSize(live))
	}
	return ""
}

// compact vacuums db and checks it afterwards. VACUUM builds a copy and swaps
// it in at commit, so a failed or killed run leaves the original intact.
func (p *SQLiteCompactProvider) compact(ctx context.Context, bin string, db SQLiteDatabase) (freed int64, summary string, err error) {
	before := sqliteDiskBytes(db.Path)

	runCtx := ctx
	if p.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	out, runErr := p.run(runCtx, bin, db.Path, "VACUUM; PRAGMA wal_checkpoint(TRUNCATE);")
	if runErr != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			runErr = fmt.Errorf("timed out after %s: %w", p.timeout, runErr)
		}
		return 0, "", fmt.Errorf("VACUUM failed: %w: %s", runErr, firstLine(out))
	}

	freed = max(before-sqliteDiskBytes(db.Path), 0)
	verdict, err := p.run(ctx, bin, db.Path, "PRAGMA integrity_check;")
	if err != nil || strings.TrimSpace(verdict) != "ok" {
		return freed, fmt.Sprintf("compacted %s but integrity_check did not return ok: %s",
			db.Path, firstLine(verdict)), nil
	}
	return freed, fmt.Sprintf("compacted %s (freed %s)", db.Path, size.FormatSize(freed)), nil
}

// sqliteDiskBytes is the database plus its -wal, which VACUUM grows.
func sqliteDiskBytes(db string) int64 {
	var total int64
	for _, f := range sqliteFiles(db)[:2] {
		if info, err := os.Stat(f); err == nil {
			total += info.Size()
		}
	}
	return total
}
