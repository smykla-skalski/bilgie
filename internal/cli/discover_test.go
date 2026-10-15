package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/discover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const discoverConfig = `# my overrides
version: "1"
auto:
  min_free: 40G
providers:
  pip:
    enabled: false
`

type discoverFixture struct {
	t        *testing.T
	home     string
	cfgPath  string
	loader   *config.Loader
	env      discoverEnv
	out      *bytes.Buffer
	platform config.Platform
}

func newDiscoverFixture(t *testing.T) *discoverFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA"} {
		t.Setenv(key, home)
	}
	tmp := filepath.Join(home, "tmp")
	require.NoError(t, os.MkdirAll(tmp, 0o750))
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, tmp)
	}
	for _, key := range []string{"LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "UV_CACHE_DIR"} {
		t.Setenv(key, "")
	}

	platform := config.CurrentPlatform()
	platform.SystemTempDir = ""
	cfgPath := filepath.Join(home, "cfg", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgPath), 0o750))
	require.NoError(t, os.WriteFile(cfgPath, []byte(discoverConfig), 0o600))
	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SetPlatform(platform)

	f := &discoverFixture{t: t, home: home, cfgPath: cfgPath, loader: loader, out: &bytes.Buffer{}, platform: platform}
	f.env = discoverEnv{
		out:          f.out,
		in:           strings.NewReader(""),
		now:          time.Now,
		lookPath:     func(string) (string, error) { return "", errors.New("not found") },
		home:         home,
		platform:     platform,
		unmanagedMin: 1,
	}
	return f
}

func (f *discoverFixture) cache(name string, size int) {
	f.t.Helper()
	paths, err := config.ExpandPaths(config.DefaultProvidersFor(f.platform)[name].Paths)
	require.NoError(f.t, err)
	require.NotEmpty(f.t, paths)
	require.NoError(f.t, os.MkdirAll(paths[0], 0o750))
	require.NoError(f.t, os.WriteFile(filepath.Join(paths[0], "blob"), make([]byte, size), 0o600))
}

func (f *discoverFixture) run(flags discoverFlags, names ...string) error {
	f.t.Helper()
	f.out.Reset()
	return runDiscoverWithLoader(context.Background(), f.loader, f.env, flags, names)
}

func (f *discoverFixture) config() string {
	f.t.Helper()
	data, err := os.ReadFile(f.cfgPath)
	require.NoError(f.t, err)
	return string(data)
}

func (f *discoverFixture) withSuggestions() {
	f.cache("pip", 4096)
	f.cache("huggingface", 8192)
	f.cache("go-build", 2048)
}

func TestDiscover_TextAndJSONListTheSameCandidates(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()
	cacheRoot := filepath.Join(f.home, ".cache")
	require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, "bigtool"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cacheRoot, "bigtool", "blob"), make([]byte, 1<<16), 0o600))
	f.env.roots = []string{cacheRoot}

	require.NoError(t, f.run(discoverFlags{json: true, unmanaged: 5}))
	var got DiscoverOutput
	require.NoError(t, json.Unmarshal(f.out.Bytes(), &got))
	require.NoError(t, f.run(discoverFlags{unmanaged: 5}))
	text := f.out.String()

	require.GreaterOrEqual(t, len(got.Candidates), 4)
	for _, c := range got.Candidates {
		var line string
		for l := range strings.SplitSeq(text, "\n") {
			if strings.HasPrefix(l, c.Name+" ") {
				line = l
				break
			}
		}
		require.NotEmpty(t, line, "text output lacks %s", c.Name)
		assert.Contains(t, line, c.Size, c.Name)
		assert.Contains(t, line, string(c.Tier), c.Name)
		assert.Contains(t, line, string(c.Action), c.Name)
	}

	byName := map[string]discover.Candidate{}
	for _, c := range got.Candidates {
		byName[c.Name] = c
	}
	assert.Equal(t, discover.TierRegenerable, byName["pip"].Tier)
	assert.Equal(t, discover.TierOptIn, byName["huggingface"].Tier)
	assert.Equal(t, discover.TierRegenerable, byName["custom-bigtool"].Tier)
	assert.Contains(t, text, "Nothing was changed")
}

