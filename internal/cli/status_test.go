package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	// Read in goroutine to avoid pipe buffer deadlock
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()

	require.NoError(t, w.Close())
	os.Stdout = old
	<-done
	return buf.String()
}

func TestScanProvider(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("hello"), 0o600))

	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"gh": {
				Paths:   []string{tmpDir},
				MaxSize: "1GB",
				Enabled: true,
			},
		},
	}

	status := scanProvider(t.Context(), cfg, "gh")

	assert.Equal(t, "gh", status.Name)
	assert.Equal(t, int64(5), status.Current)
	assert.Equal(t, "5 B", status.CurrentFmt)
	assert.Equal(t, int64(1073741824), status.Max)
	assert.Equal(t, "1.0 GiB", status.MaxFmt)
	assert.False(t, status.OverLimit)
	assert.Empty(t, status.Error)
}

func TestScanProvider_OverLimit(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("hello world"), 0o600))

	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"gh": {
				Paths:   []string{tmpDir},
				MaxSize: "5B",
				Enabled: true,
			},
		},
	}

	status := scanProvider(t.Context(), cfg, "gh")

	assert.True(t, status.OverLimit)
	assert.Greater(t, status.Current, status.Max)
}

func TestScanProvider_InvalidMaxSize(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"cargo": {
				Paths:   []string{t.TempDir()},
				MaxSize: "invalid",
				Enabled: true,
			},
		},
	}

	status := scanProvider(t.Context(), cfg, "cargo")

	assert.Contains(t, status.Error, "provider cargo: ")
	assert.Contains(t, status.Error, "parse max_size")
}

func TestScanProvider_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"cargo": {
				Paths:   []string{tmpDir},
				MaxSize: "1GB",
				Enabled: true,
			},
		},
	}

	status := scanProvider(t.Context(), cfg, "cargo")

	assert.Equal(t, int64(0), status.Current)
	assert.False(t, status.OverLimit)
	assert.Empty(t, status.Error)
}

func TestScanProviders_Parallel(t *testing.T) {
	tmpDir1 := t.TempDir()
	tmpDir2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir1, "a.txt"), []byte("aaa"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir2, "b.txt"), []byte("bbbbb"), 0o600))

	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"gh":   {Paths: []string{tmpDir1}, MaxSize: "1GB", Enabled: true},
			"edge": {Paths: []string{tmpDir2}, MaxSize: "1GB", Enabled: true},
		},
	}

	statuses := scanProviders(t.Context(), cfg, []string{"gh", "edge"})

	assert.Len(t, statuses, 2)
	assert.Equal(t, "gh", statuses[0].Name)
	assert.Equal(t, int64(3), statuses[0].Current)
	assert.Equal(t, "edge", statuses[1].Name)
	assert.Equal(t, int64(5), statuses[1].Current)
}

func TestOutputJSON(t *testing.T) {
	statuses := []ProviderStatus{
		{Name: "test1", Current: 1024, CurrentFmt: "1.0 KiB", Max: 2048, MaxFmt: "2.0 KiB", OverLimit: false},
		{Name: "test2", Current: 3072, CurrentFmt: "3.0 KiB", Max: 1024, MaxFmt: "1.0 KiB", OverLimit: true},
	}

	var err error
	output := captureStdout(t, func() {
		err = outputJSON(statuses, nil, nil)
	})
	require.NoError(t, err)

	var out StatusOutput
	require.NoError(t, json.Unmarshal([]byte(output), &out))

	assert.Len(t, out.Providers, 2)
	assert.Equal(t, int64(4096), out.TotalBytes)
	assert.Equal(t, "4.0 KiB", out.Total)
	assert.Equal(t, "test1", out.Providers[0].Name)
	assert.Equal(t, "test2", out.Providers[1].Name)
	assert.True(t, out.Providers[1].OverLimit)
}

