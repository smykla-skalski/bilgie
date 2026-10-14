package cli

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
)

func TestUnavailableProvider_String(t *testing.T) {
	tests := []struct {
		name string
		in   unavailableProvider
		want string
	}{
		{"load failure is already a full line", unavailableProvider{Name: "go", Reason: "provider go: bad config"}, "provider go: bad config"},
		{"no reason shows the name", unavailableProvider{Name: "go"}, "go"},
		{"reason is added in parentheses", unavailableProvider{Name: "go", Reason: "not installed"}, "go (not installed)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.in.String())
		})
	}
}

func TestEntryCount(t *testing.T) {
	entries := func(n int) provider.CleanResult {
		return provider.CleanResult{Entries: make([]provider.Entry, n)}
	}
	assert.Empty(t, entryCount(entries(0)))
	assert.Equal(t, ", 1 entry", entryCount(entries(1)))
	assert.Equal(t, ", 3 entries", entryCount(entries(3)))
}

func TestSkippedCount(t *testing.T) {
	assert.Empty(t, skippedCount(provider.CleanResult{}))
	assert.Equal(t, ", 2 skipped", skippedCount(provider.CleanResult{SkippedEntries: 2}))
}

func TestWriteUnlisted_ListsOnlyEntriesTheOutputDoesNotName(t *testing.T) {
	entries := []provider.Entry{
		{Path: "/cache/named", Size: 1024},
		{Path: "/cache/silent", Size: 2048},
	}

	var buf bytes.Buffer
	writeUnlisted(&buf, entries, "deleted /cache/named\n")

	assert.Equal(t, "removed: /cache/silent (2.0 KiB)\n", buf.String())
}

func TestWriteUnlisted_CapsTheListAndCountsTheRest(t *testing.T) {
	entries := make([]provider.Entry, 205)
	for i := range entries {
		entries[i] = provider.Entry{Path: fmt.Sprintf("/cache/e%03d", i), Size: 1}
	}

	var buf bytes.Buffer
	writeUnlisted(&buf, entries, "")

	assert.Contains(t, buf.String(), "removed: /cache/e199 (1 B)\n")
	assert.NotContains(t, buf.String(), "/cache/e200")
	assert.Contains(t, buf.String(), "... and 5 more removed entries\n")
}

func TestPrintSkipped(t *testing.T) {
	tests := []struct {
		name           string
		dryRun, quiet  bool
		wantOut, wantE string
	}{
		{"quiet goes to stderr", false, true, "", "skipped npm: busy\n"},
		{"dry-run names the provider", true, false, "[dry-run] npm: skipped (busy)\n", ""},
		{"normal run continues the provider line", false, false, "skipped (busy)\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr string
			stdout := captureStdout(t, func() {
				stderr = captureStderr(t, func() { printSkipped("npm", "busy", tt.dryRun, tt.quiet) })
			})

			assert.Equal(t, tt.wantOut, stdout)
			assert.Equal(t, tt.wantE, stderr)
		})
	}
}
