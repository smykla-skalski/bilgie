package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchProcess(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		wanted []string
		want   string
	}{
		{"exact binary", "go build ./...", []string{"go"}, "go"},
		{"absolute path", "/usr/local/go/bin/go test ./...", []string{"go"}, "go"},
		{"second wanted", "/Users/me/.cargo/bin/rustc --crate-name x", []string{"cargo", "rustc"}, "rustc"},
		{"homebrew ruby script", "/opt/homebrew/ruby -W1 /opt/homebrew/Library/Homebrew/brew.rb cleanup", []string{"brew"}, "brew"},
		{"sudo wrapper", "sudo -u bob go build", []string{"go"}, "go"},
		{"env wrapper", "env -u FOO go test ./...", []string{"go"}, "go"},
		{"timeout wrapper", "timeout 60 go test", []string{"go"}, "go"},
		{"xargs", "xargs go vet", []string{"go"}, "go"},
		{"shell -c", `bash -lc "cargo build"`, []string{"cargo"}, "cargo"},
		{"shell chain", "/bin/sh -c cd /x && go build ./...", []string{"go"}, "go"},
		{"uvx belongs to uv", "/opt/homebrew/bin/uvx ruff check", []string{"uv"}, "uv"},
		{"editor arg errs toward busy", "vim go", []string{"go"}, "go"},
		{"prefix is not a match", "gopls serve", []string{"go"}, ""},
		{"similar name", "go-task build", []string{"go"}, ""},
		{"empty line", "", []string{"go"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, matchProcess(tt.line, tt.wanted))
		})
	}
}

func TestMatchProcessFor_YarnForms(t *testing.T) {
	assert.Equal(t, "yarn", matchProcessFor(`C:\Users\u\npm\Yarn.CMD install`, []string{"yarn"}, true))
	assert.Equal(t, "yarn", matchProcessFor("node .yarn/releases/yarn-1.22.19.cjs install", []string{"yarn"}, false))
	assert.Equal(t, "yarn", matchProcessFor("/usr/bin/yarnpkg add x", []string{"yarn"}, false))
}

func TestExcludeSelf_OperatorInBilgieArgument(t *testing.T) {
	procs := []osshim.Process{
		{PID: 50, CommandLine: "sh -c 'bilgie --config /x/R&D/c.yaml clean cargo'"},
		{PID: 60, PPID: 50, CommandLine: "bilgie --config /x/R&D/c.yaml clean cargo"},
	}
	assert.Empty(t, excludeSelfFor(procs, 60, []string{"cargo"}, false))
}

func TestMatchProcessFor_WindowsRulesOnAnyHost(t *testing.T) {
	assert.Equal(t, "cargo", matchProcessFor(`C:\Tools\CARGO.EXE build`, []string{"cargo"}, true))
	assert.Equal(t, "go", matchProcessFor(`"C:\Program Files\Go\bin\go.exe" test`, []string{"go"}, true))
	assert.Empty(t, matchProcessFor(`C:\Tools\cargo-edit.exe`, []string{"cargo"}, true))
	assert.Empty(t, matchProcessFor(`C:\Tools\cargo.exe`, []string{"cargo"}, false), "a backslash is part of a name on POSIX")
}

func TestMatchProcess_WindowsImageNames(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("image-name suffix handling is windows-only")
	}
	assert.Equal(t, "go", matchProcess("go.exe", []string{"go"}))
	assert.Equal(t, "cargo", matchProcess(`C:\Tools\CARGO.EXE`, []string{"cargo"}))
}

