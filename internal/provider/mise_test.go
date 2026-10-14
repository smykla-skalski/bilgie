package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type miseFixture struct {
	root   string
	node   string
	python string
	marker string
}

func newMiseFixture(t *testing.T) miseFixture {
	t.Helper()
	root := t.TempDir()
	f := miseFixture{
		root:   root,
		node:   filepath.Join(root, "installs", "node", "20.0.0"),
		python: filepath.Join(root, "installs", "python", "3.11.2"),
		marker: filepath.Join(t.TempDir(), "yes-called"),
	}
	writeAged(t, filepath.Join(f.node, "bin", "node"), 3000, time.Hour)
	writeAged(t, filepath.Join(f.python, "bin", "python"), 1000, time.Hour)
	writeAged(t, filepath.Join(root, "installs", "node", "22.1.0", "bin", "node"), 500, time.Hour)
	return f
}

func (f miseFixture) listing() string {
	return "rm -rf " + f.node + "\nrm -rf " + f.python + "\n"
}

func newMise(t *testing.T, root string, cfg config.Provider) *MiseProvider {
	t.Helper()
	cfg.Enabled = true
	cfg.Paths = []string{root}
	if cfg.MaxSize == "" {
		cfg.MaxSize = "1G"
	}
	if cfg.MaxAge == "" {
		cfg.MaxAge = "30d"
	}
	p, err := NewMiseProvider("mise", cfg)
	require.NoError(t, err)
	p.procLines = func(context.Context) ([]string, error) { return nil, nil }
	p.busy = nil
	p.protected = nil
	return p
}

func TestMise_DryRunListsVersionsAndSizesAndDeletesNothing(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stdout: f.listing()},
			"prune --yes":     {Touch: f.marker},
		},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})

	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	assert.Contains(t, res.Output, "would prune: node@20.0.0 (")
	assert.Contains(t, res.Output, "would prune: python@3.11.2 (")
	assert.NotContains(t, res.Output, "size unknown")
	assert.Equal(t, int64(4000), res.BytesCleaned)
	require.Len(t, res.Entries, 2)
	assert.Equal(t, "node@20.0.0", res.Entries[0].Detail)
	assert.Equal(t, int64(3000), res.Entries[0].Size)
	assert.DirExists(t, f.node)
	assert.DirExists(t, f.python)
	assert.NoFileExists(t, f.marker)
}

func TestMise_RealRunCallsPruneYesOnce(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stdout: f.listing()},
			"prune --yes": {
				Remove: []string{f.node}, Touch: f.marker,
				IfExists: f.marker, Then: &fakeReply{Stderr: "called twice", Exit: 9},
			},
		},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

	require.NoError(t, err)
	assert.FileExists(t, f.marker)
	assert.NoDirExists(t, f.node)
	assert.DirExists(t, f.python, "mise kept it, bilgie reports it")
	assert.Contains(t, res.Output, "pruned: node@20.0.0 (")
	assert.Contains(t, res.Output, "kept: python@3.11.2")
	assert.Equal(t, int64(3000), res.BytesCleaned)
	assert.Equal(t, 1, res.SkippedEntries)
	require.Len(t, res.Entries, 1)
	assert.DirExists(t, filepath.Join(f.root, "installs", "node", "22.1.0"))
}

func TestMise_NothingToPruneDoesNotCallPrune(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{"prune --yes": {Touch: f.marker}},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})

	require.NoError(t, err)
	assert.Contains(t, res.Output, "no unused tool versions")
	assert.NoFileExists(t, f.marker)
}

func TestMise_FullModeBehavesLikeSmart(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stdout: f.listing()},
			"prune --yes":     {Remove: []string{f.node, f.python}},
		},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})

	require.NoError(t, err)
	assert.Equal(t, int64(4000), res.BytesCleaned)
	assert.NoDirExists(t, f.python)
}

func TestMise_AtVersionListing(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{"prune --dry-run": {Stdout: "node@20.0.0\nghost@1.0\n"}},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})

	require.NoError(t, err)
	assert.Contains(t, res.Output, "would prune: node@20.0.0 (")
	assert.Contains(t, res.Output, "would prune: ghost@1.0 (size unknown)")
}

