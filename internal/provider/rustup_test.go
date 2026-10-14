package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rustupListing = `stable-aarch64-apple-darwin
nightly-aarch64-apple-darwin (default)
1.75.0-aarch64-apple-darwin
nightly-2024-01-01-aarch64-apple-darwin
beta-aarch64-apple-darwin (active)
`

// rustupRealListing is the format plain `rustup toolchain list` prints:
// names and markers only, never paths.
const rustupRealListing = `stable-aarch64-apple-darwin (active, default)
1.99.0-aarch64-apple-darwin
1.75.0-aarch64-apple-darwin
mylinked
`

func TestRemovableToolchains(t *testing.T) {
	tests := []struct {
		name      string
		listing   string
		overrides string
		linked    map[string]bool
		want      []string
		wantErr   bool
	}{
		{
			name:    "keeps stable, default and active",
			listing: rustupListing,
			want:    []string{"1.75.0-aarch64-apple-darwin", "nightly-2024-01-01-aarch64-apple-darwin"},
		},
		{
			name:    "stable as default",
			listing: "stable-aarch64-apple-darwin (active, default)\n1.80.0-aarch64-apple-darwin\n",
			want:    []string{"1.80.0-aarch64-apple-darwin"},
		},
		{
			name:    "bare stable name kept",
			listing: "stable (default)\n1.80.0\n",
			want:    []string{"1.80.0"},
		},
		{
			name:    "stable prefix lookalike is not stable",
			listing: "stable-aarch64-apple-darwin (default)\nstableish-x86_64\n",
			want:    []string{"stableish-x86_64"},
		},
		{
			name:    "no default fails closed",
			listing: "stable-aarch64-apple-darwin\n1.80.0\n",
			wantErr: true,
		},
		{
			name:    "linked toolchain is kept",
			listing: "stable-aarch64-apple-darwin\nnightly (default)\nmytc\n1.75.0\n",
			linked:  map[string]bool{"mytc": true},
			want:    []string{"1.75.0"},
		},
		{
			name:      "no overrides is not a pin",
			listing:   "nightly (default)\noverrides\n1.75.0\n",
			overrides: "no overrides\n",
			want:      []string{"overrides", "1.75.0"},
		},
		{
			name:    "stderr noise is not a toolchain",
			listing: "info: default toolchain not set\nwarning: default (x)\nstable-aarch64-apple-darwin\n1.75.0\n",
			wantErr: true,
		},
		{
			name:    "override toolchain kept",
			listing: "nightly (default)\n1.70.0 (override)\n1.75.0\n",
			want:    []string{"1.75.0"},
		},
		{
			name:    "stable without marker kept next to other default",
			listing: "stable-aarch64-apple-darwin\nnightly-aarch64-apple-darwin (default)\n1.75.0\n",
			want:    []string{"1.75.0"},
		},
		{
			name:      "directory override elsewhere keeps pinned channel",
			listing:   "nightly-aarch64-apple-darwin (default)\n1.75.0-aarch64-apple-darwin\n1.70.0-aarch64-apple-darwin\n",
			overrides: "/Users/me/proj\t1.75.0\n",
			want:      []string{"1.70.0-aarch64-apple-darwin"},
		},
		{
			name:    "none installed",
			listing: "no installed toolchains\n",
		},
		{
			name:    "empty",
			listing: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := removableToolchains(tt.listing, tt.overrides, func(n string) bool { return tt.linked[n] })
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

type fakeRustup struct {
	list  string
	fail  string
	calls [][]string
}

func (f *fakeRustup) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if len(args) < 2 {
		return "", errors.New("unexpected rustup args")
	}
	if args[0] == "override" {
		return "no overrides", nil
	}
	if args[1] == "list" {
		return f.list, nil
	}
	if f.fail != "" && len(args) > 2 && args[2] == f.fail {
		return "boom", errors.New("exit 1")
	}
	return "", nil
}

func newFakeRustupProvider(t *testing.T, f *fakeRustup) *RustupProvider {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blob"), make([]byte, 2048), 0o600))
	for line := range strings.SplitSeq(f.list, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && toolchainNamePattern.MatchString(fields[0]) {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, fields[0]), 0o750))
		}
	}
	p, err := NewRustupProvider("rustup", config.Provider{
		Paths:   []string{dir},
		MaxSize: "1K",
		Enabled: true,
	})
	require.NoError(t, err)
	p.run = f.run
	p.busy = nil // never inspect the real process table
	return p
}

