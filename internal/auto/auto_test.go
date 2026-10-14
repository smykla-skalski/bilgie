package auto

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gib = int64(1) << 30

func autoCfg() config.Auto {
	return config.Auto{Interval: "30m", MinFree: "30G", MinFreePct: 15, MinFreeCap: "200G"}
}

func TestChooseTier(t *testing.T) {
	total := 1000 * gib

	tests := []struct {
		name string
		cfg  config.Auto
		free int64
		want Tier
	}{
		{"plenty free", autoCfg(), 400 * gib, TierOK},
		{"exactly at min_free and pct", autoCfg(), 150 * gib, TierOK},
		{"under pct floor only", autoCfg(), 100 * gib, TierLow},
		{"under min_free only", config.Auto{MinFree: "200G", MinFreePct: 1}, 150 * gib, TierLow},
		{"pct disabled", config.Auto{MinFree: "30G", MinFreePct: 0}, 40 * gib, TierOK},
		{"just above critical", autoCfg(), 10*gib + 1, TierLow},
		{"exactly critical boundary", autoCfg(), 10 * gib, TierLow},
		{"just under critical", autoCfg(), 10*gib - 1, TierCritical},
		{"just above emergency", autoCfg(), 5*gib + 1, TierCritical},
		{"exactly emergency boundary", autoCfg(), 5 * gib, TierCritical},
		{"just under emergency", autoCfg(), 5*gib - 1, TierEmergency},
		{"nearly full", autoCfg(), 400 << 20, TierEmergency},
		{"large disk default is not permanently low", config.Auto{}, 139 * gib, TierOK},
		{"cap bounds the pct floor", config.Auto{MinFree: "30G", MinFreePct: 15}, 101 * gib, TierOK},
		{"below the cap is low", config.Auto{MinFree: "30G", MinFreePct: 15}, 99 * gib, TierLow},
		{"explicit min_free above the cap is kept", config.Auto{MinFree: "200G", MinFreePct: 15}, 150 * gib, TierLow},
		{"blank settings use defaults", config.Auto{MinFreePct: 0}, 20 * gib, TierLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChooseTier(FreeSpace{Free: tt.free, Total: total}, tt.cfg)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestChooseTier_InvalidMinFree(t *testing.T) {
	_, err := ChooseTier(FreeSpace{Free: gib, Total: gib}, config.Auto{MinFree: "lots"})
	require.Error(t, err)
}

func TestStatfsFree_ReadsRealVolume(t *testing.T) {
	fs, err := StatfsFree(t.TempDir())()

	require.NoError(t, err)
	assert.Positive(t, fs.Total)
	assert.LessOrEqual(t, fs.Free, fs.Total)
}

func TestStatfsFree_MissingPath(t *testing.T) {
	_, err := StatfsFree(filepath.Join(t.TempDir(), "missing"))()
	require.Error(t, err)
}

func TestTierString(t *testing.T) {
	assert.Equal(t, "ok", TierOK.String())
	assert.Equal(t, "low", TierLow.String())
	assert.Equal(t, "critical", TierCritical.String())
	assert.Equal(t, "emergency", TierEmergency.String())
	assert.Equal(t, "unknown", Tier(9).String())
}

type fakeProvider struct {
	calls       *[]string
	opts        *[]provider.CleanOptions
	name        string
	paths       []string
	size        int64
	max         int64
	freed       int64
	skip        string
	err         error
	missing     bool
	onAvailable func()
}

func (f *fakeProvider) Name() string                               { return f.name }
func (f *fakeProvider) Paths() []string                            { return f.paths }
func (f *fakeProvider) CurrentSize(context.Context) (int64, error) { return f.size, nil }
func (f *fakeProvider) MaxSize() int64                             { return f.max }
func (f *fakeProvider) MaxAge() time.Duration                      { return time.Hour }
func (f *fakeProvider) Available() bool {
	if f.onAvailable != nil {
		f.onAvailable()
	}
	return !f.missing
}
func (f *fakeProvider) Clean(_ context.Context, o provider.CleanOptions) (provider.CleanResult, error) {
	*f.calls = append(*f.calls, f.name)
	*f.opts = append(*f.opts, o)
	return provider.CleanResult{BytesCleaned: f.freed, SkipReason: f.skip, Output: "out " + f.name}, f.err
}

type harness struct {
	t      *testing.T
	cfg    *config.Config
	calls  []string
	opts   []provider.CleanOptions
	fakes  map[string]*fakeProvider
	out    bytes.Buffer
	home   string
	wrap   map[string]provider.Provider
	budget time.Duration
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.DefaultConfigFor(config.Platform{OS: config.OSDarwin})
	cfg.Providers = map[string]config.Provider{}
	cfg.Auto = autoCfg()
	return &harness{
		t:     t,
		cfg:   cfg,
		fakes: map[string]*fakeProvider{},
		home:  t.TempDir(),
	}
}

func (h *harness) dir(parts ...string) string {
	h.t.Helper()
	p := filepath.Join(append([]string{h.home}, parts...)...)
	require.NoError(h.t, os.MkdirAll(p, 0o750))
	return p
}

func (h *harness) add(name string, enabled, over bool, mutate ...func(*fakeProvider, *config.Provider)) {
	h.t.Helper()
	path := h.dir("caches", name)
	pc := config.Provider{Enabled: enabled, Paths: []string{path}, MaxSize: "1G"}
	f := &fakeProvider{calls: &h.calls, opts: &h.opts, name: name, paths: []string{path}, max: gib, freed: gib}
	if over {
		f.size = 2 * gib
	}
	for _, m := range mutate {
		m(f, &pc)
	}
	h.cfg.Providers[name] = pc
	h.fakes[name] = f
}

func (h *harness) run(dryRun bool, free ...int64) (Report, error) {
	h.t.Helper()
	return h.runWith(false, dryRun, free...)
}

func (h *harness) runWith(verbose, dryRun bool, free ...int64) (Report, error) {
	h.t.Helper()
	i := 0
	deps := Deps{
		Free: func() (FreeSpace, error) {
			v := free[min(i, len(free)-1)]
			i++
			return FreeSpace{Free: v, Total: 1000 * gib}, nil
		},
		NewProvider: func(name string, _ config.Provider) (provider.Provider, error) {
			if w, ok := h.wrap[name]; ok {
				return w, nil
			}
			f, ok := h.fakes[name]
			if !ok {
				return nil, errors.New("unknown " + name)
			}
			return f, nil
		},
		Out:            &h.out,
		Home:           h.home,
		Verbose:        verbose,
		ProviderBudget: h.budget,
	}
	return Run(h.t.Context(), h.cfg, dryRun, deps)
}

func TestRun_OKTierTrimsOnlyOverLimitProviders(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, true)
	h.add("pip", true, false)
	h.add("go-build", true, true)

	report, err := h.run(false, 400*gib)

	require.NoError(t, err)
	assert.Equal(t, TierOK, report.Tier)
	assert.Equal(t, []string{"npm", "go-build"}, h.calls)
	assert.Contains(t, h.out.String(), "skipped (1):\n  pip: within limit")
	assert.Contains(t, h.out.String(), "total: freed 2.0 GiB across 2 providers; 1 skipped")
	for _, o := range h.opts {
		assert.Equal(t, provider.CleanModeSmart, o.Mode)
		assert.False(t, o.DryRun)
	}
}

func TestRun_LowTierTrimsAllEnabledCheapestFirst(t *testing.T) {
	h := newHarness(t)
	h.add("go-build", true, false)
	h.add("docker", true, false)
	h.add("npm", true, false)
	h.add("homebrew", true, false)
	h.add("mystery", true, false)
	h.add("disabled-one", false, true)

	report, err := h.run(false, 100*gib)

	require.NoError(t, err)
	assert.Equal(t, TierLow, report.Tier)
	assert.Equal(t, []string{"npm", "homebrew", "mystery", "go-build", "docker"}, h.calls)
}

func TestRun_LowTierStopsOnceSpaceRecovers(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, false)
	h.add("homebrew", true, false)
	h.add("go-build", true, false)

	report, err := h.run(false, 100*gib, 100*gib, 100*gib, 400*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"npm", "homebrew"}, h.calls)
	assert.True(t, report.Recovered)
	assert.Contains(t, h.out.String(), "free space recovered")
}

