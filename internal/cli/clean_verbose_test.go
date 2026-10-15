package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scriptedProvider struct{ result provider.CleanResult }

func (scriptedProvider) Name() string                               { return "scripted" }
func (scriptedProvider) Paths() []string                            { return nil }
func (scriptedProvider) CurrentSize(context.Context) (int64, error) { return 0, nil }
func (scriptedProvider) MaxSize() int64                             { return 0 }
func (scriptedProvider) MaxAge() time.Duration                      { return 0 }
func (scriptedProvider) Available() bool                            { return true }
func (s scriptedProvider) Clean(context.Context, provider.CleanOptions) (provider.CleanResult, error) {
	return s.result, nil
}

func scriptedResult() provider.CleanResult {
	var entries []provider.Entry
	var output strings.Builder
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		entries = append(entries, provider.Entry{Path: "/x/" + name, Size: 10})
		output.WriteString("removed: /x/" + name + "\n")
	}
	return provider.CleanResult{
		Output:       output.String(),
		Entries:      entries,
		BytesCleaned: 80,
		Warnings:     []string{"cannot sweep leftover /x/.bilgie-trash-target-abcdefgh: permission denied"},
	}
}

func runScripted(t *testing.T, opts cleanOptions) string {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		err = executeClean(context.Background(), []provider.Provider{scriptedProvider{scriptedResult()}}, nil, opts, provider.CleanModeFull)
	})
	require.NoError(t, err)
	return out
}

func TestRealCleanVerboseListsEveryEntry(t *testing.T) {
	out := runScripted(t, cleanOptions{force: true, verbose: true})

	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		assert.Contains(t, out, "removed: /x/"+name)
	}
	assert.NotContains(t, out, "more (--verbose lists all)")
}

func TestRealCleanConciseKeepsTheShortList(t *testing.T) {
	out := runScripted(t, cleanOptions{force: true})

	assert.Contains(t, out, "and 3 more")
	assert.NotContains(t, out, "removed: /x/h")
}

func TestRealCleanShowsWarningsInTextAndJSON(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		out := runScripted(t, cleanOptions{force: true, verbose: verbose})
		assert.Contains(t, out, "warning: cannot sweep leftover")
	}

	var parsed CleanOutput
	require.NoError(t, json.Unmarshal([]byte(runScripted(t, cleanOptions{force: true, json: true})), &parsed))
	require.Len(t, parsed.Providers, 1)
	require.NotNil(t, parsed.Providers[0].Summary)
	assert.Len(t, parsed.Providers[0].Summary.Warnings, 1)
}

func TestRealCleanConciseShowsSkippedCount(t *testing.T) {
	result := scriptedResult()
	result.SkippedEntries = 2
	var err error
	out := captureStdout(t, func() {
		err = executeClean(context.Background(), []provider.Provider{scriptedProvider{result}}, nil,
			cleanOptions{force: true}, provider.CleanModeFull)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "done (freed 80 B, 8 entries, 2 skipped)")
}

func TestRealCleanVerboseListsEntriesTheOutputOmits(t *testing.T) {
	result := provider.CleanResult{
		Output:       "removed 2 entries, deleted 0 files",
		Entries:      []provider.Entry{{Path: "/x/a", Size: 10}, {Path: "/x/b", Size: 20}},
		BytesCleaned: 30,
	}
	var err error
	out := captureStdout(t, func() {
		err = executeClean(context.Background(), []provider.Provider{scriptedProvider{result}}, nil,
			cleanOptions{force: true, verbose: true}, provider.CleanModeFull)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "removed: /x/a")
	assert.Contains(t, out, "removed: /x/b")
}

func TestRealCleanVerboseShowsWhyEachHeldEntryStays(t *testing.T) {
	result := provider.CleanResult{
		Output: "over limit (2.0 GiB > 1.0 GiB) but nothing is evictable\n" +
			"skip: /x/a (in use by a running process)\nskip: /x/b (modified 3m0s ago)",
		SkipReason:     "over limit but nothing is evictable",
		SkippedEntries: 2,
	}
	run := func(opts cleanOptions) string {
		var err error
		out := captureStdout(t, func() {
			err = executeClean(context.Background(), []provider.Provider{scriptedProvider{result}}, nil, opts, provider.CleanModeSmart)
		})
		require.NoError(t, err)
		return out
	}

	verbose := run(cleanOptions{force: true, verbose: true})
	assert.Contains(t, verbose, "skip: /x/a (in use by a running process)")
	assert.Contains(t, verbose, "skip: /x/b (modified 3m0s ago)")
	verboseDry := run(cleanOptions{dryRun: true, verbose: true})
	assert.Contains(t, verboseDry, "skip: /x/a (in use by a running process)")

	assert.NotContains(t, run(cleanOptions{force: true}), "skip: /x/a")
	assert.NotContains(t, run(cleanOptions{force: true, verbose: true, quiet: true}), "skip: /x/a")

	var parsed CleanOutput
	require.NoError(t, json.Unmarshal([]byte(run(cleanOptions{force: true, verbose: true, json: true})), &parsed))
	require.Len(t, parsed.Providers, 1)
	assert.Contains(t, parsed.Providers[0].Output, "skip: /x/a")
}
