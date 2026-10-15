package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteOverrides_LeavesTheRestOfTheFileByteForByte(t *testing.T) {
	tests := []struct {
		name    string
		entries []Override
		before  string
		after   string
	}{
		{
			name:    "comment on an anchor line stays on it",
			entries: []Override{{Name: "npm"}},
			before:  "providers:\n  base: &b # about base\n    max_size: 5G\n  npm: *b # use base\n",
			after:   "providers:\n  base: &b # about base\n    max_size: 5G\n  npm: # use base\n    <<: *b\n    enabled: true\n",
		},
		{
			name:    "enabling an alias does not copy the anchor's comments",
			entries: []Override{{Name: "npm"}},
			before:  "providers:\n  base: &b\n    max_size: 5G # five\n  npm: *b\n",
			after:   "providers:\n  base: &b\n    max_size: 5G # five\n  npm:\n    <<: *b\n    enabled: true\n",
		},
		{
			name:    "CRLF stays CRLF and blank lines stay",
			entries: []Override{{Name: "npm"}},
			before:  "version: \"1\"\r\n# note\r\n\r\nproviders:\r\n  go-build:\r\n    max_size: 20G\r\n\r\nauto:\r\n  min_free: 1G\r\n",
			after:   "version: \"1\"\r\n# note\r\n\r\nproviders:\r\n  go-build:\r\n    max_size: 20G\r\n  npm:\r\n    enabled: true\r\n\r\nauto:\r\n  min_free: 1G\r\n",
		},
		{
			name:    "a new section goes to the end and keeps the style",
			entries: []Override{{Name: "npm"}},
			before:  "version: \"1\"\nauto:\n    min_free: 1G",
			after:   "version: \"1\"\nauto:\n    min_free: 1G\nproviders:\n  npm:\n    enabled: true\n",
		},
		{
			name:    "four space indentation is followed",
			entries: []Override{{Name: "npm"}, {Name: "pip"}},
			before:  "providers:\n    npm:\n        max_size: 1G\n    # trailing note\nauto:\n    min_free: 1G",
			after:   "providers:\n    npm:\n        max_size: 1G\n        enabled: true\n    # trailing note\n    pip:\n        enabled: true\nauto:\n    min_free: 1G",
		},
		{
			name:    "a disabled entry flips in place with its comment",
			entries: []Override{{Name: "pip"}},
			before:  "providers:\n  pip:\n    enabled: false # off for now\n    max_size: 1G\n",
			after:   "providers:\n  pip:\n    enabled: true # off for now\n    max_size: 1G\n",
		},
		{
			name:    "an entry without enabled gains it at the end of its keys",
			entries: []Override{{Name: "npm"}},
			before:  "providers:\n  npm:\n    paths:\n      - a\n      - b\n  other:\n    enabled: true\n",
			after:   "providers:\n  npm:\n    paths:\n      - a\n      - b\n    enabled: true\n  other:\n    enabled: true\n",
		},
		{
			name:    "a custom entry is rendered once and the rest stays",
			entries: []Override{{Name: "custom-x", Define: &Provider{Type: TypeDirPattern, Paths: []string{"~/.cache/[x]"}, MaxSize: "5G", MinIdle: "30d"}}},
			before:  "# mine\nproviders:\n  npm:\n    enabled: true\n\n\n",
			after:   "# mine\nproviders:\n  npm:\n    enabled: true\n  custom-x:\n    max_size: 5G\n    paths:\n      - ~/.cache/[x]\n    enabled: true\n    type: dir-pattern\n    min_idle: 30d\n\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loader, path := overridesLoader(t, tc.before)

			changed, err := loader.WriteOverrides(tc.entries)
			require.NoError(t, err)
			require.NotEmpty(t, changed)

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tc.after, string(data))
		})
	}
}

func TestWriteOverrides_FallsBackToReencodingForFlowStyle(t *testing.T) {
	loader, path := overridesLoader(t, "providers: {}\n")

	changed, err := loader.WriteOverrides([]Override{{Name: "npm"}})

	require.NoError(t, err)
	assert.Equal(t, []string{"npm"}, changed)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.YAMLEq(t, "providers:\n  npm:\n    enabled: true\n", string(data))
}

func TestWriteOverrides_ListsARepeatedNameOnce(t *testing.T) {
	loader, _ := overridesLoader(t, "")

	changed, err := loader.WriteOverrides([]Override{{Name: "npm"}, {Name: "pip"}, {Name: "npm"}})

	require.NoError(t, err)
	assert.Equal(t, []string{"npm", "pip"}, changed)
}

func TestWriteOverrides_ParallelWritersKeepEveryProvider(t *testing.T) {
	_, path := overridesLoader(t, "version: \"1\"\n# keep\n")
	names := make([]string, 12)
	for i := range names {
		names[i] = fmt.Sprintf("tool-%02d", i)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Go(func() {
			loader := NewLoader()
			loader.SetConfigPath(path)
			_, errs[i] = loader.WriteOverrides([]Override{{Name: name}})
		})
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, name := range names {
		assert.Contains(t, string(data), name+":\n    enabled: true", name)
	}
	assert.Contains(t, string(data), "# keep")
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp-", "temp file left behind")
	}
}