func TestRun_CriticalSweepsFirstEvenWhenDisabled(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, false)
	h.add("sail-dirs", false, false, func(_ *fakeProvider, pc *config.Provider) { pc.Type = config.TypeDirPattern })

	report, err := h.run(false, 2*gib)

	require.NoError(t, err)
	assert.Equal(t, TierEmergency, report.Tier)
	assert.Equal(t, []string{"sail-dirs", "npm"}, h.calls)
}

func TestRun_SweepSkippedWhenTierDropsBelowCritical(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, false)
	h.add("sail-dirs", false, false, func(_ *fakeProvider, pc *config.Provider) { pc.Type = config.TypeDirPattern })

	_, err := h.run(false, 2*gib, 20*gib, 20*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"npm"}, h.calls)
}

func TestRun_DisabledSweepNotRunBelowCritical(t *testing.T) {
	h := newHarness(t)
	h.add("sail-dirs", false, true, func(_ *fakeProvider, pc *config.Provider) { pc.Type = config.TypeDirPattern })

	for _, free := range []int64{400 * gib, 100 * gib, 6 * gib} {
		_, err := h.run(false, free)
		require.NoError(t, err)
	}

	assert.Empty(t, h.calls)
}

func TestRun_EnabledSweepRunsLikeAnyProvider(t *testing.T) {
	h := newHarness(t)
	h.add("sail-dirs", true, true, func(_ *fakeProvider, pc *config.Provider) { pc.Type = config.TypeDirPattern })

	_, err := h.run(false, 100*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"sail-dirs"}, h.calls)
}

