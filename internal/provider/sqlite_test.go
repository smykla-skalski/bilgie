package provider

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPageSize = 4096

func writeFakeSQLite(t *testing.T, path string, pages, freePages uint32) {
	t.Helper()
	hdr := make([]byte, sqliteHeaderSize)
	copy(hdr, sqliteMagic)
	binary.BigEndian.PutUint16(hdr[16:18], testPageSize)
	binary.BigEndian.PutUint32(hdr[28:32], pages)
	binary.BigEndian.PutUint32(hdr[36:40], freePages)
	require.NoError(t, os.WriteFile(path, hdr, 0o600))
}

type sqliteFixture struct {
	provider *SQLiteCompactProvider
	calls    *[]string
	db       string
}

func newSQLiteFixture(t *testing.T, maxSize string, pages, freePages uint32) sqliteFixture {
	t.Helper()
	db := filepath.Join(t.TempDir(), "state.db")
	writeFakeSQLite(t, db, pages, freePages)

	p, err := NewSQLiteCompactProvider("sqlite-compact", config.Provider{
		Enabled: true, Paths: []string{db}, MaxSize: maxSize,
	})
	require.NoError(t, err)

	calls := []string{}
	p.lookPath = func(string) (string, error) { return "/usr/bin/sqlite3", nil }
	p.anyOpen = func(context.Context, []string) (bool, error) { return false, nil }
	p.freeDisk = func(string) (uint64, error) { return 1 << 40, nil }
	p.run = func(_ context.Context, _, _, sql string) (string, error) {
		calls = append(calls, sql)
		if strings.HasPrefix(sql, "PRAGMA integrity_check") {
			return "ok", nil
		}
		return "", nil
	}
	return sqliteFixture{provider: p, calls: &calls, db: db}
}

func TestSQLiteCompactReportsFreePages(t *testing.T) {
	fx := newSQLiteFixture(t, "1M", 1000, 600)

	recoverable, err := fx.provider.Recoverable(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(600*testPageSize), recoverable)

	current, err := fx.provider.CurrentSize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, recoverable, current)
}

func TestSQLiteCompactReportsDatabaseInUse(t *testing.T) {
	fx := newSQLiteFixture(t, "1M", 1000, 600)
	fx.provider.anyOpen = func(context.Context, []string) (bool, error) { return true, nil }

	dbs := fx.provider.Databases(t.Context())
	require.Len(t, dbs, 1)
	assert.True(t, dbs[0].InUse)
	assert.Equal(t, int64(600*testPageSize), dbs[0].FreeBytes)
}

func TestSQLiteCompactSkipsNonDatabases(t *testing.T) {
	fx := newSQLiteFixture(t, "1M", 1000, 600)
	require.NoError(t, os.WriteFile(fx.db, []byte("plain text"), 0o600))

	assert.Empty(t, fx.provider.Databases(t.Context()))
}

func TestSQLiteCompactClean(t *testing.T) {
	tests := []struct {
		name        string
		pages       uint32
		free        uint32
		open        bool
		openErr     error
		freeDisk    uint64
		skipContain string
		wantRun     bool
	}{
		{name: "compacts when eligible", pages: 1000, free: 600, freeDisk: 1 << 40, wantRun: true},
		{name: "skips below max_size", pages: 1000, free: 200, freeDisk: 1 << 40, skipContain: "not above"},
		{name: "skips at half of the file", pages: 1000, free: 500, freeDisk: 1 << 40, skipContain: "not above"},
		{name: "skips in use", pages: 1000, free: 600, open: true, freeDisk: 1 << 40, skipContain: "in use"},
		{
			name: "skips when open files are unknown", pages: 1000, free: 600,
			openErr: errors.New("no lsof"), freeDisk: 1 << 40, skipContain: "cannot check open files",
		},
		{name: "skips on low disk", pages: 1000, free: 600, freeDisk: 100, skipContain: "free disk"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newSQLiteFixture(t, "1K", tt.pages, tt.free)
			fx.provider.anyOpen = func(context.Context, []string) (bool, error) { return tt.open, tt.openErr }
			fx.provider.freeDisk = func(string) (uint64, error) { return tt.freeDisk, nil }

			res, err := fx.provider.Clean(t.Context(), CleanOptions{})
			require.NoError(t, err)

			if tt.wantRun {
				assert.Empty(t, res.SkipReason)
				assert.Contains(t, res.Output, "compacted "+fx.db)
				assert.Equal(t, []string{"VACUUM; PRAGMA wal_checkpoint(TRUNCATE);", "PRAGMA integrity_check;"}, *fx.calls)
				return
			}
			assert.Contains(t, res.SkipReason, tt.skipContain)
			assert.Empty(t, *fx.calls)
			assert.Equal(t, 1, res.SkippedEntries)
		})
	}
}

