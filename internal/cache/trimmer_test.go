package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestFile(t *testing.T, path string, size int64, age time.Duration) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, make([]byte, size), 0o600))

	mtime := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func TestTrim_DeletesOldFiles(t *testing.T) {
	dir := t.TempDir()

	// Create files with different ages
	createTestFile(t, filepath.Join(dir, "old.txt"), 1000, 40*24*time.Hour)  // 40 days old
	createTestFile(t, filepath.Join(dir, "new.txt"), 1000, 10*24*time.Hour)  // 10 days old
	createTestFile(t, filepath.Join(dir, "newer.txt"), 1000, 5*24*time.Hour) // 5 days old

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 10000, // Large enough to not trigger size-based deletion
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  false,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(1000), result.FreedBytes)
	assert.Equal(t, int64(1), result.DeletedCount)

	// Verify old file deleted, others remain
	assert.NoFileExists(t, filepath.Join(dir, "old.txt"))
	assert.FileExists(t, filepath.Join(dir, "new.txt"))
	assert.FileExists(t, filepath.Join(dir, "newer.txt"))
}

func TestTrim_DeletesUntilUnderMaxSize(t *testing.T) {
	dir := t.TempDir()

	// Create files totaling 3000 bytes
	createTestFile(t, filepath.Join(dir, "oldest.txt"), 1000, 20*24*time.Hour)
	createTestFile(t, filepath.Join(dir, "middle.txt"), 1000, 10*24*time.Hour)
	createTestFile(t, filepath.Join(dir, "newest.txt"), 1000, 5*24*time.Hour)

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 2000,                // Target: 2000 * 0.9 = 1800 bytes
		MaxAge:  60 * 24 * time.Hour, // No files qualify for age-based deletion
		DryRun:  false,
	})

	require.NoError(t, err)
	// Should delete oldest and middle to get under 1800
	assert.Equal(t, int64(2000), result.FreedBytes)
	assert.Equal(t, int64(2), result.DeletedCount)

	// Verify oldest files deleted
	assert.NoFileExists(t, filepath.Join(dir, "oldest.txt"))
	assert.NoFileExists(t, filepath.Join(dir, "middle.txt"))
	assert.FileExists(t, filepath.Join(dir, "newest.txt"))
}

func TestTrim_DryRun(t *testing.T) {
	dir := t.TempDir()

	createTestFile(t, filepath.Join(dir, "old.txt"), 1000, 40*24*time.Hour)

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 10000,
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  true,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(1000), result.FreedBytes)
	assert.Equal(t, int64(1), result.DeletedCount)
	assert.Contains(t, result.Output, "would delete")

	// File should still exist
	assert.FileExists(t, filepath.Join(dir, "old.txt"))
}

func TestTrim_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 1000,
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  false,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(0), result.FreedBytes)
	assert.Equal(t, int64(0), result.DeletedCount)
	assert.Contains(t, result.Output, "no files found")
}

func TestTrim_AlreadyUnderLimit(t *testing.T) {
	dir := t.TempDir()

	// All files are recent and under size limit
	createTestFile(t, filepath.Join(dir, "recent.txt"), 100, 1*24*time.Hour)

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 10000,
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  false,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(0), result.FreedBytes)
	assert.Equal(t, int64(0), result.DeletedCount)
	assert.FileExists(t, filepath.Join(dir, "recent.txt"))
}

func TestTrim_CombinesAgeAndSize(t *testing.T) {
	dir := t.TempDir()

	// Files: 1 old (age), 2 recent but over size
	createTestFile(t, filepath.Join(dir, "ancient.txt"), 500, 60*24*time.Hour) // Age-based deletion
	createTestFile(t, filepath.Join(dir, "old.txt"), 500, 15*24*time.Hour)     // Size-based (oldest remaining)
	createTestFile(t, filepath.Join(dir, "recent.txt"), 500, 5*24*time.Hour)   // Keep

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 1000, // Target: 900 bytes after buffer
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  false,
	})

	require.NoError(t, err)
	// ancient.txt deleted by age, old.txt deleted by size
	assert.Equal(t, int64(1000), result.FreedBytes)
	assert.Equal(t, int64(2), result.DeletedCount)

	assert.NoFileExists(t, filepath.Join(dir, "ancient.txt"))
	assert.NoFileExists(t, filepath.Join(dir, "old.txt"))
	assert.FileExists(t, filepath.Join(dir, "recent.txt"))
}

