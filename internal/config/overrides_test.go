package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func overridesLoader(t *testing.T, content string) (loader *Loader, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "sub", "config.yaml")
	if content != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	loader = NewLoader()
	loader.SetConfigPath(path)
	return loader, path
}

func TestWriteOverrides_CreatesFileWithOnlyTheChosenProviders(t *testing.T) {
	loader, path := overridesLoader(t, "")

	changed, err := loader.WriteOverrides([]Override{{Name: "npm"}, {Name: "pip"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"npm", "pip"}, changed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.YAMLEq(t, `
version: "1"
providers:
  npm:
    enabled: true
  pip:
    enabled: true
`, string(data))
}

func TestWriteOverrides_KeepsUnrelatedKeysAndComments(t *testing.T) {
	loader, path := overridesLoader(t, `version: "1"
# keep this note
auto:
  min_free: 40G
protected:
  - ~/keepme
providers:
  go-build:
    max_size: 20G
  npm:
    enabled: false
    max_age: 7d
`)

	changed, err := loader.WriteOverrides([]Override{{Name: "npm"}, {Name: "pip"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"npm", "pip"}, changed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "# keep this note")
	assert.YAMLEq(t, `
version: "1"
auto:
  min_free: 40G
protected:
  - ~/keepme
providers:
  go-build:
    max_size: 20G
  npm:
    enabled: true
    max_age: 7d
  pip:
    enabled: true
`, string(data))

	cfg, err := loader.Load()
	require.NoError(t, err)
	assert.True(t, cfg.Providers["npm"].Enabled)
	assert.Equal(t, "20G", cfg.Providers["go-build"].MaxSize)
	assert.Equal(t, "40G", cfg.Auto.MinFree)
}

func TestWriteOverrides_IsIdempotent(t *testing.T) {
	loader, path := overridesLoader(t, "providers:\n  npm:\n    enabled: true\n")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	changed, err := loader.WriteOverrides([]Override{{Name: "npm"}})
	require.NoError(t, err)
	assert.Empty(t, changed)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

func TestWriteOverrides_DefinesCustomProviderOnce(t *testing.T) {
	loader, _ := overridesLoader(t, "")
	def := Provider{Type: TypeDirPattern, Paths: []string{"~/.cache/[t]ool"}, MaxSize: "5G", MinIdle: "30d"}

	changed, err := loader.WriteOverrides([]Override{{Name: "custom-tool", Define: &def}})
	require.NoError(t, err)
	assert.Equal(t, []string{"custom-tool"}, changed)

	cfg, err := loader.Load()
	require.NoError(t, err)
	got := cfg.Providers["custom-tool"]
	assert.True(t, got.Enabled)
	assert.Equal(t, TypeDirPattern, got.Type)
	assert.Equal(t, def.Paths, got.Paths)
	require.NoError(t, cfg.Validate())

	other := Provider{Type: TypeDirPattern, Paths: []string{"~/.cache/[o]ther"}, MaxSize: "1G"}
	_, err = loader.WriteOverrides([]Override{{Name: "custom-tool", Define: &other}})
	require.NoError(t, err)
	cfg, err = loader.Load()
	require.NoError(t, err)
	assert.Equal(t, def.Paths, cfg.Providers["custom-tool"].Paths)
}

func TestWriteOverrides_RejectsWithoutTouchingTheFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		entry   Override
		wantErr string
	}{
		{
			name:    "definition without a glob",
			entry:   Override{Name: "custom-x", Define: &Provider{Type: TypeDirPattern, Paths: []string{"~/.cache/x"}, MaxSize: "1G"}},
			wantErr: "glob",
		},
		{name: "blank name", entry: Override{}, wantErr: "name is required"},
		{
			name:    "providers is a list",
			content: "providers:\n  - npm\n",
			entry:   Override{Name: "npm"},
			wantErr: "not a mapping",
		},
		{
			name:    "entry is a scalar",
			content: "providers:\n  npm: yes please\n",
			entry:   Override{Name: "npm"},
			wantErr: "not a mapping",
		},
		{
			name:    "top level is a list",
			content: "- a\n- b\n",
			entry:   Override{Name: "npm"},
			wantErr: "not a mapping",
		},
		{
			name:    "broken yaml",
			content: "providers: [\n",
			entry:   Override{Name: "npm"},
			wantErr: "parse",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loader, path := overridesLoader(t, tc.content)

			_, err := loader.WriteOverrides([]Override{tc.entry})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			data, readErr := os.ReadFile(path)
			if tc.content == "" {
				assert.ErrorIs(t, readErr, os.ErrNotExist)
				return
			}
			require.NoError(t, readErr)
			assert.Equal(t, tc.content, string(data))
		})
	}
}

func TestWriteOverrides_TurnsOnADisabledEntryWithNoOtherChange(t *testing.T) {
	loader, path := overridesLoader(t, "providers:\n  npm:\n    enabled: false\n")

	changed, err := loader.WriteOverrides([]Override{{Name: "npm"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"npm"}, changed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.YAMLEq(t, "providers:\n  npm:\n    enabled: true\n", string(data))
}