func TestSQLiteCompactDryRunRunsNothing(t *testing.T) {
	fx := newSQLiteFixture(t, "1K", 1000, 600)
	fx.provider.lookPath = func(string) (string, error) { return "", errors.New("missing") }

	res, err := fx.provider.Clean(t.Context(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "would compact "+fx.db)
	assert.Empty(t, *fx.calls)
}

func TestSQLiteCompactMissingBinary(t *testing.T) {
	fx := newSQLiteFixture(t, "1K", 1000, 600)
	fx.provider.lookPath = func(string) (string, error) { return "", errors.New("missing") }

	res, err := fx.provider.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "sqlite3 not found")
}

func TestSQLiteCompactVacuumFailureIsSkip(t *testing.T) {
	fx := newSQLiteFixture(t, "1K", 1000, 600)
	fx.provider.run = func(context.Context, string, string, string) (string, error) {
		return "database is locked", errors.New("exit status 5")
	}

	res, err := fx.provider.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "VACUUM failed")
	assert.Zero(t, res.BytesCleaned)
}

func TestSQLiteCompactWarnsOnFailedIntegrityCheck(t *testing.T) {
	fx := newSQLiteFixture(t, "1K", 1000, 600)
	fx.provider.run = func(_ context.Context, _, _, sql string) (string, error) {
		if strings.HasPrefix(sql, "PRAGMA integrity_check") {
			return "*** in database main ***", nil
		}
		return "", nil
	}

	res, err := fx.provider.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "integrity_check")
}

func TestSQLiteCompactRejectsGlobPath(t *testing.T) {
	_, err := NewSQLiteCompactProvider("sqlite-compact", config.Provider{
		Paths: []string{"/tmp/*.db"}, MaxSize: "1G",
	})
	require.Error(t, err)
}

func TestSQLiteCompactRealDatabase(t *testing.T) {
	bin, err := exec.LookPath(sqliteBinary)
	if err != nil {
		t.Skip("sqlite3 not installed")
	}
	db := filepath.Join(t.TempDir(), "real.db")
	setup := "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB);" +
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<200)" +
		" INSERT INTO t(v) SELECT zeroblob(20000) FROM n;" +
		"DELETE FROM t WHERE id>10;"
	out, err := execSQLite(t.Context(), bin, db, setup)
	require.NoError(t, err, out)

	p, err := NewSQLiteCompactProvider("sqlite-compact", config.Provider{
		Enabled: true, Paths: []string{db}, MaxSize: "1K",
	})
	require.NoError(t, err)

	before, err := p.Recoverable(t.Context())
	require.NoError(t, err)
	require.Positive(t, before, "setup output: %s", out)

	res, err := p.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.Empty(t, res.SkipReason, res.Output)
	assert.Empty(t, res.Warnings)

	after, err := p.Recoverable(t.Context())
	require.NoError(t, err)
	assert.Zero(t, after)

	verdict, err := execSQLite(t.Context(), bin, db, "PRAGMA integrity_check;")
	require.NoError(t, err)
	assert.Equal(t, "ok", verdict)
}

func TestSQLiteCompactTimeoutIsSkip(t *testing.T) {
	fx := newSQLiteFixture(t, "1K", 1000, 600)
	fx.provider.timeout = 1
	fx.provider.run = func(ctx context.Context, _, _, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	res, err := fx.provider.Clean(t.Context(), CleanOptions{})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "timed out")
}

func TestSQLiteCompactCancelledContext(t *testing.T) {
	fx := newSQLiteFixture(t, "1K", 1000, 600)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := fx.provider.Clean(ctx, CleanOptions{})
	assert.NoError(t, err)
	assert.Empty(t, *fx.calls)
}

func TestReadSQLiteHeaderRejectsBadHeaders(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.db")
	require.NoError(t, os.WriteFile(short, []byte(sqliteMagic), 0o600))
	_, _, err := readSQLiteHeader(short)
	require.Error(t, err)

	empty := filepath.Join(dir, "empty.db")
	writeFakeSQLite(t, empty, 0, 0)
	_, _, err = readSQLiteHeader(empty)
	require.Error(t, err)

	_, _, err = readSQLiteHeader(filepath.Join(dir, "absent.db"))
	require.Error(t, err)
}

func TestReadSQLiteHeaderPageSize65536(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.db")
	writeFakeSQLite(t, path, 10, 4)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	binary.BigEndian.PutUint16(data[16:18], 1)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	fileBytes, freeBytes, err := readSQLiteHeader(path)
	require.NoError(t, err)
	assert.Equal(t, int64(10*65536), fileBytes)
	assert.Equal(t, int64(4*65536), freeBytes)
}