func TestMise_ListingErrorSkipsWithoutDeleting(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stderr: "config error: boom\nmore", Exit: 1},
			"prune --yes":     {Touch: f.marker, Remove: []string{f.node}},
		},
	})
	p := newMise(t, f.root, config.Provider{})
	writeAged(t, filepath.Join(f.root, "downloads", "x", "old.tgz"), 10, 90*24*time.Hour)

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "mise prune --dry-run failed")
	assert.Contains(t, res.SkipReason, "config error: boom")
	assert.NoFileExists(t, f.marker)
	assert.DirExists(t, f.node)
	assert.FileExists(t, filepath.Join(f.root, "downloads", "x", "old.tgz"))
}

func TestMise_UnparseableOutputSkips(t *testing.T) {
	for name, stdout := range map[string]string{
		"prose":            "Pruning is a state of mind\n",
		"outside installs": "rm -rf /usr/local/bin/node\n",
		"too deep":         "rm -rf INSTALLS/node/20/bin\n",
		"too shallow":      "rm -rf INSTALLS/node\n",
		"installs itself":  "rm -rf INSTALLS\n",
		"good then bad":    "rm -rf INSTALLS/node/20.0.0\nwhat\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newMiseFixture(t)
			stdout = strings.ReplaceAll(stdout, "INSTALLS", filepath.Join(f.root, "installs"))
			installFakeTool(t, "mise", fakeToolSpec{
				Replies: map[string]fakeReply{
					"prune --dry-run": {Stdout: stdout},
					"prune --yes":     {Touch: f.marker},
				},
			})
			p := newMise(t, f.root, config.Provider{})

			res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

			require.NoError(t, err)
			assert.Contains(t, res.SkipReason, "cannot parse")
			assert.NoFileExists(t, f.marker)
			assert.DirExists(t, f.node)
		})
	}
}

func TestMise_StderrNoiseIsNotAListing(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{"prune --dry-run": {Stdout: f.listing(), Stderr: "mise WARN something\n"}},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})

	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	assert.Len(t, res.Entries, 2)
}

func TestMise_PluginGitClonesDoNotCauseSkip(t *testing.T) {
	f := newMiseFixture(t)
	for _, plugin := range []string{"lua", "make", "teleport-ent"} {
		require.NoError(t, os.MkdirAll(filepath.Join(f.root, "plugins", plugin, ".git"), 0o750))
		writeAged(t, filepath.Join(f.root, "plugins", plugin, "bin", "install"), 10, 90*24*time.Hour)
	}
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stdout: f.listing()},
			"prune --yes":     {Remove: []string{f.node, f.python}},
		},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	assert.NoDirExists(t, f.node)
	for _, plugin := range []string{"lua", "make", "teleport-ent"} {
		assert.FileExists(t, filepath.Join(f.root, "plugins", plugin, "bin", "install"))
		assert.DirExists(t, filepath.Join(f.root, "plugins", plugin, ".git"))
	}
}

func TestMise_RunningManagedBinaryDoesNotSkip(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stdout: f.listing()},
			"prune --yes":     {Remove: []string{f.node, f.python}},
		},
	})
	p := newMise(t, f.root, config.Provider{})
	bin := filepath.Join(f.root, "installs", "node", "22.1.0", "bin", "node")
	p.procLines = func(context.Context) ([]string, error) {
		return []string{"/usr/bin/true", bin + " server.js"}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	assert.NoDirExists(t, f.node)
}

func TestMise_ProcessListFailureSkips(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{"prune --dry-run": {Touch: f.marker}}})
	p := newMise(t, f.root, config.Provider{})
	p.procLines = func(context.Context) ([]string, error) { return nil, assert.AnError }

	res, err := p.Clean(context.Background(), CleanOptions{})

	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "cannot list processes")
	assert.NoFileExists(t, f.marker)
}

func TestMise_MutatingMiseSkips(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{"prune --dry-run": {Touch: f.marker}}})
	p := newMise(t, f.root, config.Provider{})
	p.procLines = func(context.Context) ([]string, error) {
		return []string{"/opt/homebrew/bin/mise run dev", "/opt/homebrew/bin/mise install node@22"}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{})

	require.NoError(t, err)
	assert.Equal(t, "mise install is running", res.SkipReason)
	assert.NoFileExists(t, f.marker)
}

func TestMise_LongRunningMiseTaskDoesNotSkip(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{"prune --dry-run": {}}})
	p := newMise(t, f.root, config.Provider{})
	p.procLines = func(context.Context) ([]string, error) {
		return []string{"/opt/homebrew/bin/mise dev", "mise run dev", "mise watch build"}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})

	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
}

