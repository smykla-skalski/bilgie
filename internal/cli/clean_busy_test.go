package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubUVOnPath puts a placeholder uv first on PATH so the provider counts as
// available; a busy uv is skipped before the binary would ever run.
func stubUVOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "uv"
	if runtime.GOOS == "windows" {
		name = "uv.exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// busyUVLoader returns a config whose uv cache holds a live flock, plus an
// idle command provider, so uv is deterministically busy.
func busyUVLoader(t *testing.T, extra string) (loader *config.Loader, lockPath string) {
	t.Helper()
	stubUVOnPath(t)

	uvDir := t.TempDir()
	lockPath = filepath.Join(uvDir, ".lock")
	release, acquired, err := osshim.TryLock(lockPath)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(release)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := `version: "1"
providers:
  uv:
    enabled: true
    paths:
      - ` + uvDir + `
    max_size: 1
  idle:
    enabled: true
    paths:
      - ` + t.TempDir() + `
    max_size: 1GB
    clean_cmd: "echo cleaned"
` + extra
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))

	loader = config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()
	return loader, lockPath
}

func TestClean_BusyProviderSkippedInText(t *testing.T) {
	loader, lockPath := busyUVLoader(t, "")

	var err error
	output := captureStdout(t, func() {
		err = runCleanWithLoader(loader, nil, true, false, true, false, false, os.Stdin)
	})

	require.NoError(t, err, "a skip is not a failure")
	assert.Contains(t, output, "Cleaning uv... skipped (lock held: "+lockPath+")")
	assert.Contains(t, output, "Cleaning idle... done")
}

func TestClean_BusyProviderSkippedInJSON(t *testing.T) {
	loader, lockPath := busyUVLoader(t, "")

	var err error
	output := captureStdout(t, func() {
		err = runCleanWithOptions(loader, nil, cleanOptions{all: true, force: true, json: true}, os.Stdin)
	})
	require.NoError(t, err)

	var got CleanOutput
	require.NoError(t, json.Unmarshal([]byte(output), &got), output)

	byName := map[string]ProviderCleanResult{}
	for _, r := range got.Providers {
		byName[r.Name] = r
	}
	assert.Equal(t, statusSkipped, byName["uv"].Status)
	assert.Equal(t, "lock held: "+lockPath, byName["uv"].Reason)
	assert.Equal(t, statusCleaned, byName["idle"].Status)
}