func TestRun_NeverRunsDockerVolumes(t *testing.T) {
	for _, free := range []int64{400 * gib, 100 * gib, 1 * gib} {
		h := newHarness(t)
		h.add("docker-volumes", true, true)
		h.add("npm", true, true)

		_, err := h.run(false, free)

		require.NoError(t, err)
		assert.NotContains(t, h.calls, "docker-volumes", "free=%d", free)
		assert.Contains(t, h.calls, "npm")
	}
}

func TestRun_NeverRunsDockerVolumesEvenAsSweep(t *testing.T) {
	h := newHarness(t)
	h.add("docker-volumes", false, true, func(_ *fakeProvider, pc *config.Provider) { pc.Type = config.TypeDirPattern })

	_, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Empty(t, h.calls)
}

func TestRun_SkipsProtectedPaths(t *testing.T) {
	tests := []struct {
		name string
		path []string
	}{
		{"downloads", []string{"Downloads", "cache"}},
		{"downloads root", []string{"Downloads"}},
		{"opencode data", []string{".local", "share", "opencode", "x"}},
		{"opencode config", []string{".config", "opencode"}},
		{"home itself", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			path := h.dir(tt.path...)
			h.add("victim", true, true, func(f *fakeProvider, pc *config.Provider) {
				pc.Paths = []string{path}
				f.paths = []string{path}
			})

			_, err := h.run(false, 1*gib)

			require.NoError(t, err)
			assert.Empty(t, h.calls)
			assert.Contains(t, h.out.String(), "protected path")
		})
	}
}

func TestRun_ProtectedPathReachedThroughSymlink(t *testing.T) {
	h := newHarness(t)
	target := h.dir("Downloads", "stuff")
	link := filepath.Join(h.dir("caches"), "alias")
	require.NoError(t, os.Symlink(target, link))
	h.add("victim", true, true, func(f *fakeProvider, pc *config.Provider) {
		pc.Paths = []string{link}
		f.paths = []string{link}
	})

	_, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Empty(t, h.calls)
}

func TestRun_DryRunPassesFlagAndNeverRecovers(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, false)
	h.add("pip", true, false)

	report, err := h.run(true, 100*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"npm", "pip"}, h.calls)
	for _, o := range h.opts {
		assert.True(t, o.DryRun)
	}
	assert.Contains(t, h.out.String(), "dry-run, nothing is deleted")
	assert.Equal(t, StatusDryRun, report.Results[0].Status)
}

func TestRun_AppliesDockerTimeout(t *testing.T) {
	h := newHarness(t)
	h.add("docker", true, true)

	_, err := h.run(false, 100*gib)

	require.NoError(t, err)
	require.Len(t, h.opts, 1)
	assert.Equal(t, DockerCleanTimeout, h.opts[0].Timeout)
	assert.Positive(t, h.opts[0].Timeout)
}