func TestTrim_ContextCancellation(t *testing.T) {
	dir := t.TempDir()

	createTestFile(t, filepath.Join(dir, "file1.txt"), 1000, 40*24*time.Hour)
	createTestFile(t, filepath.Join(dir, "file2.txt"), 1000, 41*24*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := Trim(ctx, []string{dir}, TrimOptions{
		MaxSize: 100,
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  false,
	})

	// A cancelled context aborts the file scan before any deletion.
	assert.ErrorIs(t, err, context.Canceled)
}

func TestTrim_MultiplePaths(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	createTestFile(t, filepath.Join(dir1, "old1.txt"), 500, 40*24*time.Hour)
	createTestFile(t, filepath.Join(dir2, "old2.txt"), 500, 45*24*time.Hour)

	result, err := Trim(context.Background(), []string{dir1, dir2}, TrimOptions{
		MaxSize: 10000,
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  false,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(1000), result.FreedBytes)
	assert.Equal(t, int64(2), result.DeletedCount)
}

func TestTrim_DeleteError(t *testing.T) {
	skipWithoutModeBits(t)
	if os.Getuid() == 0 {
		t.Skip("skipping permission test as root")
	}

	dir := t.TempDir()

	// Create old file
	oldFile := filepath.Join(dir, "old.txt")
	createTestFile(t, oldFile, 1000, 40*24*time.Hour)

	// Make directory read-only so delete fails
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 10000,
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  false,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(0), result.FreedBytes)
	assert.Equal(t, int64(0), result.DeletedCount)
	assert.Len(t, result.Errors, 1)
	assert.Equal(t, ReasonPermissionDenied, result.Errors[0].Reason)
}

func TestTrim_CarriesForwardScanWarnings(t *testing.T) {
	skipWithoutModeBits(t)
	if os.Getuid() == 0 {
		t.Skip("skipping permission test as root")
	}

	dir := t.TempDir()

	// Create accessible file
	createTestFile(t, filepath.Join(dir, "accessible.txt"), 100, 1*24*time.Hour)

	// Create inaccessible subdirectory
	inaccessible := filepath.Join(dir, "noaccess")
	require.NoError(t, os.Mkdir(inaccessible, 0o000))
	t.Cleanup(func() { _ = os.Chmod(inaccessible, 0o750) })

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 10000,
		MaxAge:  30 * 24 * time.Hour,
		DryRun:  false,
	})

	require.NoError(t, err)
	// Should have warning from scan
	assert.NotEmpty(t, result.Errors)
}

func TestTrim_ReportsRemovedFiles(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		dir := t.TempDir()
		oldFile := filepath.Join(dir, "old.txt")
		createTestFile(t, oldFile, 700, 40*24*time.Hour)
		createTestFile(t, filepath.Join(dir, "new.txt"), 100, time.Hour)

		result, err := Trim(context.Background(), []string{dir}, TrimOptions{MaxSize: 1 << 20, MaxAge: 30 * 24 * time.Hour, DryRun: dryRun})
		require.NoError(t, err)

		require.Len(t, result.Removed, 1)
		assert.Equal(t, oldFile, result.Removed[0].Path)
		assert.Equal(t, int64(700), result.Removed[0].Size)
	}
}

func linkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Link(oldname, newname); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	mtime := time.Now().Add(-40 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(newname, mtime, mtime))
}

func TestTrim_CreditsSharedInodeOnceWhenAllLinksGo(t *testing.T) {
	for _, dry := range []bool{true, false} {
		dir := t.TempDir()
		first := filepath.Join(dir, "a.bin")
		createTestFile(t, first, 1000, 40*24*time.Hour)
		linkOrSkip(t, first, filepath.Join(dir, "b.bin"))

		result, err := Trim(context.Background(), []string{dir}, TrimOptions{
			MaxSize: 10000, MaxAge: 30 * 24 * time.Hour, DryRun: dry,
		})

		require.NoError(t, err)
		assert.Equal(t, int64(1000), result.FreedBytes, "dry=%v", dry)
		assert.Equal(t, int64(2), result.DeletedCount, "dry=%v", dry)
	}
}