func TestMiseMutatingCommand(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"mise install", "install"},
		{"/usr/local/bin/mise install node@22", "install"},
		{"mise i", "i"},
		{"mise upgrade", "upgrade"},
		{"mise up --bump", "up"},
		{"mise prune --yes", "prune"},
		{"mise use -g node@22", "use"},
		{"mise -q install", "install"},
		{"mise --cd /work/proj install", "install"},
		{"mise -C /work use node", "use"},
		{"mise dev", ""},
		{"mise run install", ""},
		{"mise exec node@22 -- install", ""},
		{"mise", ""},
		{"", ""},
		{"/home/u/.local/share/mise/installs/node/22/bin/node install", ""},
		{"vim mise install", ""},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			assert.Equal(t, tt.want, miseMutatingCommand(tt.line))
		})
	}
}

func TestMise_ProcessMentioningMiseDirIsNotBusy(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{"prune --dry-run": {}}})
	p := newMise(t, f.root, config.Provider{})
	p.procLines = func(context.Context) ([]string, error) {
		return []string{"vim " + filepath.Join(f.root, "..", "mise"), "ls /home/u/.config/mise"}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})

	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
}

func TestMise_ListingTimeoutSkips(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {SleepMS: 60000},
			"prune --yes":     {Touch: f.marker},
		},
	})
	p := newMise(t, f.root, config.Provider{})
	p.timeout = 3 * time.Second

	start := time.Now()
	res, err := p.Clean(context.Background(), CleanOptions{})

	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "timed out")
	assert.Less(t, time.Since(start), 30*time.Second)
	assert.NoFileExists(t, f.marker)
}

func TestMise_PruneTimeoutIsAnError(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stdout: f.listing()},
			"prune --yes":     {SleepMS: 60000},
		},
	})
	p := newMise(t, f.root, config.Provider{})
	p.timeout = 3 * time.Second

	_, err := p.Clean(context.Background(), CleanOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestMise_PruneFailureIsAnError(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stdout: f.listing()},
			"prune --yes":     {Stderr: "denied", Exit: 1},
		},
	})
	p := newMise(t, f.root, config.Provider{})

	_, err := p.Clean(context.Background(), CleanOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied")
}

func TestMise_ProtectedPathsAreRefused(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{"prune --dry-run": {Touch: f.marker}}})

	for name, protected := range map[string]string{
		"equal":    f.root,
		"inside":   filepath.Join(f.root, "installs"),
		"contains": filepath.Dir(f.root),
	} {
		t.Run(name, func(t *testing.T) {
			p := newMise(t, f.root, config.Provider{})
			p.SetProtected([]string{protected})

			res, err := p.Clean(context.Background(), CleanOptions{})

			require.NoError(t, err)
			assert.Contains(t, res.SkipReason, "protected path")
			assert.NoFileExists(t, f.marker)
		})
	}
}

func TestMise_TrimsDownloadsAndCacheByAgeNotInstalls(t *testing.T) {
	f := newMiseFixture(t)
	cacheDir := t.TempDir()
	oldDownload := filepath.Join(f.root, "downloads", "node", "old.tgz")
	newDownload := filepath.Join(f.root, "downloads", "node", "new.tgz")
	oldCache := filepath.Join(cacheDir, "node", "list.msgpack")
	oldInstall := filepath.Join(f.root, "installs", "node", "22.1.0", "bin", "node")
	oldPlugin := filepath.Join(f.root, "plugins", "lua", "bin", "install")
	for _, path := range []string{oldDownload, oldCache, oldInstall, oldPlugin} {
		writeAged(t, path, 10, 90*24*time.Hour)
	}
	writeAged(t, newDownload, 10, time.Hour)
	installFakeTool(t, "mise", fakeToolSpec{})
	p := newMise(t, f.root, config.Provider{})
	p.paths = []string{f.root, cacheDir}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "would delete: "+oldDownload)
	assert.FileExists(t, oldDownload)

	_, err = p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.NoFileExists(t, oldDownload)
	assert.NoFileExists(t, oldCache)
	assert.FileExists(t, newDownload)
	assert.FileExists(t, oldInstall)
	assert.FileExists(t, oldPlugin)
}