func TestRun_ReportsSkipsAndErrorsWithoutAborting(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, true, func(f *fakeProvider, _ *config.Provider) { f.skip = "npm is running" })
	h.add("pip", true, true, func(f *fakeProvider, _ *config.Provider) { f.err = errors.New("boom") })
	h.add("yarn", true, true)
	h.add("gone", true, true, func(f *fakeProvider, _ *config.Provider) { f.missing = true })

	report, err := h.run(false, 100*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"npm", "pip", "yarn"}, h.calls)
	require.Error(t, report.Err())
	assert.Contains(t, report.Err().Error(), "pip: boom")
	assert.Contains(t, h.out.String(), "skipped (2):\n  npm: npm is running\n  gone: unavailable")
	assert.Contains(t, h.out.String(), "pip: error: boom")
	assert.Contains(t, h.out.String(), "total: freed 2.0 GiB across 1 provider; 2 skipped; 1 failed")
}

func TestRun_FreeSpaceErrorDeletesNothing(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, true)

	_, err := Run(t.Context(), h.cfg, false, Deps{
		Free: func() (FreeSpace, error) { return FreeSpace{}, errors.New("no disk") },
		NewProvider: func(string, config.Provider) (provider.Provider, error) {
			return h.fakes["npm"], nil
		},
		Out: &h.out,
	})

	require.ErrorContains(t, err, "no disk")
	assert.Empty(t, h.calls)
}

func TestRun_CancelledContextStops(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Run(ctx, h.cfg, false, Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 100 * gib, Total: 1000 * gib}, nil },
		NewProvider: func(string, config.Provider) (provider.Provider, error) { return h.fakes["npm"], nil },
		Out:         &h.out,
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, h.calls)
}

func TestRun_ProviderLoadErrorIsReported(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, true)
	delete(h.fakes, "npm")

	report, err := h.run(false, 100*gib)

	require.NoError(t, err)
	require.Error(t, report.Err())
}

func TestFirstRunMarkerLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	assert.False(t, FirstRunPending(dir))

	require.NoError(t, MarkFirstRunPending(dir))
	assert.True(t, FirstRunPending(dir))

	require.NoError(t, ClearFirstRun(dir))
	assert.False(t, FirstRunPending(dir))
	require.NoError(t, ClearFirstRun(dir), "clearing twice is fine")
}

func TestFirstRunPending_UnreadableCountsAsPending(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := os.Stat(filepath.Join(dir, firstRunMarker)); err == nil || os.IsNotExist(err) {
		t.Skip("directory permissions not enforced")
	}
	assert.True(t, FirstRunPending(dir))
}

func TestRunLockIsExclusive(t *testing.T) {
	dir := t.TempDir()

	first, ok, err := AcquireRunLock(dir)
	require.NoError(t, err)
	require.True(t, ok)

	second, ok, err := AcquireRunLock(dir)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, second)

	first.Release()
	third, ok, err := AcquireRunLock(dir)
	require.NoError(t, err)
	assert.True(t, ok)
	third.Release()
}

type recorder struct {
	mu    sync.Mutex
	calls [][]string
	fail  map[string]error
}

func (r *recorder) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(args) > 0 {
		if err := r.fail[args[0]]; err != nil {
			return []byte(err.Error()), err
		}
	}
	return nil, nil
}

func newAgent(t *testing.T, rec *recorder) Agent {
	t.Helper()
	home := t.TempDir()
	return Agent{
		Exec:       rec.exec,
		Out:        &bytes.Buffer{},
		Home:       home,
		Exe:        "/opt/homebrew/bin/bilgie",
		StateDir:   filepath.Join(home, "state"),
		UID:        501,
		Interval:   45 * time.Minute,
		RetryDelay: time.Nanosecond,
		OS:         "darwin",
	}
}

func TestAgentInstall_WritesPlistArmsDryRunAndLoads(t *testing.T) {
	rec := &recorder{}
	a := newAgent(t, rec)

	require.NoError(t, a.Install(t.Context()))

	data, err := os.ReadFile(a.PlistPath())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(a.Home, "Library", "LaunchAgents", AgentLabel+".plist"), a.PlistPath())
	assert.True(t, FirstRunPending(a.StateDir))
	assert.Equal(t, [][]string{
		{"launchctl", "bootout", "gui/501/" + AgentLabel},
		{"launchctl", "bootstrap", "gui/501", a.PlistPath()},
	}, rec.calls)
	assert.Contains(t, string(data), "<integer>2700</integer>")
	info, err := os.Stat(a.PlistPath())
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	}
}