func TestRustupProvider_Clean(t *testing.T) {
	f := &fakeRustup{list: rustupListing}
	p := newFakeRustupProvider(t, f)

	res, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)

	var uninstalled []string
	for _, c := range f.calls[2:] {
		assert.Equal(t, []string{"toolchain", "uninstall"}, c[:2])
		uninstalled = append(uninstalled, c[2])
	}
	assert.Equal(t, []string{"1.75.0-aarch64-apple-darwin", "nightly-2024-01-01-aarch64-apple-darwin"}, uninstalled)
	assert.Contains(t, res.Output, "uninstalled")
}

func TestRustupProvider_DryRunUninstallsNothing(t *testing.T) {
	f := &fakeRustup{list: rustupListing}
	p := newFakeRustupProvider(t, f)

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true, Mode: CleanModeSmart})
	require.NoError(t, err)

	assert.Len(t, f.calls, 2)
	assert.Contains(t, res.Output, "would uninstall")
	assert.Contains(t, res.Output, "1.75.0")
	assert.NotContains(t, res.Output, "stable")
}

func TestRustupProvider_NoDefaultRemovesNothing(t *testing.T) {
	f := &fakeRustup{list: "stable-aarch64-apple-darwin\n1.80.0\n"}
	p := newFakeRustupProvider(t, f)

	_, err := p.Clean(context.Background(), CleanOptions{})
	require.Error(t, err)
	assert.Len(t, f.calls, 2)
}

func TestRustupProvider_UninstallFailure(t *testing.T) {
	f := &fakeRustup{list: rustupListing, fail: "1.75.0-aarch64-apple-darwin"}
	p := newFakeRustupProvider(t, f)

	_, err := p.Clean(context.Background(), CleanOptions{})
	require.Error(t, err)
}

func TestRustupProvider_Available(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	p := newFakeRustupProvider(t, &fakeRustup{})
	assert.False(t, p.Available())

	require.NoError(t, os.WriteFile(filepath.Join(binDir, executableName("rustup")), []byte("#!/bin/sh\n"), 0o755))
	assert.True(t, p.Available())
}

func TestRustupProvider_UnderLimitRemovesNothing(t *testing.T) {
	f := &fakeRustup{list: rustupListing}
	p, err := NewRustupProvider("rustup", config.Provider{Paths: []string{t.TempDir()}, MaxSize: "1G", Enabled: true})
	require.NoError(t, err)
	p.run = f.run
	p.busy = nil

	res, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.Empty(t, f.calls)
	assert.Equal(t, "already under limit", res.Output)
}

func TestRustupProvider_SkipsWhenBusy(t *testing.T) {
	f := &fakeRustup{list: rustupListing}
	p := newFakeRustupProvider(t, f)
	p.busy = &busyGuard{
		processes:     busyProcesses["rustup"],
		listProcesses: listOf("/x/.cargo/bin/cargo +1.75.0 build"),
	}

	res, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.Empty(t, f.calls)
	assert.NotEmpty(t, res.SkipReason)
}

