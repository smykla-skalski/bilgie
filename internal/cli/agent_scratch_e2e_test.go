package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const scratchMarker = ".sail-scratch-root"

// scratchRoot is a temp root holding one scratch directory per owner state.
// Markers carry real process facts: this test process is the live owner and a
// child that already exited is the dead one. Nothing outside the root is read.
type scratchRoot struct {
	root       string
	live       string
	reusedPID  string
	exited     string
	malformed  string
	markerless string
}

func newScratchRoot(t *testing.T) scratchRoot {
	t.Helper()
	root := t.TempDir()

	pid := os.Getpid()
	start, running, err := osshim.ProcessStart(t.Context(), pid)
	require.NoError(t, err)
	require.True(t, running)
	if start.IsZero() {
		t.Skip("process start time unavailable")
	}

	child := exec.Command(os.Args[0], "-test.run=^$")
	require.NoError(t, child.Run())
	longAgo := time.Now().Add(-time.Hour).Unix()

	mk := func(name, marker string) string {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		files := []string{filepath.Join(dir, "data")}
		require.NoError(t, os.WriteFile(files[0], []byte(strings.Repeat("x", 512)), 0o600))
		if marker != "" {
			files = append(files, filepath.Join(dir, scratchMarker))
			require.NoError(t, os.WriteFile(files[1], []byte(marker), 0o600))
		}
		old := time.Now().Add(-10 * time.Hour)
		for _, f := range append(files, dir) {
			require.NoError(t, os.Chtimes(f, old, old))
		}
		return dir
	}
	return scratchRoot{
		root:       root,
		live:       mk("live", fmt.Sprintf("%d %d", pid, start.Unix())),
		reusedPID:  mk("reused-pid", fmt.Sprintf("%d %d", pid, longAgo)),
		exited:     mk("exited", fmt.Sprintf("%d %d", child.Process.Pid, longAgo)),
		malformed:  mk("malformed", "not a marker"),
		markerless: mk("markerless", ""),
	}
}

func (s scratchRoot) providerYAML(enabled bool) string {
	return `  agent-scratch-dirs:
    enabled: ` + strconv.FormatBool(enabled) + `
    type: dir-pattern
    paths:
      - ` + s.root + `/*
    max_size: 1G
    min_idle: 2h
    owner_marker: true
    skip_if_open: false
`
}

func (s scratchRoot) removable() []string { return []string{s.reusedPID, s.exited} }

func (s scratchRoot) protected() []string { return []string{s.live, s.malformed, s.markerless} }

func (s scratchRoot) assertOutput(t *testing.T, out, wouldVerb string) {
	t.Helper()
	for _, dir := range s.removable() {
		assert.Contains(t, out, wouldVerb+": "+dir, filepath.Base(dir))
	}
	assert.Contains(t, out, "skip: "+s.live+" (owner pid "+strconv.Itoa(os.Getpid())+" is running)")
	assert.Contains(t, out, "skip: "+s.malformed+" (owner marker "+scratchMarker+" malformed")
	assert.NotContains(t, out, s.markerless, "a directory without a marker is never a candidate")
	assert.NotContains(t, out, "skip: "+s.exited)
}

func (s scratchRoot) assertAutoOutput(t *testing.T, out string, dryRun bool) {
	t.Helper()
	assert.Contains(t, out, "tier emergency")
	assert.Contains(t, out, "agent-scratch-dirs: ")
	for _, dir := range s.protected() {
		assert.NotContains(t, out, dir, filepath.Base(dir))
	}
	if !dryRun {
		assert.Contains(t, out, "agent-scratch-dirs: freed ")
		return
	}
	for _, dir := range s.removable() {
		assert.Contains(t, out, "would remove: "+dir, filepath.Base(dir))
	}
	assert.Contains(t, out, "2 entries skipped")
}

func (s scratchRoot) assertDisk(t *testing.T, swept bool) {
	t.Helper()
	for _, dir := range s.removable() {
		assert.Equal(t, !swept, pathExists(dir), "%s swept=%v", filepath.Base(dir), swept)
	}
	for _, dir := range s.protected() {
		assert.DirExists(t, dir)
		assert.FileExists(t, filepath.Join(dir, "data"))
	}
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func scratchLoader(t *testing.T, providers string) *config.Loader {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("version: \"1\"\nproviders:\n"+providers), 0o600))
	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()
	return loader
}

func TestCleanAgentScratchDirs_SweepsOnlyDirsWhoseOwnerIsGone(t *testing.T) {
	tests := []struct {
		name  string
		opts  cleanOptions
		swept bool
		verb  string
	}{
		{"dry run", cleanOptions{all: true, dryRun: true, verbose: true}, false, "would remove"},
		{"real run", cleanOptions{all: true, force: true, verbose: true}, true, "removed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScratchRoot(t)
			loader := scratchLoader(t, s.providerYAML(true))

			var err error
			out := captureStdout(t, func() {
				err = runCleanWithOptions(loader, nil, tt.opts, os.Stdin)
			})
			require.NoError(t, err)

			s.assertOutput(t, out, tt.verb)
			s.assertDisk(t, tt.swept)
		})
	}
}

func TestCleanAgentScratchDirs_SecondRunFindsNothingMoreToSweep(t *testing.T) {
	s := newScratchRoot(t)
	loader := scratchLoader(t, s.providerYAML(true))
	opts := cleanOptions{all: true, force: true, verbose: true}
	captureStdout(t, func() { require.NoError(t, runCleanWithOptions(loader, nil, opts, os.Stdin)) })

	var err error
	out := captureStdout(t, func() { err = runCleanWithOptions(loader, nil, opts, os.Stdin) })

	require.NoError(t, err)
	assert.NotContains(t, out, "removed: ")
	s.assertDisk(t, true)
}

func TestAutoAgentScratchDirs_EmergencySweepHonorsOwnerMarkers(t *testing.T) {
	tests := []struct {
		name   string
		dryRun bool
	}{
		{"dry run", true},
		{"real run", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScratchRoot(t)
			f := newAutoFixture(t, 1*autoGiB, s.providerYAML(false))
			f.env.verbose = true

			require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, tt.dryRun))

			s.assertAutoOutput(t, f.out.String(), tt.dryRun)
			s.assertDisk(t, !tt.dryRun)
		})
	}
}

func TestAutoAgentScratchDirs_LeftAloneBelowEmergencyTier(t *testing.T) {
	s := newScratchRoot(t)
	f := newAutoFixture(t, 7*autoGiB, s.providerYAML(false)+"auto:\n  min_free: 10G\n  min_free_pct: 0\n")

	require.NoError(t, runAutoWithLoader(t.Context(), f.loader, f.env, false))

	assert.Contains(t, f.out.String(), "tier critical")
	assert.NotContains(t, f.out.String(), "agent-scratch-dirs")
	s.assertDisk(t, false)
}