func TestAgentInstall_BootstrapFailureIsReturned(t *testing.T) {
	rec := &recorder{fail: map[string]error{"bootstrap": errors.New("exit 5")}}
	a := newAgent(t, rec)

	err := a.Install(t.Context())

	require.ErrorContains(t, err, "launchctl bootstrap")
	assert.ErrorContains(t, err, "exit 5")
}

func TestAgentInstall_RetriesBootstrapUntilItSucceeds(t *testing.T) {
	attempts := 0
	a := newAgent(t, &recorder{})
	a.Exec = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			attempts++
			if attempts < 3 {
				return []byte("Input/output error"), errors.New("exit 5")
			}
		}
		return nil, nil
	}

	require.NoError(t, a.Install(t.Context()))
	assert.Equal(t, 3, attempts)
}

func TestAgentInstall_GivesUpAfterFiveBootstrapAttempts(t *testing.T) {
	attempts := 0
	a := newAgent(t, &recorder{})
	a.Exec = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			attempts++
			return nil, errors.New("exit 5")
		}
		return nil, nil
	}

	require.Error(t, a.Install(t.Context()))
	assert.Equal(t, 5, attempts)
}

func TestAgentInstall_RefusesGoRunBinaryAndRelativePath(t *testing.T) {
	for _, exe := range []string{"/var/folders/x/go-build123/b001/exe/bilgie", "bilgie"} {
		rec := &recorder{}
		a := newAgent(t, rec)
		a.Exe = exe

		require.Error(t, a.Install(t.Context()), exe)
		assert.Empty(t, rec.calls)
		assert.NoFileExists(t, a.PlistPath())
		assert.False(t, FirstRunPending(a.StateDir))
	}
}

func TestAgentUninstall_UnloadsRemovesPlistAndMarker(t *testing.T) {
	rec := &recorder{}
	a := newAgent(t, rec)
	require.NoError(t, a.Install(t.Context()))
	rec.calls = nil

	require.NoError(t, a.Uninstall(t.Context()))

	assert.NoFileExists(t, a.PlistPath())
	assert.False(t, FirstRunPending(a.StateDir))
	assert.Equal(t, [][]string{
		{"launchctl", "bootout", "gui/501/" + AgentLabel},
	}, rec.calls)
}

func TestAgentUninstall_NotInstalledIsNotAnError(t *testing.T) {
	rec := &recorder{fail: map[string]error{"bootout": errors.New(`Could not find service "x" in domain`)}}
	a := newAgent(t, rec)
	out := a.Out.(*bytes.Buffer)

	require.NoError(t, a.Uninstall(t.Context()))
	assert.Contains(t, out.String(), "not installed")
}

func TestRenderPlist_ValidXMLWithExpectedKeys(t *testing.T) {
	data, err := RenderPlist("/Users/me/bin/cache&buster", "/Users/me", "/Users/me/Library/Logs/cb/auto.log", 30*time.Minute)
	require.NoError(t, err)

	dec := xml.NewDecoder(bytes.NewReader(data))
	var tokens []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if cd, ok := tok.(xml.CharData); ok {
			tokens = append(tokens, strings.TrimSpace(string(cd)))
		}
	}
	text := string(data)
	assert.Contains(t, tokens, AgentLabel)
	assert.Contains(t, tokens, "/Users/me/bin/cache&buster", "ampersand must round-trip through escaping")
	assert.Contains(t, tokens, "tick")
	assert.Contains(t, text, "<key>StartInterval</key>\n\t<integer>1800</integer>")
	assert.Contains(t, text, "<key>RunAtLoad</key>\n\t<true/>")
	assert.Contains(t, text, "/Users/me/.local/share/mise/shims")
}

func TestRenderPlist_RejectsSubSecondInterval(t *testing.T) {
	_, err := RenderPlist("/bin/x", "/h", "/l", time.Millisecond)
	require.Error(t, err)
}

func TestRenderPlist_PassesPlutilLint(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available")
	}
	data, err := RenderPlist("/opt/homebrew/bin/bilgie", "/Users/me", "/Users/me/log", time.Hour)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "x.plist")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	out, err := exec.CommandContext(t.Context(), plutil, "-lint", path).CombinedOutput()

	require.NoError(t, err, string(out))
}

func TestSortByRebuildCost(t *testing.T) {
	names := []string{"docker", "zzz-unknown", "go-build", "aaa-unknown", "npm", "xcode-archives"}

	sortByRebuildCost(names)

	assert.Equal(t, []string{"npm", "aaa-unknown", "zzz-unknown", "go-build", "docker", "xcode-archives"}, names)
}