func TestExcludeSelf(t *testing.T) {
	wanted := []string{"cargo", "rustc"}
	procs := []osshim.Process{
		{PID: 1, PPID: 0, CommandLine: "/sbin/launchd"},
		{PID: 40, PPID: 1, CommandLine: "sudo sh -c bilgie clean cargo"},
		{PID: 50, PPID: 40, CommandLine: "zsh -c bilgie clean cargo"},
		{PID: 60, PPID: 50, CommandLine: "bilgie clean cargo"},
		{PID: 70, PPID: 1, CommandLine: "vim notes.txt"},
	}

	t.Run("self and wrapper ancestors are dropped", func(t *testing.T) {
		lines := excludeSelf(procs, 60, wanted)
		assert.Equal(t, []string{"vim notes.txt"}, commandLines(lines))
		g := fakeGuard(lines, nil, nil)
		assert.Empty(t, g.busyReason(context.Background()))
	})

	t.Run("real tool process still skips", func(t *testing.T) {
		withTool := slices.Concat(procs, []osshim.Process{{PID: 80, PPID: 1, CommandLine: "/Users/me/.cargo/bin/cargo build"}})
		g := fakeGuard(excludeSelf(withTool, 60, wanted), nil, nil)
		assert.Contains(t, g.busyReason(context.Background()), "cargo is running")
	})

	t.Run("tool ancestor stays visible", func(t *testing.T) {
		parentTool := []osshim.Process{
			{PID: 1, PPID: 0, CommandLine: "/sbin/launchd"},
			{PID: 50, PPID: 1, CommandLine: "/Users/me/.cargo/bin/cargo run -- clean cargo"},
			{PID: 60, PPID: 50, CommandLine: "target/debug/bilgie clean cargo"},
		}
		g := fakeGuard(excludeSelf(parentTool, 60, wanted), nil, nil)
		assert.Contains(t, g.busyReason(context.Background()), "cargo is running")
	})

	t.Run("linux comm suffix does not defeat the wrapper check", func(t *testing.T) {
		linux := []osshim.Process{
			{PID: 50, PPID: 1, CommandLine: "sh -c bilgie clean cargo sh", Args: "sh -c bilgie clean cargo"},
			{PID: 60, PPID: 50, CommandLine: "bilgie clean cargo bilgie", Args: "bilgie clean cargo"},
		}
		assert.Empty(t, excludeSelf(linux, 60, wanted))
	})

	t.Run("tool named later in the ancestor line stays visible", func(t *testing.T) {
		brew := []osshim.Process{
			{PID: 1, PPID: 0, CommandLine: "/sbin/launchd"},
			{PID: 50, PPID: 1, CommandLine: "/opt/homebrew/ruby -W1 /opt/homebrew/Library/Homebrew/brew.rb bundle"},
			{PID: 55, PPID: 50, CommandLine: "/bin/sh -c bilgie clean homebrew"},
			{PID: 60, PPID: 55, CommandLine: "bilgie clean homebrew"},
		}
		g := fakeGuard(excludeSelf(brew, 60, []string{"brew"}), nil, nil)
		g.processes = []string{"brew"}
		assert.Contains(t, g.busyReason(context.Background()), "brew is running")

		spaced := []osshim.Process{
			{PID: 50, PPID: 0, CommandLine: "/Users/John Smith/.cargo/bin/cargo run"},
			{PID: 60, PPID: 50, CommandLine: "bilgie clean cargo"},
		}
		g = fakeGuard(excludeSelf(spaced, 60, wanted), nil, nil)
		assert.Contains(t, g.busyReason(context.Background()), "cargo is running")
	})

	t.Run("cycle among kept ancestors terminates", func(t *testing.T) {
		cyc := []osshim.Process{
			{PID: 60, PPID: 5, CommandLine: "bilgie clean cargo"},
			{PID: 5, PPID: 6, CommandLine: "cargo"},
			{PID: 6, PPID: 5, CommandLine: "cargo"},
		}
		assert.Len(t, excludeSelf(cyc, 60, wanted), 2)
	})

	t.Run("unknown pids and cycles are safe", func(t *testing.T) {
		got := excludeSelf([]osshim.Process{{CommandLine: "cargo build"}}, 0, wanted)
		assert.Equal(t, []string{"cargo build"}, commandLines(got))
		cyc := []osshim.Process{{PID: 5, PPID: 6, CommandLine: "a"}, {PID: 6, PPID: 5, CommandLine: "b"}}
		assert.Empty(t, excludeSelf(cyc, 5, wanted))
	})
}

