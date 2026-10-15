//go:build !windows

package provider

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const aliasServerScript = `import os, sys, time
print("up", flush=True)
deadline = time.time() + 120
while not os.path.exists(sys.argv[1]) and time.time() < deadline:
    time.sleep(0.1)
import six
print("imported", six.__version__, flush=True)
`

func uvxEnv(cache string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "UV_CACHE_DIR=") && !strings.HasPrefix(kv, "UV_NO_CACHE=") {
			env = append(env, kv)
		}
	}
	return append(env, "UV_CACHE_DIR="+cache)
}

func environmentLinks(t *testing.T, cache string) []string {
	t.Helper()
	links, err := filepath.Glob(filepath.Join(cache, "environments-v*", "*", "*"))
	require.NoError(t, err)
	return links
}

// TestUVProvider_RealUvxServerThroughAliasedCache runs a real uvx server from
// a temporary cache that is reached through symlinks and checks that the
// sweep keeps the server's environment and removes a stale one. It needs uvx
// and a package index; without them it skips.
func TestUVProvider_RealUvxServerThroughAliasedCache(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a real uvx server")
	}
	uvx, err := exec.LookPath("uvx")
	if err != nil {
		t.Skip("uvx not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cache := filepath.Join(base, "cache")
	require.NoError(t, os.Mkdir(cache, 0o700))
	configured := filepath.Join(base, "configured")
	spelledByServer := filepath.Join(base, "server")
	require.NoError(t, os.Symlink(cache, configured))
	require.NoError(t, os.Symlink(configured, spelledByServer))

	seed := exec.CommandContext(t.Context(), uvx, "--from", "packaging", "python", "-c", "import packaging")
	seed.Env = uvxEnv(spelledByServer)
	if out, seedErr := seed.CombinedOutput(); seedErr != nil {
		t.Skipf("cannot seed a stale environment (no package index?): %v: %s", seedErr, out)
	}
	stale := environmentLinks(t, cache)
	require.NotEmpty(t, stale)

	script := filepath.Join(base, "server.py")
	require.NoError(t, os.WriteFile(script, []byte(aliasServerScript), 0o600))
	proceed := filepath.Join(base, "proceed")
	server := exec.CommandContext(t.Context(), uvx, "--from", "six", "python", script, proceed)
	server.Env = uvxEnv(spelledByServer)
	server.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderrFile, err := os.Create(filepath.Join(base, "stderr"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stderrFile.Close() })
	server.Stderr = stderrFile
	stderr := func() string {
		out, _ := os.ReadFile(stderrFile.Name())
		return string(out)
	}
	stdout, err := server.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, server.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-server.Process.Pid, syscall.SIGKILL)
		_ = server.Wait()
	})
	lines := bufio.NewScanner(stdout)
	up := make(chan bool, 1)
	go func() { up <- lines.Scan() && strings.TrimSpace(lines.Text()) == "up" }()
	select {
	case ok := <-up:
		if !ok {
			t.Skipf("uvx server did not start: %s", stderr())
		}
	case <-time.After(90 * time.Second):
		t.Skip("uvx server did not start in time")
	}

	all := environmentLinks(t, cache)
	require.Greater(t, len(all), len(stale))
	p := newUVProvider(t, config.Provider{Paths: []string{configured}})
	p.uvxMinAge = time.Nanosecond
	p.busy.listProcesses = processTableLister(p.busy.processes)

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	if strings.HasPrefix(res.SkipReason, "uv is running") || strings.Contains(res.SkipReason, "uv started (pid") {
		t.Skipf("an unrelated uv process is running: %s", res.SkipReason)
	}
	require.Empty(t, res.SkipReason)
	assert.Empty(t, res.Warnings)

	left := environmentLinks(t, cache)
	for _, link := range stale {
		assert.NoFileExists(t, link, "the stale environment must be removed")
	}
	assert.Len(t, left, len(all)-len(stale), "the server's environment must stay")
	for _, link := range left {
		info, statErr := os.Stat(link)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir())
	}

	require.NoError(t, os.WriteFile(proceed, nil, 0o600))
	require.True(t, lines.Scan(), "the server must still import its module: %s", stderr())
	assert.True(t, strings.HasPrefix(lines.Text(), "imported"), lines.Text())
}
