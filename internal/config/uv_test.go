package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderPathsExist_UVFollowsCacheEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", "")
	moved := filepath.Join(t.TempDir(), "moved")
	t.Setenv("UV_CACHE_DIR", "")
	uv := DefaultProviders()["uv"]

	assert.False(t, ProviderPathsExist("uv", uv), "no default dir and no env")

	require.NoError(t, os.MkdirAll(moved, 0o700))
	t.Setenv("UV_CACHE_DIR", moved)
	assert.True(t, ProviderPathsExist("uv", uv), "UV_CACHE_DIR points at an existing dir")

	explicit := Provider{Paths: []string{filepath.Join(t.TempDir(), "absent")}}
	assert.False(t, ProviderPathsExist("uv", explicit), "an explicit path ignores the environment")
	assert.False(t, ProviderPathsExist("other", uv), "only uv resolves through the environment")
}

func TestProviderPathsExist_UVEnvDirIsLiteral(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", "")
	literal := filepath.Join(filepath.Clean(t.TempDir()), "cache[1]", "uv")
	require.NoError(t, os.MkdirAll(literal, 0o700))
	t.Setenv("UV_CACHE_DIR", literal)

	assert.True(t, ProviderPathsExist("uv", DefaultProviders()["uv"]))

	enabledUV := DefaultProviders()["uv"]
	enabledUV.Enabled = true
	cfg := &Config{Providers: map[string]Provider{"uv": enabledUV}}
	assert.Equal(t, []string{"uv"}, cfg.EnabledProviders())
}

func TestProviderPathsExist_UVBadPathsStayVisible(t *testing.T) {
	assert.True(t, ProviderPathsExist("uv", Provider{Paths: []string{}}), "a load error must name the provider")
	assert.True(t, ProviderPathsExist("uv", Provider{Paths: []string{""}}))
}

func TestResolveUVCacheDir_DefaultFromBracketedXDGOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG_CACHE_HOME builds the default path on Linux only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("UV_CACHE_DIR", "")
	xdg := filepath.Join(filepath.Clean(t.TempDir()), "cache[1]")
	t.Setenv("XDG_CACHE_HOME", xdg)

	dir, err := ResolveUVCacheDir(DefaultProviders()["uv"].Paths)

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(xdg, "uv"), dir)
}

func TestResolveUVCacheDir_PatternSpelledPathIsRejectedOnlyWhenAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("UV_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")
	absent := filepath.Join(t.TempDir(), "uv*")

	_, err := ResolveUVCacheDir([]string{absent})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "literal directory, not a glob pattern")

	_, err = ResolveUVCacheDir([]string{filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one uv cache directory")
}

func TestDefaultProviders_UVIsOptInOnEveryOS(t *testing.T) {
	for _, p := range []Platform{macPlatform, linPlatform, winPlatform} {
		uv, ok := DefaultProvidersFor(p)["uv"]
		require.True(t, ok, p.OS)
		assert.False(t, uv.Enabled, p.OS)
		assert.Equal(t, "uv cache clean", uv.CleanCmd, p.OS)
	}
}

func loadUVConfig(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	loader := NewLoader()
	loader.SetConfigPath(path)
	cfg, err := loader.Load()
	require.NoError(t, err)
	return cfg
}

func TestLoader_UVEnabledFlagMerge(t *testing.T) {
	omitted := loadUVConfig(t, "version: \"1\"\nproviders:\n  uv:\n    max_size: 2G\n")
	assert.False(t, omitted.Providers["uv"].Enabled, "an omitted enabled follows the new opt-in default")

	explicitOn := loadUVConfig(t, "version: \"1\"\nproviders:\n  uv:\n    enabled: true\n")
	assert.True(t, explicitOn.Providers["uv"].Enabled, "a saved enabled: true is kept")

	explicitOff := loadUVConfig(t, "version: \"1\"\nproviders:\n  uv:\n    enabled: false\n")
	assert.False(t, explicitOff.Providers["uv"].Enabled)
}

func TestResolveUVCacheDir_RejectsPathsThatStayRelativeAfterExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(home)

	spellings := []string{`\x`, `\cache\uv`}
	if runtime.GOOS != "windows" {
		spellings = append(spellings, `\\x`, `~\x`, `~\cache\uv`)
	}
	for _, spelled := range spellings {
		t.Run(spelled, func(t *testing.T) {
			dir, err := ResolveUVCacheDir([]string{spelled})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be absolute or start with ~/")
			assert.Empty(t, dir)
		})
	}
}

func TestResolveUVCacheDir_AcceptsTildeAndAbsolute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("UV_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")
	abs := filepath.Join(filepath.Clean(t.TempDir()), "uvc")

	got, err := ResolveUVCacheDir([]string{abs})
	require.NoError(t, err)
	assert.Equal(t, abs, got)

	got, err = ResolveUVCacheDir([]string{"~/custom/uv"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "custom", "uv"), got)
}

func TestProvider_PathError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	abs := filepath.Clean(t.TempDir())

	for _, ok := range [][]string{nil, {abs}, {"~"}, {"~/x"}, {abs, "~/y/*"}} {
		assert.NoError(t, Provider{Paths: ok}.PathError(), "%v", ok)
	}
	bad := [][]string{{"Documents"}, {"."}, {"a/../b"}, {abs, "rel"}, {"~user/x"}, {""}, {`\x\cache`}}
	if runtime.GOOS != "windows" {
		bad = append(bad, []string{`~\x`}, []string{`C:/x`}, []string{`C:\x`})
	}
	for _, paths := range bad {
		err := Provider{Paths: paths}.PathError()
		require.Error(t, err, "%v", paths)
		assert.Contains(t, err.Error(), "must be absolute or start with ~/")
	}
}

func TestEnabledProviders_RelativePathsStayVisibleWhateverTheCwd(t *testing.T) {
	cwd := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, "Documents"), 0o700))
	t.Chdir(cwd)
	cfg := &Config{Providers: map[string]Provider{
		"go-build": {Enabled: true, Paths: []string{"Documents"}, MaxSize: "1G"},
		"absent":   {Enabled: true, Paths: []string{filepath.Join(t.TempDir(), "none")}, MaxSize: "1G"},
	}}

	assert.Equal(t, []string{"go-build"}, cfg.EnabledProviders(), "present or not, the relative path is reported")

	require.NoError(t, os.RemoveAll(filepath.Join(cwd, "Documents")))
	assert.Equal(t, []string{"go-build"}, cfg.EnabledProviders())
}

func TestStateDir_IgnoresXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir, err := StateDir()

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".local", "state", "bilgie"), dir)
}