func TestMise_DryRunDoesNotCountListedCacheTwice(t *testing.T) {
	f := newMiseFixture(t)
	cacheDir := filepath.Join(t.TempDir(), "mise")
	listedCache := filepath.Join(cacheDir, "node", "20.0.0")
	oldListed := filepath.Join(listedCache, "meta")
	oldStray := filepath.Join(cacheDir, "node", "list.msgpack")
	writeAged(t, oldListed, 200, 90*24*time.Hour)
	writeAged(t, oldStray, 50, 90*24*time.Hour)
	listing := "mise node@20.0.0 is prunable: node is not required\n" +
		"mise node@20.0.0 [dryrun]   remove " + f.node + ", " + listedCache + "\n"
	installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{"prune --dry-run": {Stdout: listing}}})
	p := newMise(t, f.root, config.Provider{})
	p.paths = []string{f.root, cacheDir}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})

	require.NoError(t, err)
	assert.Contains(t, res.Output, "would prune: node@20.0.0 (")
	assert.NotContains(t, res.Output, "would delete: "+oldListed)
	assert.Contains(t, res.Output, "would delete: "+oldStray)
	assert.Equal(t, int64(3000+200+50), res.BytesCleaned)
	assert.Equal(t, int64(1), res.FilesDeleted)
	var paths []string
	for _, e := range res.Entries {
		paths = append(paths, e.Path)
	}
	assert.NotContains(t, paths, oldListed)
	assert.Contains(t, paths, oldStray)
}

func TestMise_CleanCmdValidation(t *testing.T) {
	for _, cmd := range []string{"mise ls", "mise", "mise run prune", "mise exec node -- npm prune", "mise prune --dry-run", "mise prune --yes", "mise prune -ny", "mise prune -yn", "mise prune --dry-run=true", "mise prune -- node", "mise prune -Cstaging"} {
		_, err := NewMiseProvider("mise", config.Provider{Paths: []string{t.TempDir()}, MaxSize: "1G", CleanCmd: cmd})
		require.Error(t, err, cmd)
	}
	p, err := NewMiseProvider("mise", config.Provider{Paths: []string{t.TempDir()}, MaxSize: "1G"})
	require.NoError(t, err)
	assert.Equal(t, []string{"mise", "prune"}, p.cmdArgs)
	_, isAware := any(p).(ProtectionAware)
	assert.True(t, isAware)
}

func TestMise_TildeListingExpandsToHome(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", "mise")
	writeAged(t, filepath.Join(root, "installs", "node", "18.0.0", "bin", "node"), 700, time.Hour)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{"prune --dry-run": {Stdout: "rm -rf ~/.local/share/mise/installs/node/18.0.0\n"}},
	})
	p := newMise(t, root, config.Provider{})
	p.home = home

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})

	require.NoError(t, err)
	assert.Empty(t, res.SkipReason)
	assert.Equal(t, int64(700), res.BytesCleaned)
}

func TestMise_AtListingCannotEscapeInstalls(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{"prune --dry-run": {Stdout: "node@../../..\n"}},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})

	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "cannot parse")
}

func TestMise_UnknownVersionIsNotCountedAsPruned(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{"prune --dry-run": {Stdout: "ghost@1.0\n"}, "prune --yes": {}},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{})

	require.NoError(t, err)
	assert.Contains(t, res.Output, "unverified: ghost@1.0")
	assert.Empty(t, res.Entries)
	assert.Zero(t, res.FilesDeleted)
}

func TestMise_FailedPruneStillAccountsRemovedVersions(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{
			"prune --dry-run": {Stdout: f.listing()},
			"prune --yes":     {Remove: []string{f.node}, Stderr: "half done", Exit: 1},
		},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{})

	require.Error(t, err)
	assert.Equal(t, int64(3000), res.BytesCleaned)
	assert.Contains(t, res.Output, "pruned: node@20.0.0")
}

func TestMise_ListingOnStderrIsRead(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{"prune --dry-run": {Stderr: f.listing()}},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})

	require.NoError(t, err)
	assert.Len(t, res.Entries, 2)
}

func TestMise_DataDirWithoutDownloadsIsNeverTrimmedWhole(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "tracked-configs", "abc")
	writeAged(t, state, 10, 90*24*time.Hour)
	installFakeTool(t, "mise", fakeToolSpec{})
	p := newMise(t, root, config.Provider{})

	_, err := p.Clean(context.Background(), CleanOptions{})

	require.NoError(t, err)
	assert.FileExists(t, state)
}

func TestMise_StderrListingAmongWarningsIsRead(t *testing.T) {
	f := newMiseFixture(t)
	installFakeTool(t, "mise", fakeToolSpec{
		Replies: map[string]fakeReply{"prune --dry-run": {Stderr: "mise WARN deprecated\nmise rm -rf " + f.node + "\n"}},
	})
	p := newMise(t, f.root, config.Provider{})

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})

	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "node@20.0.0", res.Entries[0].Detail)
}