func TestPrintPreview_CountsSkipsAndKeepsRemovals(t *testing.T) {
	var out bytes.Buffer

	printPreview(&out, "skip: /a (not a directory)\nwould remove: /b (1 GiB, idle 3h)\nskip: /c (busy)\n")

	assert.Equal(t, "  would remove: /b (1 GiB, idle 3h)\n  2 entries skipped\n", out.String())
}

func TestRun_SkipsCustomProvidersThatPruneVolumes(t *testing.T) {
	h := newHarness(t)
	h.add("my-volumes", true, true, func(_ *fakeProvider, pc *config.Provider) { pc.CleanCmd = "docker volume prune -f" })
	h.add("npm", true, true)

	_, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"npm"}, h.calls)
}

func TestIsProtected_CaseInsensitiveAndCacheLocations(t *testing.T) {
	home := "/Users/me"

	for _, p := range []string{
		"/Users/me/downloads/x",
		"/Users/me/.cache/opencode/models",
		"/Users/me/Library/Caches/opencode",
		"/Users/me/.cache/OpenCode",
	} {
		assert.True(t, isProtected(p, home, true), p)
	}
	for _, p := range []string{
		"/Users/me/work/Worktrees/a",
		"/Users/me/.cache/mise/downloads/opencode",
		"/Users/me/.local/share/mise/downloads/opencode",
		"/Users/me/.cache/tool/Downloads",
	} {
		assert.False(t, isProtected(p, home, false), p)
	}
	assert.False(t, isProtected("/Users/me/.cache/uv", home, true))
	assert.False(t, isProtected("/Users/me/Library/Caches/Homebrew", home, true))
}

func TestAgentUninstall_RealBootoutFailureKeepsPlistAndMarker(t *testing.T) {
	rec := &recorder{}
	a := newAgent(t, rec)
	require.NoError(t, a.Install(t.Context()))
	rec.fail = map[string]error{"bootout": errors.New("Boot-out failed: 5: Input/output error")}

	err := a.Uninstall(t.Context())

	require.ErrorContains(t, err, "launchctl bootout")
	assert.FileExists(t, a.PlistPath())
	assert.True(t, FirstRunPending(a.StateDir))
}

func TestRun_SkipsProviderWhoseTreeContainsWorktrees(t *testing.T) {
	h := newHarness(t)
	root := h.dir("proj")
	writeWorktreeMarker(t, filepath.Join(root, "a", "worktrees", "w"))
	h.add("ancestor", true, true, func(f *fakeProvider, pc *config.Provider) {
		pc.Paths = []string{root}
		f.paths = []string{root}
	})

	_, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Empty(t, h.calls)
	assert.Contains(t, h.out.String(), "protected path")
	assert.Contains(t, h.out.String(), "git checkout at ")
	assert.Contains(t, h.out.String(), filepath.Join("a", "worktrees", "w")+")")
}

func TestRun_AncestorWithHomebrewStyleDownloadsDirIsNotProtected(t *testing.T) {
	h := newHarness(t)
	root := h.dir("brew")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "downloads"), 0o750))
	h.add("homebrew", true, true, func(f *fakeProvider, pc *config.Provider) {
		pc.Paths = []string{root}
		f.paths = []string{root}
	})

	_, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"homebrew"}, h.calls)
}

func TestRun_InterruptDuringProbeStopsRun(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.add("npm", true, true, func(f *fakeProvider, _ *config.Provider) { f.onAvailable = cancel })
	h.add("pip", true, true)

	_, err := Run(ctx, h.cfg, false, Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 100 * gib, Total: 1000 * gib}, nil },
		NewProvider: func(name string, _ config.Provider) (provider.Provider, error) { return h.fakes[name], nil },
		Out:         &h.out,
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, h.calls)
}

func TestRun_AncestorOfWorktreeBuriedBeyondScanDepthIsAcceptedAsClear(t *testing.T) {
	h := newHarness(t)
	root := h.dir("far")
	writeWorktreeMarker(t, filepath.Join(root, "a", "b", "c", "d", "e", "w"))
	h.add("ancestor", true, true, func(f *fakeProvider, pc *config.Provider) {
		pc.Paths = []string{root}
		f.paths = []string{root}
	})

	_, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"ancestor"}, h.calls)
}