func TestDiscover_WriteYesAddsOnlyRegenerableSuggestionsAndKeepsTheRest(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()

	require.NoError(t, f.run(discoverFlags{write: true, yes: true}))

	assert.Contains(t, f.out.String(), "Wrote 1 provider(s)")
	assert.YAMLEq(t, `
version: "1"
auto:
  min_free: 40G
providers:
  pip:
    enabled: true
`, f.config())
	assert.Contains(t, f.config(), "# my overrides")
	cfg, err := f.loader.Load()
	require.NoError(t, err)
	assert.True(t, cfg.Providers["pip"].Enabled)
	assert.False(t, cfg.Providers["huggingface"].Enabled)
	assert.Equal(t, "40G", cfg.Auto.MinFree)
}

func TestDiscover_WriteYesWithANameAcceptsAnOptInProvider(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()

	require.NoError(t, f.run(discoverFlags{write: true, yes: true}, "huggingface"))

	cfg, err := f.loader.Load()
	require.NoError(t, err)
	assert.True(t, cfg.Providers["huggingface"].Enabled)
	assert.False(t, cfg.Providers["pip"].Enabled)
	assert.NotContains(t, f.config(), "go-build")
}

func TestDiscover_InteractiveAsksPerProvider(t *testing.T) {
	tests := []struct {
		name    string
		answers string
		want    map[string]bool
	}{
		{name: "first only", answers: "y\nn\n", want: map[string]bool{"pip": true, "huggingface": false}},
		{name: "second only", answers: "n\nyes\n", want: map[string]bool{"pip": false, "huggingface": true}},
		{name: "both", answers: "y\ny\n", want: map[string]bool{"pip": true, "huggingface": true}},
		{name: "none", answers: "\n\n", want: map[string]bool{"pip": false, "huggingface": false}},
		{name: "input ends", answers: "y\n", want: map[string]bool{"pip": true, "huggingface": false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiscoverFixture(t)
			f.withSuggestions()
			f.env.interactive = true
			f.env.in = strings.NewReader(tc.answers)

			require.NoError(t, f.run(discoverFlags{write: true}))

			cfg, err := f.loader.Load()
			require.NoError(t, err)
			for name, enabled := range tc.want {
				assert.Equal(t, enabled, cfg.Providers[name].Enabled, name)
			}
			assert.Contains(t, f.out.String(), "Enable it? [y/N]")
		})
	}
}

func TestDiscover_RejectsUnsafeFlagCombinations(t *testing.T) {
	tests := []struct {
		name        string
		flags       discoverFlags
		interactive bool
		wantErr     string
	}{
		{name: "yes without write", flags: discoverFlags{yes: true}, wantErr: "--yes needs --write"},
		{name: "write json without yes", flags: discoverFlags{write: true, json: true}, interactive: true, wantErr: "needs --yes"},
		{name: "write without terminal", flags: discoverFlags{write: true}, wantErr: "not a terminal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiscoverFixture(t)
			f.withSuggestions()
			f.env.interactive = tc.interactive
			before := f.config()

			err := f.run(tc.flags)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, before, f.config())
		})
	}
}

func TestDiscover_NeverEnablesProtectedPaths(t *testing.T) {
	f := newDiscoverFixture(t)
	downloads := filepath.Join(f.home, "Downloads", "pip-cache")
	require.NoError(t, os.MkdirAll(downloads, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(downloads, "blob"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(f.cfgPath, []byte("providers:\n  pip:\n    enabled: false\n    paths:\n      - "+filepath.ToSlash(downloads)+"\n"), 0o600))
	before := f.config()

	require.NoError(t, f.run(discoverFlags{write: true, yes: true}))
	assert.Equal(t, before, f.config())
	assert.Contains(t, f.out.String(), "Nothing written")

	require.NoError(t, f.run(discoverFlags{json: true}))
	var got DiscoverOutput
	require.NoError(t, json.Unmarshal(f.out.Bytes(), &got))
	for _, c := range got.Candidates {
		if c.Name == "pip" {
			assert.Equal(t, discover.TierProtected, c.Tier)
			assert.Equal(t, discover.ActionNever, c.Action)
		}
	}

	err := f.run(discoverFlags{write: true, yes: true}, "pip")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "protected")
	assert.Equal(t, before, f.config())
}

func TestDiscover_RejectsUnknownNames(t *testing.T) {
	f := newDiscoverFixture(t)

	err := f.run(discoverFlags{}, "no-such-provider")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-provider")
}

func TestDiscover_WritesACustomEntryForAnUnmanagedDirectory(t *testing.T) {
	f := newDiscoverFixture(t)
	cacheRoot := filepath.Join(f.home, ".cache")
	for _, dir := range []string{"bigtool", "opencode"} {
		require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, dir), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(cacheRoot, dir, "blob"), make([]byte, 1<<16), 0o600))
	}
	f.env.roots = []string{cacheRoot}

	require.NoError(t, f.run(discoverFlags{write: true, yes: true, unmanaged: 5}, "custom-bigtool"))

	cfg, err := f.loader.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	got := cfg.Providers["custom-bigtool"]
	assert.True(t, got.Enabled)
	assert.Equal(t, config.TypeDirPattern, got.Type)
	assert.Equal(t, []string{"~/.cache/[b]igtool"}, got.Paths)

	protected := filepath.Join(cacheRoot, "opencode")
	err = f.run(discoverFlags{write: true, yes: true, unmanaged: 5}, protected)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "protected")
	assert.NotContains(t, f.config(), "opencode")
}