func TestOutputJSON_WithError(t *testing.T) {
	statuses := []ProviderStatus{
		{Name: "broken", Error: "something went wrong"},
	}

	var err error
	output := captureStdout(t, func() {
		err = outputJSON(statuses, nil, nil)
	})
	require.NoError(t, err)

	var out StatusOutput
	require.NoError(t, json.Unmarshal([]byte(output), &out))

	assert.Equal(t, "something went wrong", out.Providers[0].Error)
}

func TestOutputTable(t *testing.T) {
	statuses := []ProviderStatus{
		{Name: "ok-provider", Current: 100, CurrentFmt: "100 B", Max: 1000, MaxFmt: "1000 B", OverLimit: false},
		{Name: "over-provider", Current: 2000, CurrentFmt: "2.0 KiB", Max: 1000, MaxFmt: "1000 B", OverLimit: true},
		{Name: "error-provider", Error: "failed to scan"},
	}

	var err error
	output := captureStdout(t, func() {
		err = outputTable(statuses)
	})
	require.NoError(t, err)

	assert.Contains(t, output, "ok-provider")
	assert.Contains(t, output, "over-provider")
	assert.Contains(t, output, "error-provider")
	assert.Contains(t, output, "ok")
	assert.Contains(t, output, "OVER")
	assert.Contains(t, output, "error")
	assert.Contains(t, output, "Total:")
	// Error providers show "-" for empty values
	assert.Contains(t, output, "-")
}

func TestOutputTable_DiskImageAnnotation(t *testing.T) {
	statuses := []ProviderStatus{
		{
			Name: "docker", Current: 1024, CurrentFmt: "1.0 KiB",
			Max: 10240, MaxFmt: "10 KiB", OverLimit: false,
			DiskImageFmt: "5.0 KiB", DiskImageBytes: 5120,
		},
	}

	var err error
	output := captureStdout(t, func() {
		err = outputTable(statuses)
	})
	require.NoError(t, err)

	assert.Contains(t, output, "1.0 KiB (5.0 KiB on disk)")
}

func TestOutputJSON_DiskImageFields(t *testing.T) {
	statuses := []ProviderStatus{
		{
			Name: "docker", Current: 1024, CurrentFmt: "1.0 KiB",
			Max: 10240, MaxFmt: "10 KiB", OverLimit: false,
			DiskImageFmt: "5.0 KiB", DiskImageBytes: 5120,
		},
	}

	var err error
	output := captureStdout(t, func() {
		err = outputJSON(statuses, nil, nil)
	})
	require.NoError(t, err)

	var out StatusOutput
	require.NoError(t, json.Unmarshal([]byte(output), &out))

	assert.Equal(t, int64(5120), out.Providers[0].DiskImageBytes)
	assert.Equal(t, "5.0 KiB", out.Providers[0].DiskImageFmt)
}

func TestOutputTable_Empty(t *testing.T) {
	var err error
	output := captureStdout(t, func() {
		err = outputTable([]ProviderStatus{})
	})
	require.NoError(t, err)

	assert.Contains(t, output, "Total: 0 B")
}

func TestRunStatus_NoConfig_UsesDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "go", "pkg", "mod"), 0o750))
	cfgPath := filepath.Join(tmpDir, "nonexistent.yaml")
	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)

	var err error
	output := captureStdout(t, func() {
		err = runStatusWithLoader(loader, false, nil, nil)
	})

	require.NoError(t, err)
	// Config file should NOT be auto-created
	_, statErr := os.Stat(cfgPath)
	assert.True(t, os.IsNotExist(statErr), "config file should not be auto-created")
	// But should show default providers (go-mod exists in the sandbox home)
	assert.Contains(t, output, "go-mod")
}