func TestRun_DefaultPathProviderIgnoresProtectedNamesInsideTheCache(t *testing.T) {
	for _, sub := range []string{"worktrees", "Downloads", "opencode", "downloads/opencode"} {
		h := newHarness(t)
		root := h.dir("npm-cache")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "a", sub, "x"), 0o750))
		def := config.DefaultProviders()["npm"]
		h.add("npm", true, true, func(f *fakeProvider, pc *config.Provider) {
			*pc = def
			pc.Paths = []string{root}
			f.paths = []string{root}
		})

		_, err := h.run(false, 1*gib)

		require.NoError(t, err)
		assert.Equal(t, []string{"npm"}, h.calls, sub)
		assert.NotContains(t, h.out.String(), "protected path", sub)
	}
}

func TestRun_NeverRunsXcodeArchives(t *testing.T) {
	for _, free := range []int64{1 * gib, 8 * gib, 100 * gib} {
		h := newHarness(t)
		h.add("xcode-archives", true, true)
		h.add("my-archives", true, true, func(f *fakeProvider, pc *config.Provider) {
			p := filepath.Join(h.home, "Library", "Developer", "Xcode", "archives")
			pc.Paths = []string{p}
			f.paths = []string{p}
		})
		h.add("npm", true, true)

		_, err := h.run(false, free)

		require.NoError(t, err)
		assert.Equal(t, []string{"npm"}, h.calls, "free=%d", free)
	}
}

func TestIsXcodeArchives(t *testing.T) {
	assert.True(t, isXcodeArchives("~/Library/Developer/Xcode/Archives"))
	assert.True(t, isXcodeArchives("/Users/me/Library/Developer/Xcode/Archives/2026-01-01"))
	assert.False(t, isXcodeArchives("/Users/me/Library/Developer/Xcode/DerivedData"))
	assert.False(t, isXcodeArchives("/Users/me/Archives"))
}

func TestIsProtected_XcodeArchivesAndAncestors(t *testing.T) {
	home := "/Users/me"
	assert.True(t, isProtected("/Users/me/Library/Developer/Xcode/Archives", home, false))
	assert.True(t, isProtected("/Users/me/Library/Developer/Xcode/Archives/x", home, false))
	assert.True(t, isProtected("/Users/me/Library/Developer", home, false))
	assert.False(t, isProtected("/Users/me/Library/Developer/Xcode/DerivedData", home, false))
}

func TestIsProtected_FilesystemRootAndDataAlias(t *testing.T) {
	home := "/Users/me"
	for _, scan := range []bool{true, false} {
		if !scan {
			assert.True(t, isProtected("/", home, scan), "root, scan=%v", scan)
		}
		assert.True(t, isProtected("/System/Volumes/Data/Users/me/.cache/opencode", home, scan))
		assert.True(t, isProtected("/System/Volumes/Data/Users/me/Library/Developer/Xcode/Archives", home, scan))
		assert.True(t, isProtected("/System/Volumes/Data", home, scan))
	}
	assert.True(t, isProtected("/", home, true))
	assert.True(t, within(filepath.FromSlash("/Users/me"), string(filepath.Separator)))
	assert.False(t, within(filepath.FromSlash("/Users/me2"), filepath.FromSlash("/Users/me")))
}

func TestRun_SkipsUncleanXcodeArchivesPathOutsideHome(t *testing.T) {
	for _, spelling := range []string{"Xcode/./Archives", "Xcode/Foo/../Archives", "Xcode//Archives"} {
		h := newHarness(t)
		h.dir("o", "Xcode", "Archives")
		p := h.home + "/o/" + spelling
		h.add("renamed", true, true, func(f *fakeProvider, pc *config.Provider) { f.paths = []string{p}; pc.Paths = []string{p} })
		h.home = "/nonexistent-home"

		_, err := h.run(false, 1*gib)

		require.NoError(t, err)
		assert.Empty(t, h.calls, spelling)
	}
}

func TestIsProtected_RootWithoutHome(t *testing.T) {
	for _, scan := range []bool{true, false} {
		assert.True(t, isProtected("/", "", scan))
		assert.True(t, isProtected("/System/Volumes/Data", "", scan))
	}
}