func TestTrim_LinkOutsideTheCacheKeepsTheBytes(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	first := filepath.Join(dir, "a.bin")
	createTestFile(t, first, 1000, 40*24*time.Hour)
	linkOrSkip(t, first, filepath.Join(elsewhere, "keep.bin"))

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 10000, MaxAge: 30 * 24 * time.Hour,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(0), result.FreedBytes)
	assert.Equal(t, int64(1), result.DeletedCount)
}

func TestTrim_SizeTrimCountsSharedInodeOnce(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.bin")
	createTestFile(t, first, 1000, 40*24*time.Hour)
	linkOrSkip(t, first, filepath.Join(dir, "b.bin"))
	createTestFile(t, filepath.Join(dir, "new.bin"), 1000, time.Hour)

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 1500, MaxAge: 90 * 24 * time.Hour,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(1000), result.FreedBytes)
	assert.Equal(t, int64(2), result.DeletedCount)
	assert.FileExists(t, filepath.Join(dir, "new.bin"))
}

func TestTrim_OverlappingPathsListEachFileOnce(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	first := filepath.Join(dir, "sub", "a.bin")
	createTestFile(t, first, 1000, 40*24*time.Hour)
	linkOrSkip(t, first, filepath.Join(elsewhere, "keep.bin"))

	result, err := Trim(context.Background(), []string{dir, filepath.Join(dir, "sub")}, TrimOptions{
		MaxSize: 10000, MaxAge: 30 * 24 * time.Hour, DryRun: true,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(0), result.FreedBytes, "the outside link keeps the inode")
	assert.Equal(t, int64(1), result.DeletedCount)
}

func TestTrim_SizeTrimLeavesInodesItCannotFree(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	big := filepath.Join(dir, "big.bin")
	createTestFile(t, big, 8000, 20*24*time.Hour)
	linkOrSkip(t, big, filepath.Join(elsewhere, "keep.bin"))
	for i, name := range []string{"a", "b", "c"} {
		createTestFile(t, filepath.Join(dir, name), 1000, time.Duration(10-i)*24*time.Hour)
	}

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 10500, MaxAge: 90 * 24 * time.Hour,
	})

	require.NoError(t, err)
	assert.FileExists(t, big, "removing the link frees nothing")
	assert.Equal(t, int64(2000), result.FreedBytes)
	assert.Equal(t, int64(2), result.DeletedCount)
}

func TestCalculateSize_OverlappingAndAliasedPathsCountOnce(t *testing.T) {
	root := t.TempDir()
	createTestFile(t, filepath.Join(root, "a", "x.bin"), 100, time.Hour)
	createTestFile(t, filepath.Join(root, "a", "b", "y.bin"), 50, time.Hour)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	res, err := CalculateSize([]string{root, filepath.Join(root, "a"), filepath.Join(alias, "a", "b")})

	require.NoError(t, err)
	assert.Equal(t, int64(150), res.Size)
	list, err := ListFiles([]string{filepath.Join(alias, "a"), filepath.Join(root, "a", "b"), root})
	require.NoError(t, err)
	assert.Len(t, list.Files, 2)
}

func TestTrim_RemovesSharedInodeWholeOrNotAtAll(t *testing.T) {
	dir := t.TempDir()
	a1 := filepath.Join(dir, "a1")
	b1 := filepath.Join(dir, "b1")
	createTestFile(t, a1, 1000, 20*24*time.Hour)
	createTestFile(t, b1, 1000, 15*24*time.Hour)
	for _, l := range []struct {
		from, to string
		age      time.Duration
	}{{a1, "a2", 10 * 24 * time.Hour}, {b1, "b2", 5 * 24 * time.Hour}} {
		to := filepath.Join(dir, l.to)
		linkOrSkip(t, l.from, to)
		mtime := time.Now().Add(-l.age)
		require.NoError(t, os.Chtimes(to, mtime, mtime))
	}

	result, err := Trim(context.Background(), []string{dir}, TrimOptions{
		MaxSize: 1200, MaxAge: 90 * 24 * time.Hour,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(1000), result.FreedBytes)
	assert.Equal(t, int64(2), result.DeletedCount)
	assert.FileExists(t, b1, "an inode is not left with some links removed")
	assert.FileExists(t, filepath.Join(dir, "b2"))
}

func TestCalculateSize_DuplicateFileRootsCountOnce(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "f.bin")
	createTestFile(t, file, 300, time.Hour)

	res, err := CalculateSize([]string{file, file, filepath.Join(root, "..", filepath.Base(root), "f.bin")})
	require.NoError(t, err)
	assert.Equal(t, int64(300), res.Size)
	list, err := ListFiles([]string{file, file})
	require.NoError(t, err)
	assert.Len(t, list.Files, 1)
}