var realSampleInstalls = map[string]string{
	"claude-code": "2.1.283", "kubectl": "1.37.0", "lefthook": "2.1.15",
	"npm-redocly-cli": "2.46.1", "pi": "0.87.1", "pnpm": "10.34.5",
	"protoc": "36.0", "ruff": "0.16.7",
}

func realSampleFixture(t *testing.T) (home, root string) {
	t.Helper()
	home = t.TempDir()
	root = filepath.Join(home, ".local", "share", "mise")
	for tool, version := range realSampleInstalls {
		writeAged(t, filepath.Join(root, "installs", tool, version, "bin", tool), 1000, time.Hour)
		if tool != "npm-redocly-cli" {
			writeAged(t, filepath.Join(home, "Library", "Caches", "mise", tool, version, "meta"), 100, time.Hour)
		}
	}
	return home, root
}

func readRealSample(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "mise-prune-dryrun.txt"))
	require.NoError(t, err)
	return string(raw)
}

func TestMise_RealDryRunFormatEndToEnd(t *testing.T) {
	for name, reply := range map[string]func(string) fakeReply{
		"stdout": func(s string) fakeReply { return fakeReply{Stdout: s} },
		"stderr": func(s string) fakeReply { return fakeReply{Stderr: s} },
	} {
		t.Run(name, func(t *testing.T) {
			home, root := realSampleFixture(t)
			marker := filepath.Join(t.TempDir(), "yes")
			installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{
				"prune --dry-run": reply(readRealSample(t)),
				"prune --yes":     {Touch: marker},
			}})
			p := newMise(t, root, config.Provider{})
			p.home = home

			res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart, DryRun: true})

			require.NoError(t, err)
			assert.Empty(t, res.SkipReason)
			require.Len(t, res.Entries, 8)
			assert.Contains(t, res.Output, "would prune: npm:@redocly/cli@2.46.1 (")
			assert.Contains(t, res.Output, "would prune: kubectl@1.37.0 (")
			assert.NotContains(t, res.Output, "size unknown")
			assert.Equal(t, int64(8*1000+7*100), res.BytesCleaned)
			for tool, version := range realSampleInstalls {
				assert.DirExists(t, filepath.Join(root, "installs", tool, version))
			}
			assert.NoFileExists(t, marker)
		})
	}
}

func TestMise_RealFormatRealRunAccountsEachVersion(t *testing.T) {
	home, root := realSampleFixture(t)
	marker := filepath.Join(t.TempDir(), "yes")
	var gone []string
	for tool, version := range realSampleInstalls {
		if tool != "pnpm" {
			gone = append(gone, filepath.Join(root, "installs", tool, version))
		}
	}
	installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{
		"prune --dry-run": {Stdout: readRealSample(t)},
		"prune --yes": {
			Remove: gone, Touch: marker,
			IfExists: marker, Then: &fakeReply{Stderr: "twice", Exit: 9},
		},
	}})
	p := newMise(t, root, config.Provider{})
	p.home = home

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})

	require.NoError(t, err)
	assert.Len(t, res.Entries, 7)
	assert.Equal(t, 1, res.SkippedEntries)
	assert.Contains(t, res.Output, "kept: pnpm@10.34.5")
	assert.Contains(t, res.Output, "pruned: npm:@redocly/cli@2.46.1 (")
}

func TestMise_RealFormatUnexpectedLinesFailClosed(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"remove outside installs": func(s string) string {
			return strings.Replace(s, "remove ~/.local/share/mise/installs/pi/0.87.1", "remove ~/elsewhere/pi/0.87.1", 1)
		},
		"remove too deep": func(s string) string {
			return strings.Replace(s, "installs/pi/0.87.1", "installs/pi/x/0.87.1", 1)
		},
		"unknown dryrun action": func(s string) string {
			return strings.Replace(s, "[dryrun]        uninstall", "[dryrun]        explode", 1)
		},
		"stdout prose": func(s string) string { return s + "\nsomething unexpected\n" },
	} {
		t.Run(name, func(t *testing.T) {
			home, root := realSampleFixture(t)
			marker := filepath.Join(t.TempDir(), "yes")
			installFakeTool(t, "mise", fakeToolSpec{Replies: map[string]fakeReply{
				"prune --dry-run": {Stdout: edit(readRealSample(t))},
				"prune --yes":     {Touch: marker},
			}})
			p := newMise(t, root, config.Provider{})
			p.home = home

			res, err := p.Clean(context.Background(), CleanOptions{})

			require.NoError(t, err)
			assert.Contains(t, res.SkipReason, "cannot parse")
			assert.NoFileExists(t, marker)
		})
	}
}