func TestReport_Previewed(t *testing.T) {
	ran := []Result{{Status: StatusDryRun}}
	assert.False(t, Report{Tier: TierOK, Results: ran}.Previewed(), "ok tier previews nothing")
	assert.False(t, Report{Tier: TierLow, Results: []Result{{Status: StatusError}, {Status: StatusSkipped}}}.Previewed())
	assert.False(t, Report{Tier: TierLow}.Previewed())
	assert.True(t, Report{Tier: TierLow, Results: []Result{{Status: StatusError}, {Status: StatusDryRun}}}.Previewed())
	assert.True(t, Report{Tier: TierEmergency, Results: ran}.Previewed())
}

func TestAgentUninstall_UnrecognisedBootoutAndPrintFailureKeepsState(t *testing.T) {
	rec := &recorder{}
	a := newAgent(t, rec)
	require.NoError(t, a.Install(t.Context()))
	rec.fail = map[string]error{
		"bootout": errors.New("exit status 5"),
		"print":   errors.New("permission denied"),
	}

	require.ErrorContains(t, a.Uninstall(t.Context()), "launchctl bootout")

	assert.FileExists(t, a.PlistPath())
	assert.True(t, FirstRunPending(a.StateDir))
}

func TestWithoutDataAlias_CaseInsensitive(t *testing.T) {
	got := withoutDataAlias([]string{"/system/volumes/data/Users/me", "/System/Volumes/DataX/y"})
	assert.Contains(t, got, filepath.FromSlash("/Users/me"))
	assert.NotContains(t, got, "X/y")
	assert.Len(t, got, 3)
}

func TestAgentUninstall_UnrecognisedBootoutWithFailingPrintStillRemoves(t *testing.T) {
	rec := &recorder{}
	a := newAgent(t, rec)
	require.NoError(t, a.Install(t.Context()))
	rec.calls = nil
	rec.fail = map[string]error{
		"bootout": errors.New("exit status 5"),
		"print":   errors.New("Could not find service"),
	}

	require.NoError(t, a.Uninstall(t.Context()))

	assert.NoFileExists(t, a.PlistPath())
	assert.False(t, FirstRunPending(a.StateDir))
	assert.Equal(t, [][]string{
		{"launchctl", "bootout", "gui/501/" + AgentLabel},
		{"launchctl", "print", "gui/501/" + AgentLabel},
	}, rec.calls)
}

func TestRun_DockerProviderWithVolumesFlagStillRuns(t *testing.T) {
	h := newHarness(t)
	h.add("docker", true, true, func(_ *fakeProvider, pc *config.Provider) { pc.CleanCmd = "docker system prune -af --volumes" })

	_, err := h.run(false, 1*gib)

	require.NoError(t, err)
	assert.Equal(t, []string{"docker"}, h.calls)
}

type hangingProbe struct{ fakeProvider }

func (hangingProbe) Available() bool { select {} }

func TestRun_InterruptDoesNotWaitForHungProbe(t *testing.T) {
	h := newHarness(t)
	hung := &hangingProbe{}
	h.add("npm", true, true, func(f *fakeProvider, _ *config.Provider) { hung.fakeProvider = *f })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Run(ctx, h.cfg, false, Deps{
		Free:        func() (FreeSpace, error) { return FreeSpace{Free: 100 * gib, Total: 1000 * gib}, nil },
		NewProvider: func(string, config.Provider) (provider.Provider, error) { return hung, nil },
		Out:         &h.out,
		Home:        h.home,
	})

	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestRun_VerbosePrintsEveryResultLikeBefore(t *testing.T) {
	h := newHarness(t)
	h.add("npm", true, true)
	h.add("pip", true, false)

	_, err := h.runWith(true, false, 400*gib)

	require.NoError(t, err)
	assert.Contains(t, h.out.String(), "pip: skipped (within limit)")
	assert.Contains(t, h.out.String(), "npm: freed 1.0 GiB")
	assert.NotContains(t, h.out.String(), "total:")
}

func TestLoadErrors_SkipsProvidersAutoNeverRuns(t *testing.T) {
	h := newHarness(t)
	h.add("renamed-prune", true, true, func(_ *fakeProvider, pc *config.Provider) {
		pc.CleanCmd = "docker volume prune -f"
	})
	h.add("docker-volumes", true, true)
	h.add("broken", true, true)

	failing := func(name string, _ config.Provider) (provider.Provider, error) {
		return nil, errors.New("load " + name)
	}
	errs := LoadErrors(h.cfg, failing)

	require.Len(t, errs, 1)
	assert.EqualError(t, errs[0], "load broken")
}