func TestDistinctRoots_FilesystemRootAbsorbsChildren(t *testing.T) {
	root := string(filepath.Separator)
	if v := filepath.VolumeName(t.TempDir()); v != "" {
		root = v + root
	}
	got := distinctRoots([]string{filepath.Join(root, "definitely-missing-x"), root, t.TempDir()})
	assert.Equal(t, []string{root}, got)
}

func TestDistinctRoots_RealFileBeatsSymlinkToIt(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "zfile")
	createTestFile(t, file, 300, time.Hour)
	link := filepath.Join(root, "alink")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	assert.Equal(t, []string{file}, distinctRoots([]string{link, file}))
	assert.Equal(t, []string{file}, distinctRoots([]string{file, link}))
	res, err := CalculateSize([]string{link, file})
	require.NoError(t, err)
	assert.Equal(t, int64(300), res.Size)
	list, err := ListFiles([]string{link, file})
	require.NoError(t, err)
	require.Len(t, list.Files, 1)
	assert.Equal(t, file, list.Files[0].Path)
}

func TestDistinctRoots_CaseVariantSpellingsOfOneDirectoryCountOnce(t *testing.T) {
	root := t.TempDir()
	createTestFile(t, filepath.Join(root, "Cache", "sub", "x.bin"), 100, time.Hour)
	if _, err := os.Stat(filepath.Join(root, "cache")); err != nil {
		t.Skip("file system is case-sensitive")
	}

	got := distinctRoots([]string{filepath.Join(root, "Cache"), filepath.Join(root, "cache")})
	assert.Len(t, got, 1)
	got = distinctRoots([]string{filepath.Join(root, "Cache"), filepath.Join(root, "cache", "sub")})
	assert.Equal(t, []string{filepath.Join(root, "Cache")}, got)
	res, err := CalculateSize([]string{filepath.Join(root, "Cache"), filepath.Join(root, "cache", "sub")})
	require.NoError(t, err)
	assert.Equal(t, int64(100), res.Size)
}

func TestScan_SymlinkRootIsNotFollowed(t *testing.T) {
	target := t.TempDir()
	createTestFile(t, filepath.Join(target, "x.bin"), 100, time.Hour)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	res, err := CalculateSize([]string{link})
	require.NoError(t, err)
	assert.Zero(t, res.Size)
	list, err := ListFiles([]string{link})
	require.NoError(t, err)
	assert.Empty(t, list.Files)
}

func TestDistinctRoots_HardLinkedFileRootsStayDistinct(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.bin")
	createTestFile(t, a, 300, 48*time.Hour)
	b := filepath.Join(root, "b.bin")
	linkOrSkip(t, a, b)

	assert.Len(t, distinctRoots([]string{a, b}), 2)
	result, err := Trim(context.Background(), []string{a, b}, TrimOptions{MaxSize: 10000, MaxAge: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.DeletedCount)
	assert.Equal(t, int64(300), result.FreedBytes)
	assert.NoFileExists(t, a)
	assert.NoFileExists(t, b)
}

func TestDistinctRoots_RepeatedMissingRootCountsOnce(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	assert.Equal(t, []string{missing}, distinctRoots([]string{missing, missing}))
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "nowhere"), dangling); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	assert.Equal(t, []string{dangling}, distinctRoots([]string{dangling, dangling}))
}

func TestDistinctRoots_CaseVariantSpellingsOfOneFileCountOnce(t *testing.T) {
	root := t.TempDir()
	createTestFile(t, filepath.Join(root, "File"), 100, time.Hour)
	if _, err := os.Stat(filepath.Join(root, "file")); err != nil {
		t.Skip("file system is case-sensitive")
	}

	res, err := CalculateSize([]string{filepath.Join(root, "File"), filepath.Join(root, "file")})
	require.NoError(t, err)
	assert.Equal(t, int64(100), res.Size)
}