func TestRunStatus_NoEnabledProviders(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `version: "1"
providers:
  test:
    enabled: false
    paths:
      - /tmp
    max_size: 1GB
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o600))

	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()

	var err error
	output := captureStdout(t, func() {
		err = runStatusWithLoader(loader, false, nil, nil)
	})
	require.NoError(t, err)

	assert.Contains(t, output, "No enabled providers")
}

func TestRunStatus_TableOutput(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "data.txt"), []byte("test data"), 0o600))

	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `version: "1"
providers:
  test:
    enabled: true
    paths:
      - ` + cacheDir + `
    max_size: 1GB
    clean_cmd: echo
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o600))

	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()

	var err error
	output := captureStdout(t, func() {
		err = runStatusWithLoader(loader, false, nil, nil)
	})
	require.NoError(t, err)

	assert.Contains(t, output, "test")
	assert.Contains(t, output, "ok")
	assert.Contains(t, output, "Total:")
}

func TestRunStatus_JSONOutput(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "data.txt"), []byte("json test"), 0o600))

	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `version: "1"
providers:
  jsontest:
    enabled: true
    paths:
      - ` + cacheDir + `
    max_size: 1GB
    clean_cmd: echo
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o600))

	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()

	var err error
	output := captureStdout(t, func() {
		err = runStatusWithLoader(loader, true, nil, nil)
	})
	require.NoError(t, err)

	var out StatusOutput
	require.NoError(t, json.Unmarshal([]byte(output), &out))

	assert.Len(t, out.Providers, 1)
	assert.Equal(t, "jsontest", out.Providers[0].Name)
	assert.Equal(t, int64(9), out.Providers[0].Current)
}

func TestStatusCmd_HasJSONFlag(t *testing.T) {
	flag := StatusCmd.Flags().Lookup("json")
	require.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)
}

func TestScanProvider_InvalidGlobPattern(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"cargo": {
				Paths:   []string{"[invalid"},
				MaxSize: "1GB",
				Enabled: true,
			},
		},
	}

	status := scanProvider(t.Context(), cfg, "cargo")

	assert.Contains(t, status.Error, "provider cargo: ")
	assert.Contains(t, status.Error, "expand paths")
}

func TestStatus_ListsOnlyProvidersThatApplyOnInjectedOS(t *testing.T) {
	macOnlyDirs := []string{
		"Library/Developer/Xcode/DerivedData",
		"Library/Developer/Xcode/Archives",
		"Library/Developer/CoreSimulator/Caches",
		"Library/Caches/Homebrew",
	}
	tests := []struct {
		os      string
		goBuild string
		want    []string
		absent  []string
	}{
		{config.OSDarwin, "Library/Caches/go-build",
			[]string{"go-build", "homebrew", "ios-simulator", "xcode-archives", "xcode-deriveddata"}, nil},
		{config.OSLinux, ".cache/go-build", []string{"go-build"},
			[]string{"homebrew", "ios-simulator", "xcode-archives", "xcode-deriveddata"}},
		{config.OSWindows, "AppData/Local/go-build", []string{"go-build"},
			[]string{"homebrew", "ios-simulator", "xcode-archives", "xcode-deriveddata"}},
	}
	for _, tt := range tests {
		t.Run(tt.os, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			for _, rel := range append([]string{tt.goBuild}, macOnlyDirs...) {
				require.NoError(t, os.MkdirAll(filepath.Join(home, filepath.FromSlash(rel)), 0o750))
			}
			loader := config.NewLoader()
			loader.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
			loader.SetPlatform(config.Platform{OS: tt.os, Home: home})

			var err error
			out := captureStdout(t, func() { err = runStatusWithLoader(loader, true, nil, nil) })
			require.NoError(t, err)

			var got StatusOutput
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			var names []string
			for _, p := range got.Providers {
				names = append(names, p.Name)
			}
			if tt.absent == nil {
				assert.Equal(t, tt.want, names)
				return
			}
			assert.Subset(t, names, tt.want)
			for _, name := range tt.absent {
				assert.NotContains(t, names, name)
			}
		})
	}
}
