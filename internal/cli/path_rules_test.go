package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loaderFor(t *testing.T, body string) *config.Loader {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))
	loader := config.NewLoader()
	loader.SetConfigPath(cfgPath)
	loader.SkipDefaults()
	return loader
}

const relativeGoBuild = `version: "1"
providers:
  go-build:
    enabled: true
    paths:
      - Documents
    max_size: 1G
    clean_cmd: go clean -cache
`

func TestConfigShow_RelativePathFailsAfterPrintingTheConfig(t *testing.T) {
	loader := loaderFor(t, relativeGoBuild)

	var err error
	out := captureStdout(t, func() { err = runConfigShowWithLoader(loader) })

	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider go-build:")
	assert.Contains(t, err.Error(), `path "Documents" must be absolute or start with ~/`)
	assert.Contains(t, out, "go-build:", "the config is still shown so the user can find the entry")
}

func TestConfigShow_DisabledProviderWithRelativePathPasses(t *testing.T) {
	loader := loaderFor(t, `version: "1"
providers:
  go-build:
    enabled: false
    paths:
      - Documents
    max_size: 1G
`)

	var err error
	captureStdout(t, func() { err = runConfigShowWithLoader(loader) })

	require.NoError(t, err)
}

func TestClean_RelativePathMessageDoesNotDependOnTheCwd(t *testing.T) {
	for _, withDir := range []bool{false, true} {
		cwd := t.TempDir()
		if withDir {
			require.NoError(t, os.MkdirAll(filepath.Join(cwd, "Documents"), 0o700))
		}
		t.Chdir(cwd)

		err := runCleanWithLoader(loaderFor(t, relativeGoBuild), []string{"go-build"}, false, false, true, false, false, os.Stdin)

		require.Error(t, err, "dir present: %t", withDir)
		assert.NotContains(t, err.Error(), "no cache directory found")
		assert.Contains(t, err.Error(), `path "Documents" must be absolute or start with ~/`)
	}
}

func TestClean_UnknownProviderWithNothingEnabledHasNoEmptyAvailableLine(t *testing.T) {
	loader := loaderFor(t, `version: "1"
providers:
  go-build:
    enabled: false
    paths:
      - /nonexistent
    max_size: 1G
`)

	err := runCleanWithLoader(loader, []string{"typo"}, false, false, true, false, false, os.Stdin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown providers: typo")
	assert.NotContains(t, err.Error(), "Available: \n")
	assert.NotRegexp(t, `Available:\s*$`, err.Error())
	assert.Contains(t, err.Error(), "No providers are enabled")

	err = runCleanWithLoader(loader, nil, false, false, true, false, false, os.Stdin)
	require.Error(t, err)
	assert.NotRegexp(t, `Available:\s*$`, err.Error())
	assert.Contains(t, err.Error(), "specify providers or use --all")
}

func TestClean_UnknownProviderListsTheEnabledOnes(t *testing.T) {
	dir := t.TempDir()
	loader := loaderFor(t, `version: "1"
providers:
  go-build:
    enabled: true
    paths:
      - `+dir+`
    max_size: 1G
`)

	err := runCleanWithLoader(loader, []string{"typo"}, false, false, true, false, false, os.Stdin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Available: go-build")
}
