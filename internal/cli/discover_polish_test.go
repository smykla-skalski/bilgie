package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForOutput(t *testing.T, out *syncBuffer, want string, count int) {
	t.Helper()
	require.Eventually(t, func() bool { return strings.Count(out.String(), want) >= count }, 5*time.Second, 5*time.Millisecond)
}

func TestDiscover_CtrlCAtThePromptWritesNothing(t *testing.T) {
	tests := []struct {
		name    string
		answers string
		prompts int
	}{
		{name: "at the first prompt", answers: "", prompts: 1},
		{name: "at the second prompt after accepting the first", answers: "y\n", prompts: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiscoverFixture(t)
			f.withSuggestions()
			before := f.config()
			in, feed := io.Pipe()
			t.Cleanup(func() { _ = feed.Close() })
			out := &syncBuffer{}
			f.env.interactive = true
			f.env.in = in
			f.env.out = out
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- runDiscoverWithLoader(ctx, f.loader, f.env, discoverFlags{write: true}, nil)
			}()

			waitForOutput(t, out, "Enable it?", 1)
			if tc.answers != "" {
				_, err := feed.Write([]byte(tc.answers))
				require.NoError(t, err)
			}
			waitForOutput(t, out, "Enable it?", tc.prompts)
			cancel()

			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
				assert.Contains(t, err.Error(), "nothing written")
			case <-time.After(5 * time.Second):
				t.Fatal("discover kept waiting for Enter after Ctrl-C")
			}
			assert.Equal(t, before, f.config())
		})
	}
}

func TestDiscover_ACancelledContextStopsBeforeThePrompt(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()
	before := f.config()
	f.env.interactive = true
	f.env.in = strings.NewReader("y\ny\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := runDiscoverWithLoader(ctx, f.loader, f.env, discoverFlags{write: true}, nil)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, before, f.config())
}

func TestDiscover_ParallelWritesKeepBothProviders(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()
	names := []string{"pip", "huggingface"}

	var wg sync.WaitGroup
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Go(func() {
			loader := config.NewLoader()
			loader.SetConfigPath(f.cfgPath)
			loader.SetPlatform(f.platform)
			env := f.env
			env.out = &syncBuffer{}
			errs[i] = runDiscoverWithLoader(t.Context(), loader, env, discoverFlags{write: true, yes: true}, []string{name})
		})
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	cfg, err := f.loader.Load()
	require.NoError(t, err)
	assert.True(t, cfg.Providers["pip"].Enabled)
	assert.True(t, cfg.Providers["huggingface"].Enabled)
}

func TestDiscover_ANamedProviderListsOnce(t *testing.T) {
	f := newDiscoverFixture(t)
	f.withSuggestions()

	require.NoError(t, f.run(discoverFlags{json: true}, "pip", "huggingface", "pip"))

	var got DiscoverOutput
	require.NoError(t, json.Unmarshal(f.out.Bytes(), &got))
	var names []string
	for _, c := range got.Candidates {
		names = append(names, c.Name)
	}
	assert.Equal(t, []string{"pip", "huggingface"}, names)
}

func TestDiscover_TheFooterNamesACommandThatDoesSomething(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *discoverFixture)
		want    []string
		notWant []string
	}{
		{
			name:    "regenerable only",
			setup:   func(f *discoverFixture) { f.cache("pip", 4096) },
			want:    []string{"Enable regenerable ones with: bilgie discover --write --yes\n"},
			notWant: []string{"Opt-in"},
		},
		{
			name:    "opt-in only",
			setup:   func(f *discoverFixture) { f.cache("huggingface", 8192) },
			want:    []string{"bilgie discover --write --yes huggingface"},
			notWant: []string{"Enable regenerable ones"},
		},
		{
			name:  "both",
			setup: func(f *discoverFixture) { f.cache("pip", 4096); f.cache("huggingface", 8192) },
			want: []string{
				"Enable regenerable ones with: bilgie discover --write --yes\n",
				"bilgie discover --write --yes huggingface",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiscoverFixture(t)
			tc.setup(f)

			require.NoError(t, f.run(discoverFlags{}))

			text := f.out.String()
			for _, want := range tc.want {
				assert.Contains(t, text, want)
			}
			for _, not := range tc.notWant {
				assert.NotContains(t, text, not)
			}
		})
	}
}

func TestDiscover_TheOptInCommandInTheFooterWritesThatProvider(t *testing.T) {
	f := newDiscoverFixture(t)
	f.cache("huggingface", 8192)

	require.NoError(t, f.run(discoverFlags{write: true, yes: true}))
	assert.Contains(t, f.out.String(), "Nothing written.")

	require.NoError(t, f.run(discoverFlags{write: true, yes: true}, "huggingface"))
	cfg, err := f.loader.Load()
	require.NoError(t, err)
	assert.True(t, cfg.Providers["huggingface"].Enabled)
}
