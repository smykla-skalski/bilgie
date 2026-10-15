package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireLinksEnv turns a link kind the host cannot create into a failure
// instead of a skip, so CI cannot pass without proving the behavior.
const requireLinksEnv = "BILGIE_REQUIRE_OS_LINKS"

type linkKind struct {
	name string
	make func(t *testing.T, link, target string)
}

var windowsLinkKinds = []linkKind{
	{name: "junction", make: makeJunction},
	{name: "dir-symlink", make: makeDirSymlink},
}

func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		unavailable(t, "junction", string(out), err)
	}
	info, statErr := os.Lstat(link)
	require.NoError(t, statErr)
	require.NotZero(t, info.Mode()&os.ModeIrregular|info.Mode()&os.ModeSymlink, "mklink /J must leave a reparse point, got %v", info.Mode())
}

func makeDirSymlink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		unavailable(t, "directory symlink", "", err)
	}
}

func unavailable(t *testing.T, what, out string, err error) {
	t.Helper()
	if os.Getenv(requireLinksEnv) != "" {
		t.Fatalf("cannot create a %s: %v %s", what, err, out)
	}
	t.Skipf("cannot create a %s: %v %s", what, err, out)
}

// precious is a directory outside every cleaned root, with a file that must
// survive whatever happens to a link pointing at it.
func precious(t *testing.T) (dir, file string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "precious")
	file = filepath.Join(dir, "sub", "data.bin")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o750))
	require.NoError(t, os.WriteFile(file, make([]byte, 64), 0o600))
	return dir, file
}

func assertIntact(t *testing.T, dir, file string) {
	t.Helper()
	info, err := os.Stat(file)
	require.NoError(t, err, "the link target lost its file")
	assert.Equal(t, int64(64), info.Size())
	assert.DirExists(t, dir)
}

func TestWindowsLinks_DirPatternNeverDeletesThroughLink(t *testing.T) {
	for _, kind := range windowsLinkKinds {
		t.Run(kind.name, func(t *testing.T) {
			root := t.TempDir()
			outside, file := precious(t)

			topLink := filepath.Join(root, "sail-link")
			kind.make(t, topLink, outside)

			inner := makeDir(t, root, "sail-inner", 10, 10*time.Hour)
			kind.make(t, filepath.Join(inner, "escape"), outside)

			p := newTestDirProvider(t, root, func(c *config.Provider) { c.MinIdle = "0s" })
			_, err := p.Clean(t.Context(), CleanOptions{})
			require.NoError(t, err)

			assertIntact(t, outside, file)
			assert.False(t, exists(inner), "the stale directory goes, link included")
		})
	}
}

func TestWindowsLinks_EvictTreeRemovesLinkNotTarget(t *testing.T) {
	for _, kind := range windowsLinkKinds {
		t.Run(kind.name, func(t *testing.T) {
			root := t.TempDir()
			outside, file := precious(t)

			tree := filepath.Join(root, "tree@v1")
			require.NoError(t, os.MkdirAll(filepath.Join(tree, "pkg"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(tree, "pkg", "a.go"), []byte("x"), 0o600))
			kind.make(t, filepath.Join(tree, "pkg", "escape"), outside)

			require.NoError(t, evictTree(tree))

			assertIntact(t, outside, file)
			assert.False(t, exists(tree))
		})
	}
}

func TestWindowsLinks_RemoveTreeOfLinkLeavesTarget(t *testing.T) {
	for _, kind := range windowsLinkKinds {
		t.Run(kind.name, func(t *testing.T) {
			outside, file := precious(t)
			link := filepath.Join(t.TempDir(), "link")
			kind.make(t, link, outside)

			require.NoError(t, removeTree(link))

			assertIntact(t, outside, file)
			assert.False(t, exists(link))
		})
	}
}