func TestRustupProvider_TimeoutBoundsRustup(t *testing.T) {
	p := newFakeRustupProvider(t, &fakeRustup{})
	p.timeout = 20 * time.Millisecond
	p.run = func(ctx context.Context, _ ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	_, err := p.Clean(context.Background(), CleanOptions{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestNewRustupProvider_CleanTimeout(t *testing.T) {
	cfg := config.Provider{Paths: []string{t.TempDir()}, MaxSize: "1G", CleanTimeout: "5s"}
	p, err := NewRustupProvider("rustup", cfg)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, p.timeout)

	cfg.CleanTimeout = "-1s"
	_, err = NewRustupProvider("rustup", cfg)
	require.Error(t, err)
}

func TestRustupProvider_PartialFailureReportsRemoved(t *testing.T) {
	f := &fakeRustup{list: rustupListing, fail: "nightly-2024-01-01-aarch64-apple-darwin"}
	p := newFakeRustupProvider(t, f)

	res, err := p.Clean(context.Background(), CleanOptions{})
	require.Error(t, err)
	assert.Contains(t, res.Output, "1.75.0-aarch64-apple-darwin")
}

func TestRustupProvider_UnmeasurableSizeRemovesNothing(t *testing.T) {
	f := &fakeRustup{list: rustupListing}
	p := newFakeRustupProvider(t, f)
	require.NoError(t, os.Chmod(p.paths[0], 0o000))
	t.Cleanup(func() { _ = os.Chmod(p.paths[0], 0o700) })

	_, err := p.Clean(context.Background(), CleanOptions{})
	if err == nil {
		t.Skip("size scan tolerated unreadable dir on this platform")
	}
	assert.Empty(t, f.calls)
}

func TestRustupProvider_ForeignRustupHomeRemovesNothing(t *testing.T) {
	f := &fakeRustup{list: rustupListing}
	p := newFakeRustupProvider(t, f)

	t.Setenv("RUSTUP_HOME", t.TempDir())
	_, err := p.Clean(context.Background(), CleanOptions{})
	require.Error(t, err)
	assert.Empty(t, f.calls)

	t.Setenv("RUSTUP_HOME", filepath.Dir(p.paths[0]))
	_, err = p.Clean(context.Background(), CleanOptions{DryRun: true})
	require.NoError(t, err)
}

func TestRustupProvider_SymlinkedToolchainNeverUninstalled(t *testing.T) {
	home := t.TempDir()
	toolchains := filepath.Join(home, "toolchains")
	require.NoError(t, os.MkdirAll(toolchains, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(toolchains, "blob"), make([]byte, 2048), 0o600))
	for _, name := range []string{"stable-aarch64-apple-darwin", "1.99.0-aarch64-apple-darwin", "1.75.0-aarch64-apple-darwin"} {
		require.NoError(t, os.MkdirAll(filepath.Join(toolchains, name), 0o750))
	}
	own := t.TempDir()
	require.NoError(t, os.Symlink(own, filepath.Join(toolchains, "mylinked")))
	t.Setenv("RUSTUP_HOME", home)

	f := &fakeRustup{list: rustupRealListing}
	p, err := NewRustupProvider("rustup", config.Provider{Paths: []string{toolchains}, MaxSize: "1K", Enabled: true})
	require.NoError(t, err)
	p.run = f.run
	p.busy = nil

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.NotContains(t, res.Output, "mylinked")
	assert.Contains(t, res.Output, "1.99.0-aarch64-apple-darwin")
	assert.Contains(t, res.Output, "1.75.0-aarch64-apple-darwin")

	_, err = p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	for _, call := range f.calls {
		assert.NotContains(t, call, "mylinked")
	}
}

func TestRustupProvider_MissingToolchainDirIsKept(t *testing.T) {
	p := newFakeRustupProvider(t, &fakeRustup{list: "nightly (default)\n"})
	assert.True(t, p.isLinked("ghost"), "unverifiable toolchains fail closed")
}

func TestRustupProvider_DryRunReportsProjectedBytes(t *testing.T) {
	f := &fakeRustup{list: rustupRealListing}
	p := newFakeRustupProvider(t, f)
	require.NoError(t, os.WriteFile(filepath.Join(p.paths[0], "1.99.0-aarch64-apple-darwin", "lib"), make([]byte, 4096), 0o600))

	res, err := p.Clean(context.Background(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, int64(4096), res.BytesCleaned)
}