func TestClean_HungCommandReportedInJSON(t *testing.T) {
	loader, _ := busyUVLoader(t, `  hung:
    enabled: true
    paths:
      - `+t.TempDir()+`
    max_size: 1GB
    clean_cmd: 'sh -c "sleep 30"'
    clean_timeout: 1s
`)

	var err error
	output := captureStdout(t, func() {
		err = runCleanWithOptions(loader, []string{"hung"}, cleanOptions{force: true, json: true}, os.Stdin)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out after 1s")

	var got CleanOutput
	require.NoError(t, json.Unmarshal([]byte(output), &got), output)
	require.Len(t, got.Providers, 1)
	assert.Equal(t, statusError, got.Providers[0].Status)
	assert.Contains(t, got.Providers[0].Error, "timed out after 1s")
}

func TestClean_JSONRequiresForceOrDryRun(t *testing.T) {
	loader, _ := busyUVLoader(t, "")

	err := runCleanWithOptions(loader, nil, cleanOptions{all: true, json: true}, os.Stdin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--json requires --force or --dry-run")
}

func TestClean_JSONDryRunReportsSkip(t *testing.T) {
	loader, _ := busyUVLoader(t, "")

	var err error
	output := captureStdout(t, func() {
		err = runCleanWithOptions(loader, nil, cleanOptions{all: true, dryRun: true, json: true}, os.Stdin)
	})
	require.NoError(t, err)

	var got CleanOutput
	require.NoError(t, json.Unmarshal([]byte(output), &got), output)
	assert.True(t, got.DryRun)
	statuses := map[string]string{}
	for _, r := range got.Providers {
		statuses[r.Name] = r.Status
	}
	assert.Equal(t, statusSkipped, statuses["uv"])
	assert.Equal(t, statusDryRun, statuses["idle"])
}

func TestCleanCmd_HasJSONFlag(t *testing.T) {
	assert.NotNil(t, CleanCmd.Flags().Lookup("json"))
}

func TestClean_JSONWithNoAvailableProviders(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := `version: "1"
providers:
  ghost:
    enabled: true
    paths:
      - ` + t.TempDir() + `
    max_size: 1GB
    clean_cmd: "definitely-not-a-real-binary-xyz clean"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))
	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()

	var err error
	output := captureStdout(t, func() {
		err = runCleanWithOptions(loader, nil, cleanOptions{all: true, force: true, json: true}, os.Stdin)
	})

	require.Error(t, err)
	var got CleanOutput
	require.NoError(t, json.Unmarshal([]byte(output), &got), output)
	require.Len(t, got.Providers, 1)
	assert.Equal(t, "ghost", got.Providers[0].Name)
	assert.Equal(t, statusUnavailable, got.Providers[0].Status)
	assert.Contains(t, err.Error(), "ghost")
}

func TestClean_LoadErrorNamesProviderAndCause(t *testing.T) {
	loader, _ := busyUVLoader(t, `  badtimeout:
    enabled: true
    paths:
      - `+t.TempDir()+`
    max_size: 1GB
    clean_cmd: "echo x"
    clean_timeout: abc
`)

	err := runCleanWithOptions(loader, []string{"badtimeout"}, cleanOptions{force: true, dryRun: true}, os.Stdin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "badtimeout")
	assert.Contains(t, err.Error(), "clean_timeout")
}

func TestClean_InterruptedCleanIsNotAnErrorInJSON(t *testing.T) {
	loader, _ := busyUVLoader(t, `  slow:
    enabled: true
    paths:
      - `+t.TempDir()+`
    max_size: 1GB
    clean_cmd: 'sh -c "sleep 30"'
    clean_timeout: 100s
`)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)

	var err error
	output := captureStdout(t, func() {
		err = runCleanWithContext(func() (context.Context, context.CancelFunc) { return ctx, cancel }, loader, []string{"slow"}, cleanOptions{force: true, json: true}, os.Stdin)
	})

	require.NoError(t, err)
	var got CleanOutput
	require.NoError(t, json.Unmarshal([]byte(output), &got), output)
	assert.True(t, got.Cancelled)
	require.Len(t, got.Providers, 1)
	assert.Equal(t, "slow", got.Providers[0].Name)
	assert.Equal(t, statusCancelled, got.Providers[0].Status)
}

func TestClean_InterruptedCleanPrintsProviderOutputInText(t *testing.T) {
	loader, _ := busyUVLoader(t, `  slow:
    enabled: true
    paths:
      - `+t.TempDir()+`
    max_size: 1GB
    clean_cmd: 'sh -c "echo partial-progress-line; sleep 30"'
    clean_timeout: 100s
`)

	tests := []struct {
		name      string
		opts      cleanOptions
		wantLines bool
	}{
		{name: "concise", opts: cleanOptions{force: true}, wantLines: true},
		{name: "verbose", opts: cleanOptions{force: true, verbose: true}, wantLines: true},
		{name: "quiet", opts: cleanOptions{force: true, quiet: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(500*time.Millisecond, cancel)

			var err error
			out := captureStdout(t, func() {
				err = runCleanWithContext(func() (context.Context, context.CancelFunc) { return ctx, cancel }, loader, []string{"slow"}, tt.opts, os.Stdin)
			})

			require.NoError(t, err)
			if !tt.wantLines {
				assert.NotContains(t, out, "partial-progress-line")
				return
			}
			assert.Contains(t, out, "partial-progress-line")
			assert.Less(t, strings.Index(out, "partial-progress-line"), strings.Index(out, "Cancelled"))
		})
	}
}

func TestClean_NamedProviderWithoutCacheDirNamesThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-uv-cache")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`version: "1"
providers:
  uv:
    enabled: true
    paths:
      - `+missing+`
    max_size: 1
`), 0o600))
	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()

	err := runCleanWithLoader(loader, []string{"uv"}, false, false, true, false, false, os.Stdin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider uv: no cache directory found (checked: "+missing+")")
	assert.NotContains(t, err.Error(), "unknown providers")
}

func TestClean_MissingCacheAndTypoAreBothReported(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-uv-cache")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`version: "1"
providers:
  uv:
    enabled: true
    paths:
      - `+missing+`
    max_size: 1
`), 0o600))

	for _, args := range [][]string{{"uv", "typo"}, {"typo", "uv"}} {
		loader := config.NewLoader()
		loader.SetConfigPath(cfgPath)
		loader.SkipDefaults()

		err := runCleanWithLoader(loader, args, false, false, true, false, false, os.Stdin)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "provider uv: no cache directory found (checked: "+missing+")")
		assert.Contains(t, err.Error(), "unknown providers: typo")
		assert.NotContains(t, err.Error(), "unknown providers: uv")
	}
}
