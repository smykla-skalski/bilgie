package auto

import (
	"context"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowProvider frees part of its space, then blocks until its context ends.
type slowProvider struct {
	*fakeProvider
}

func (s slowProvider) Clean(ctx context.Context, o provider.CleanOptions) (provider.CleanResult, error) {
	res, _ := s.fakeProvider.Clean(ctx, o)
	<-ctx.Done()
	return res, ctx.Err()
}

func TestRun_ProviderOverBudgetTimesOutAndNextRuns(t *testing.T) {
	h := newHarness(t)
	h.budget = 250 * time.Millisecond
	h.add("npm", true, false)
	h.add("cargo", true, false)
	h.wrap = map[string]provider.Provider{"npm": slowProvider{h.fakes["npm"]}}

	report, err := h.run(false, 100*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"npm", "cargo"}, h.calls)
	require.Len(t, report.Results, 2)
	assert.True(t, report.Results[0].TimedOut)
	assert.Equal(t, StatusError, report.Results[0].Status)
	assert.Equal(t, gib, report.Results[0].Freed)
	assert.Contains(t, report.Results[0].Err.Error(), "timed out after 250ms")
	assert.False(t, report.Results[1].TimedOut)
	assert.Equal(t, StatusCleaned, report.Results[1].Status)
}

func TestRun_PassDeadlineIsNotReportedAsProviderTimeout(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, false)
	h.wrap = map[string]provider.Provider{"npm": slowProvider{h.fakes["npm"]}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	deps := Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 100 * gib, Total: 1000 * gib}, nil },
		NewProvider: func(name string, _ config.Provider) (provider.Provider, error) { return h.wrap[name], nil },
		Out:         &h.out,
		Home:        h.home,
	}
	report, err := Run(ctx, h.cfg, false, deps)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	for _, r := range report.Results {
		assert.False(t, r.TimedOut)
	}
}

func TestCandidates_PressureTiersOrderByYield(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"npm", "homebrew", "go-build", "docker", "project-artifacts", "pip"} {
		h.add(n, true, false)
	}
	want := map[Tier][]string{
		TierLow:      {"npm", "pip", "homebrew", "go-build", "project-artifacts", "docker"},
		TierCritical: {"docker", "project-artifacts", "go-build", "npm", "pip", "homebrew"},
	}
	for tier, names := range want {
		list := candidates(h.cfg, tier)
		var got []string
		for i := range list {
			got = append(got, list[i].name)
		}
		assert.Equal(t, names, got, tier.String())
	}
}