func TestExcludeSelf_WrapperForms(t *testing.T) {
	wanted := []string{"cargo", "rustc"}
	tests := []struct {
		name    string
		wrapper string
		self    string
		windows bool
		busy    bool
	}{
		{"quoted path with spaces", `sh -c 'bilgie --config "/x y/c.yaml" clean cargo'`, "bilgie --config /x y/c.yaml clean cargo", false, false},
		{"padded whitespace", `bash -c "bilgie  clean  cargo"`, "bilgie clean cargo", false, false},
		{"nested wrappers", `sudo sh -c 'bash -c "bilgie  clean cargo"'`, "bilgie clean cargo", false, false},
		{"env assignment", "env VAR=x bilgie clean cargo", "bilgie clean cargo", false, false},
		{"sudo", "sudo -u bob bilgie clean cargo", "bilgie clean cargo", false, false},
		{"backslash escape", `sh -c bilgie\ clean\ cargo`, "bilgie clean cargo", false, false},
		{"absolute exe in wrapper", "sh -c '/usr/local/bin/bilgie clean cargo'", "bilgie clean cargo", false, false},
		{"cargo inside wrapper stays busy", `sh -c 'cargo run -- clean cargo'`, "bilgie clean cargo", false, true},
		{"trailing semicolon", "sh -c 'bilgie clean cargo; echo done'", "bilgie clean cargo", false, false},
		{"subshell parens", "sh -c '(bilgie clean cargo)'", "bilgie clean cargo", false, false},
		{"background ampersand", "sh -c 'bilgie clean cargo&'", "bilgie clean cargo", false, false},
		{"blank quoted word", "sh -c \"bilgie clean ' ' cargo\"", "bilgie clean cargo", false, false},
		{"windows exe path with spaces", `cmd /c "C:\Program Files\bilgie.exe" clean cargo`, `"C:\Program Files\bilgie.exe" clean cargo`, true, false},
		{"tool before bilgie behind sudo stays busy", "sudo cargo run -- bilgie clean cargo", "bilgie clean cargo", false, true},
		{"tool before bilgie behind env stays busy", "env FOO=1 cargo install foo bilgie clean cargo", "bilgie clean cargo", false, true},
		{"other program with same args stays busy", `sh -c 'go run . clean cargo'`, "bilgie clean cargo", false, true},
		{"unterminated quote stays busy", `sh -c 'bilgie clean cargo`, "bilgie clean cargo", false, true},
		{"unterminated nested quote stays busy", `sh -c "bilgie clean 'cargo"`, "bilgie clean cargo", false, true},
		{"trailing backslash stays busy", `sh -c "bilgie clean cargo" \`, "bilgie clean cargo", false, true},
		{"windows backslash path", `cmd /c "C:\Tools\bilgie.exe" --config "C:\x y\c.yaml" clean cargo`, `C:\Tools\bilgie.exe --config C:\x y\c.yaml clean cargo`, true, false},
		{"windows exe suffix", `cmd /c bilgie.exe clean cargo`, `bilgie clean cargo`, true, false},
		{"operator glued before bilgie", "sh -c 'echo hi;bilgie clean cargo'", "bilgie clean cargo", false, false},
		{"and-operator glued before bilgie", "sh -c 'make&&bilgie clean cargo'", "bilgie clean cargo", false, false},
		{"tool glued before bilgie stays busy", "sh -c 'cargo build;bilgie clean cargo'", "bilgie clean cargo", false, true},
		{"apostrophe in exe path", `sh -c /Users/o'brien/bin/bilgie clean cargo`, `/Users/o'brien/bin/bilgie clean cargo`, false, false},
		{"apostrophe path with tool before stays busy", `sh -c cargo run /Users/o'brien/bin/bilgie clean cargo`, `/Users/o'brien/bin/bilgie clean cargo`, false, true},
		{"apostrophe without exe and args stays busy", `sh -c /Users/o'brien/run clean cargo`, `/Users/o'brien/bilgie clean cargo`, false, true},
		{"windows tool path before bilgie stays busy", `cmd /c C:\Tools\cargo.exe run -- bilgie.exe clean cargo`, `bilgie clean cargo`, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procs := []osshim.Process{
				{PID: 50, PPID: 0, CommandLine: tt.wrapper},
				{PID: 60, PPID: 50, CommandLine: tt.self},
			}
			lines := excludeSelfFor(procs, 60, wanted, tt.windows)
			g := fakeGuard(lines, nil, nil)
			if tt.busy {
				assert.Contains(t, g.busyReason(context.Background()), "cargo is running")
			} else {
				assert.Empty(t, lines)
			}
		})
	}
}

func TestExcludeSelf_LinuxPaddedWrapper(t *testing.T) {
	procs := []osshim.Process{
		{PID: 50, PPID: 0, CommandLine: `bash -c bilgie  clean  cargo bash`, Args: `bash -c "bilgie  clean  cargo"`},
		{PID: 60, PPID: 50, CommandLine: "bilgie clean cargo bilgie", Args: "bilgie clean cargo"},
	}
	assert.Empty(t, excludeSelfFor(procs, 60, []string{"cargo"}, false))
}

func TestProcessLister_OmitsOwnProcess(t *testing.T) {
	lines, err := processLister([]string{"go"})(context.Background())
	require.NoError(t, err)
	all, err := osshim.ProcessTable(context.Background())
	require.NoError(t, err)
	var own string
	for i := range all {
		if all[i].PID == os.Getpid() {
			own = all[i].CommandLine
		}
	}
	require.NotEmpty(t, own)
	assert.Less(t, len(lines), len(all))
}

// procsOf turns bare command lines into process rows.
func procsOf(lines ...string) []osshim.Process {
	procs := make([]osshim.Process, len(lines))
	for i, l := range lines {
		procs[i] = osshim.Process{CommandLine: l, Args: l}
	}
	return procs
}

// listOf is a process lister that returns the given command lines.
func listOf(lines ...string) func(context.Context) ([]osshim.Process, error) {
	return func(context.Context) ([]osshim.Process, error) { return procsOf(lines...), nil }
}

func fakeGuard(procs []osshim.Process, listErr error, held map[string]bool) *busyGuard {
	return &busyGuard{
		processes: []string{"cargo", "rustc"},
		listProcesses: func(context.Context) ([]osshim.Process, error) {
			return procs, listErr
		},
		lockHeld: func(path string) (bool, error) {
			return held[path], nil
		},
	}
}

func TestBusyGuard_BusyReason(t *testing.T) {
	ctx := context.Background()

	t.Run("nil guard is never busy", func(t *testing.T) {
		var g *busyGuard
		assert.Empty(t, g.busyReason(ctx))
	})

	t.Run("process running", func(t *testing.T) {
		g := fakeGuard(procsOf("zsh", "cargo build --release"), nil, nil)
		assert.Equal(t, "cargo is running", g.busyReason(ctx))
	})

	t.Run("no matching process", func(t *testing.T) {
		g := fakeGuard(procsOf("zsh", "vim main.go"), nil, nil)
		assert.Empty(t, g.busyReason(ctx))
	})

	t.Run("lock held", func(t *testing.T) {
		g := fakeGuard(nil, nil, map[string]bool{"/c/.lock": true})
		g.locks = []string{"/c/.lock"}
		assert.Equal(t, "lock held: /c/.lock", g.busyReason(ctx))
	})

	t.Run("process list failure counts as busy", func(t *testing.T) {
		g := fakeGuard(nil, errors.New("ps denied"), nil)
		assert.Contains(t, g.busyReason(ctx), "cannot list processes: ps denied")
	})

	t.Run("lock check failure counts as busy", func(t *testing.T) {
		g := fakeGuard(nil, nil, nil)
		g.locks = []string{"/c/.lock"}
		g.lockHeld = func(string) (bool, error) { return false, errors.New("boom") }
		assert.Contains(t, g.busyReason(ctx), "cannot check lock /c/.lock: boom")
	})
}

func lockFile(t *testing.T, path string) {
	t.Helper()
	release, acquired, err := osshim.TryLock(path)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(release)
}

func TestCommandProvider_SkipsWhenToolRunning(t *testing.T) {
	cacheDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	p, err := NewCommandProvider("go-build", config.Provider{
		Enabled:  true,
		Paths:    []string{cacheDir},
		MaxSize:  "1G",
		CleanCmd: "touch " + marker,
	})
	require.NoError(t, err)
	p.busy.listProcesses = listOf("/usr/local/go/bin/go build ./...")

	result, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.Equal(t, "go is running", result.SkipReason)
	assert.NoFileExists(t, marker, "busy provider must not run its clean command")

	dry, err := p.Clean(context.Background(), CleanOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, "go is running", dry.SkipReason)
}

func TestNewBusyGuard(t *testing.T) {
	assert.Nil(t, newBusyGuard("npm", []string{"/x"}))

	uv := newBusyGuard("uv", []string{"/a", "/b"})
	require.NotNil(t, uv)
	assert.Equal(t, []string{filepath.Join("/a", ".lock"), filepath.Join("/b", ".lock")}, uv.locks)
	assert.Equal(t, []string{"uv"}, uv.processes)

	assert.Nil(t, newBusyGuard("cargo", nil))

	cargo := newBusyGuard("cargo", []string{"/h/.cargo/registry", "/h/.cargo/git"})
	require.NotNil(t, cargo)
	assert.Empty(t, cargo.processes, "a running cargo process alone must not block")
	assert.Equal(t, []string{
		filepath.Join("/h/.cargo", ".package-cache"),
		filepath.Join("/h/.cargo", ".package-cache-mutate"),
	}, cargo.locks)

	rustup := newBusyGuard("rustup", nil)
	require.NotNil(t, rustup)
	assert.Equal(t, []string{"rustup", "cargo", "rustc"}, rustup.processes)
}

func TestCargoGuard_BlocksOnlyWhileLockHeld(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	g := newBusyGuard("cargo", []string{filepath.Join(home, "registry"), filepath.Join(home, "git")})
	require.NotNil(t, g)

	t.Run("long-running cargo without the lock is not busy", func(t *testing.T) {
		g.listProcesses = func(context.Context) ([]osshim.Process, error) {
			t.Fatal("process list must not be consulted")
			return nil, nil
		}
		assert.Empty(t, g.busyReason(ctx))
	})

	t.Run("held package cache lock is busy", func(t *testing.T) {
		lock := filepath.Join(home, ".package-cache")
		release, acquired, err := osshim.TryLock(lock)
		require.NoError(t, err)
		require.True(t, acquired)
		defer release()

		assert.Equal(t, "lock held: "+lock, g.busyReason(ctx))
	})

	t.Run("released lock is free again", func(t *testing.T) {
		assert.Empty(t, g.busyReason(ctx))
	})

	t.Run("failed probe counts as busy", func(t *testing.T) {
		g.lockHeld = func(string) (bool, error) { return false, errors.New("boom") }
		assert.Contains(t, g.busyReason(ctx), "cannot check lock")
	})
}

func TestCommandProvider_CleanTimeout(t *testing.T) {
	p, err := NewCommandProvider("hang-test", config.Provider{
		Enabled:      true,
		Paths:        []string{t.TempDir()},
		MaxSize:      "1G",
		CleanCmd:     `sh -c "sleep 30"`,
		CleanTimeout: "1s",
	})
	require.NoError(t, err)

	start := time.Now()
	_, err = p.Clean(context.Background(), CleanOptions{})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out after 1s")
	assert.Less(t, elapsed, 15*time.Second, "hung command must be cancelled")
}

func TestCommandProvider_TimeoutKillsDescendants(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "heartbeat")
	// The grandchild appends to the file until killed, so a survivor shows up
	// as growth after the clean returned, whatever the machine's speed.
	script := "(while :; do echo x >> '" + filepath.ToSlash(beat) + "'; sleep 0.05; done) & wait"
	p, err := NewCommandProvider("group-test", config.Provider{
		Enabled:      true,
		Paths:        []string{t.TempDir()},
		MaxSize:      "1G",
		CleanCmd:     "sh -c \"" + script + "\"",
		CleanTimeout: "1s",
	})
	require.NoError(t, err)

	_, err = p.Clean(context.Background(), CleanOptions{})
	require.Error(t, err)

	if _, statErr := os.Stat(beat); statErr != nil {
		t.Skip("the descendant never started before the timeout")
	}
	time.Sleep(200 * time.Millisecond)
	settled := fileSize(t, beat)
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, settled, fileSize(t, beat), "grandchild must die with the timed-out command")
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

func TestCommandProvider_ParentCancelIsNotTimeout(t *testing.T) {
	p, err := NewCommandProvider("cancel-test", config.Provider{
		Enabled:  true,
		Paths:    []string{t.TempDir()},
		MaxSize:  "1G",
		CleanCmd: `sh -c "sleep 30"`,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	_, err = p.Clean(ctx, CleanOptions{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "timed out")
}

func TestCommandProvider_TimeoutConfig(t *testing.T) {
	cfg := config.Provider{Enabled: true, Paths: []string{t.TempDir()}, MaxSize: "1G", CleanCmd: "true"}

	p, err := NewCommandProvider("t", cfg)
	require.NoError(t, err)
	assert.Equal(t, DefaultCleanTimeout, p.timeout)

	cfg.CleanTimeout = "90s"
	p, err = NewCommandProvider("t", cfg)
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, p.timeout)

	for _, bad := range []string{"0", "abc", " "} {
		cfg.CleanTimeout = bad
		_, err = NewCommandProvider("t", cfg)
		assert.Error(t, err, bad)
	}
}

func TestGoGuard_AgesOutOnlyLongRunningRunAndTest(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name    string
		line    string
		started time.Time
		want    string
	}{
		{"day-old go run does not block", "go run mock.go", ago(24 * time.Hour), ""},
		{"day-old go test does not block", "/usr/local/go/bin/go test ./...", ago(19 * time.Hour), ""},
		{"day-old go run behind -C does not block", "go -C /src/app run ./cmd/x", ago(24 * time.Hour), ""},
		{"day-old go run behind -C= does not block", "go -C=/src/app run ./cmd/x", ago(24 * time.Hour), ""},
		{"old go run under sh -c does not block", "sh -c go run mock.go", ago(24 * time.Hour), ""},
		{"go run at the threshold does not block", "go run mock.go", ago(time.Hour), ""},
		{"fresh go run blocks", "go run mock.go", ago(5 * time.Minute), "go is running (pid 7, up 5m0s)"},
		{"go run just under the threshold blocks", "go run mock.go", ago(59 * time.Minute), "go is running (pid 7, up 59m0s)"},
		{"go build in progress blocks", "go build ./...", ago(2 * time.Minute), "go is running (pid 7, up 2m0s)"},
		{"day-old go build still blocks", "go build ./...", ago(24 * time.Hour), "go is running (pid 7, up 24h0m0s)"},
		{"go mod download still blocks", "go mod download", ago(24 * time.Hour), "go is running (pid 7, up 24h0m0s)"},
		{"go install still blocks", "go -C /src install ./cmd/x", ago(24 * time.Hour), "go is running (pid 7, up 24h0m0s)"},
		{"old go run chained with go build blocks", "sh -c go run a.go && go build ./...", ago(24 * time.Hour), "go is running (pid 7, up 24h0m0s)"},
		{"bare go blocks", "go", ago(24 * time.Hour), "go is running (pid 7, up 24h0m0s)"},
		{"go with a trailing -C blocks", "go -C", ago(24 * time.Hour), "go is running (pid 7, up 24h0m0s)"},
		{"unknown start time blocks", "go run mock.go", time.Time{}, "go is running (pid 7)"},
		{"start time in the future blocks", "go run mock.go", now.Add(time.Hour), "go is running (pid 7, up -1h0m0s)"},
	}
	for _, name := range []string{"go-build", "go-mod"} {
		for _, tt := range tests {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				g := newBusyGuard(name, []string{t.TempDir()})
				require.NotNil(t, g)
				g.now = func() time.Time { return now }
				g.listProcesses = func(context.Context) ([]osshim.Process, error) {
					return []osshim.Process{{PID: 7, CommandLine: tt.line, Args: tt.line, Started: tt.started}}, nil
				}
				assert.Equal(t, tt.want, g.busyReason(context.Background()))
			})
		}
	}
}

func TestGoGuard_OldRunDoesNotHideAFreshBuild(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	g := newBusyGuard("go-mod", []string{t.TempDir()})
	require.NotNil(t, g)
	g.now = func() time.Time { return now }
	g.listProcesses = func(context.Context) ([]osshim.Process, error) {
		return []osshim.Process{
			{PID: 1, CommandLine: "go run mock.go", Started: now.Add(-24 * time.Hour)},
			{PID: 2, CommandLine: "mise dev", Started: now.Add(-20 * time.Hour)},
			{PID: 3, CommandLine: "go build ./...", Started: now.Add(-30 * time.Second)},
		}, nil
	}
	assert.Equal(t, "go is running (pid 3, up 30s)", g.busyReason(context.Background()))
}

func TestOtherGuardsNeverAgeOut(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for name, line := range map[string]string{
		"homebrew": "brew install go",
		"uv":       "uv run script.py",
		"yarn":     "yarn dev",
	} {
		t.Run(name, func(t *testing.T) {
			g := newBusyGuard(name, []string{t.TempDir()})
			require.NotNil(t, g)
			assert.Nil(t, g.aging)
			g.now = func() time.Time { return now }
			g.lockHeld = func(string) (bool, error) { return false, nil }
			g.listProcesses = func(context.Context) ([]osshim.Process, error) {
				return []osshim.Process{{PID: 9, CommandLine: line, Started: now.Add(-48 * time.Hour)}}, nil
			}
			assert.Contains(t, g.busyReason(context.Background()), " is running (pid 9, up 48h0m0s)")
		})
	}
}

func TestGoGuard_WindowsImageNameAlwaysBlocks(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	assert.False(t, goBusyAging.agedOut(
		&osshim.Process{PID: 4, CommandLine: "go.exe", Started: now.Add(-48 * time.Hour)}, now),
		"a listing without arguments cannot tell run from build")
}

func TestToolSubcommands(t *testing.T) {
	tests := []struct {
		line    string
		windows bool
		want    []string
	}{
		{"go run x.go", false, []string{"run"}},
		{"/usr/local/go/bin/go -C dir test ./...", false, []string{"test"}},
		{"sudo -u me go build", false, []string{"build"}},
		{"'go run;", false, []string{"run"}},
		{"xargs go vet ./... && go run a.go", false, []string{"vet", "run"}},
		{"go", false, []string{""}},
		{"go;", false, []string{""}},
		{"vim main.go", false, nil},
		{`C:\Go\bin\go.exe run x.go`, true, []string{"run"}},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			assert.Equal(t, tt.want, toolSubcommands(tt.line, "go", tt.windows))
		})
	}
}

func TestCommandProvider_SkipReasonNamesBlockingProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	p, err := NewCommandProvider("go-mod", config.Provider{
		Enabled:  true,
		Paths:    []string{t.TempDir()},
		MaxSize:  "1G",
		CleanCmd: "touch " + marker,
	})
	require.NoError(t, err)
	p.busy.listProcesses = func(context.Context) ([]osshim.Process, error) {
		return []osshim.Process{{PID: 4242, CommandLine: "go build ./...", Started: time.Now().Add(-3 * time.Minute)}}, nil
	}

	result, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.Regexp(t, `^go is running \(pid 4242, up 3m\d+s\)$`, result.SkipReason)
	assert.NoFileExists(t, marker)
}

func TestCommandProvider_DayOldGoRunDoesNotBlockCleaning(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	p, err := NewCommandProvider("go-mod", config.Provider{
		Enabled:  true,
		Paths:    []string{t.TempDir()},
		MaxSize:  "1G",
		CleanCmd: "touch " + marker,
	})
	require.NoError(t, err)
	p.busy.listProcesses = func(context.Context) ([]osshim.Process, error) {
		return []osshim.Process{{PID: 99, CommandLine: "go run mock.go", Started: time.Now().Add(-24 * time.Hour)}}, nil
	}

	result, err := p.Clean(context.Background(), CleanOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.SkipReason)
}