func TestDiscover_ReportsNothingToSuggestOnAnEmptyMachine(t *testing.T) {
	f := newDiscoverFixture(t)

	require.NoError(t, f.run(discoverFlags{}))

	assert.Contains(t, f.out.String(), "Nothing to suggest")
	require.NoError(t, f.run(discoverFlags{json: true}))
	var got DiscoverOutput
	require.NoError(t, json.Unmarshal(f.out.Bytes(), &got))
	assert.Empty(t, got.Candidates)
	assert.Contains(t, f.out.String(), `"candidates": []`)
}

func TestDiscover_ListsUndetectedProvidersWithAll(t *testing.T) {
	f := newDiscoverFixture(t)

	require.NoError(t, f.run(discoverFlags{json: true, all: true}))

	var got DiscoverOutput
	require.NoError(t, json.Unmarshal(f.out.Bytes(), &got))
	require.NotEmpty(t, got.Candidates)
	for _, c := range got.Candidates {
		assert.False(t, c.Detected(), c.Name)
		assert.False(t, c.Suggested(), c.Name)
	}
}

func TestSuggestionNames_ListsWhatDiscoverOffers(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()
	cfg, err := f.loader.Load()
	require.NoError(t, err)

	names := suggestionNames(context.Background(), cfg, f.home, f.env.lookPath, nil)

	assert.ElementsMatch(t, []string{"pip", "huggingface"}, names)
}

func TestNoteSuggestions_PrintsACountAndChangesNothing(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()
	cfg, err := f.loader.Load()
	require.NoError(t, err)
	before := f.config()
	var out bytes.Buffer

	noteSuggestions(context.Background(), cfg, autoEnv{out: &out, home: f.home, lookPath: f.env.lookPath})

	assert.Contains(t, out.String(), "discover: 2 provider(s) could be enabled")
	assert.Contains(t, out.String(), "nothing was changed")
	assert.Equal(t, before, f.config())
}

func TestNoteSuggestions_StaysSilentWithoutSuggestions(t *testing.T) {
	f := newDiscoverFixture(t)
	cfg, err := f.loader.Load()
	require.NoError(t, err)
	var out bytes.Buffer

	noteSuggestions(context.Background(), cfg, autoEnv{out: &out, home: f.home, lookPath: f.env.lookPath})

	assert.Empty(t, out.String())
}

func TestRunDiscover_ReadsItsFlagsAndTheRealHome(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()
	require.NoError(t, DiscoverCmd.Flags().Set("json", "true"))
	require.NoError(t, DiscoverCmd.Flags().Set("unmanaged", "0"))
	t.Cleanup(func() {
		require.NoError(t, DiscoverCmd.Flags().Set("json", "false"))
		require.NoError(t, DiscoverCmd.Flags().Set("unmanaged", "10"))
	})

	var err error
	out := captureStdout(t, func() { err = runDiscover(DiscoverCmd, nil) })

	require.NoError(t, err)
	var got DiscoverOutput
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.NotEmpty(t, got.Candidates)

	require.NoError(t, DiscoverCmd.Flags().Set("unmanaged", "-1"))
	assert.ErrorContains(t, runDiscover(DiscoverCmd, nil), "must not be negative")
}

func TestPrintPaths_CapsTheList(t *testing.T) {
	var out bytes.Buffer

	printPaths(&out, []string{"/a", "/b", "/c", "/d", "/e"})

	assert.Contains(t, out.String(), "/c")
	assert.NotContains(t, out.String(), "/d")
	assert.Contains(t, out.String(), "and 2 more")
}

func TestDefaultDiscoverEnv_TargetsTheRunningMachine(t *testing.T) {
	f := newDiscoverFixture(t)

	env := defaultDiscoverEnv()

	assert.Equal(t, f.home, env.home)
	assert.NotNil(t, env.lookPath)
	assert.NotNil(t, env.now)
}
