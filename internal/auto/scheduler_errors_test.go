package auto

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errExit = errors.New("exit status 1")

// blockPath makes path a non-empty directory so that renaming a file onto it
// or removing it fails on every OS.
func blockPath(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600))
}

// fileAt makes path a regular file so that creating anything below it fails.
func fileAt(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
}

func failing(match func(name string, args []string) bool) func(string, []string) ([]byte, error) {
	return func(name string, args []string) ([]byte, error) {
		if match(name, args) {
			return []byte("boom"), errExit
		}
		if name == "crontab" {
			return noCrontab(name, args)
		}
		return nil, nil
	}
}

func scheduled(s *scriptedExec, verbs ...string) bool {
	for _, c := range s.commands() {
		for _, v := range verbs {
			if strings.Contains(c, v) {
				return true
			}
		}
	}
	return false
}

func TestInstall_UnusableIntervalReachesNoScheduler(t *testing.T) {
	tests := map[string]struct {
		goos     string
		exe      string
		interval time.Duration
		respond  func(string, []string) ([]byte, error)
		verbs    []string
	}{
		"launchd":          {"darwin", posixExe, 0, nil, []string{"bootstrap", "bootout"}},
		"systemd":          {"linux", posixExe, 0, nil, []string{"daemon-reload", "enable", "restart"}},
		"cron":             {"linux", posixExe, 90 * time.Second, failing(func(_ string, a []string) bool { return len(a) > 1 && a[1] == "show-environment" }), []string{"crontab -l"}},
		"task scheduler":   {"windows", windowsExe, 90 * time.Second, nil, []string{"/Create"}},
		"cron line breaks": {"linux", "/usr/bin/bil\ngie", time.Hour, failing(func(_ string, a []string) bool { return len(a) > 1 && a[1] == "show-environment" }), []string{"crontab -l"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{respond: tt.respond}
			a := newOSAgent(t, tt.goos, tt.exe, s)
			a.Interval = tt.interval

			require.Error(t, a.Install(t.Context()))

			assert.False(t, scheduled(s, tt.verbs...), s.commands())
			assert.False(t, FirstRunPending(a.StateDir))
		})
	}
}

func TestInstall_UnwritableStateDirFailsBeforeAnyScheduler(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			s := &scriptedExec{}
			exe := posixExe
			if goos == "windows" {
				exe = windowsExe
			}
			a := newOSAgent(t, goos, exe, s)
			fileAt(t, a.StateDir)

			require.Error(t, a.Install(t.Context()))

			assert.Empty(t, s.calls)
		})
	}
}

func TestLaunchdInstall_FileSystemFailuresAreReturned(t *testing.T) {
	tests := map[string]func(t *testing.T, a Agent){
		"log dir": func(t *testing.T, a Agent) {
			t.Helper()
			fileAt(t, filepath.Join(a.Home, "Library", "Logs"))
		},
		"plist write": func(t *testing.T, a Agent) {
			t.Helper()
			blockPath(t, a.PlistPath())
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{}
			a := newOSAgent(t, "darwin", posixExe, s)
			setup(t, a)

			require.Error(t, a.Install(t.Context()))

			assert.False(t, scheduled(s, "bootstrap"))
			assert.False(t, FirstRunPending(a.StateDir))
		})
	}
}

func TestLaunchdInstall_GivesUpAfterBootstrapRetries(t *testing.T) {
	s := &scriptedExec{respond: failing(func(_ string, a []string) bool { return a[0] == "bootstrap" })}
	a := newOSAgent(t, "darwin", posixExe, s)

	err := a.Install(t.Context())

	require.ErrorContains(t, err, "launchctl bootstrap")
	assert.ErrorContains(t, err, "boom")
	attempts := 0
	for _, c := range s.commands() {
		if strings.Contains(c, "bootstrap") {
			attempts++
		}
	}
	assert.Equal(t, bootstrapAttempts, attempts)
}

func TestLaunchdInstall_CancelStopsTheBootstrapRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := &scriptedExec{}
	s.respond = func(_ string, args []string) ([]byte, error) {
		if args[0] == "bootstrap" {
			cancel()
			return nil, errExit
		}
		return nil, nil
	}
	a := newOSAgent(t, "darwin", posixExe, s)
	a.RetryDelay = time.Hour

	require.ErrorIs(t, a.Install(ctx), context.Canceled)
}

func TestLaunchdUninstall_Failures(t *testing.T) {
	t.Run("cancelled bootout keeps the plist", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		s := &scriptedExec{}
		a := newOSAgent(t, "darwin", posixExe, s)
		require.NoError(t, a.Install(t.Context()))
		s.respond = func(string, []string) ([]byte, error) {
			cancel()
			return []byte("boom"), errExit
		}

		require.ErrorContains(t, a.Uninstall(ctx), "launchctl bootout")

		assert.FileExists(t, a.PlistPath())
		assert.True(t, FirstRunPending(a.StateDir))
	})
	t.Run("print that cannot confirm absence keeps the plist", func(t *testing.T) {
		s := &scriptedExec{}
		a := newOSAgent(t, "darwin", posixExe, s)
		require.NoError(t, a.Install(t.Context()))
		s.respond = func(string, []string) ([]byte, error) { return []byte("Operation not permitted"), errExit }

		require.ErrorContains(t, a.Uninstall(t.Context()), "launchctl bootout")

		assert.FileExists(t, a.PlistPath())
	})
	t.Run("plist that cannot be removed", func(t *testing.T) {
		s := &scriptedExec{}
		a := newOSAgent(t, "darwin", posixExe, s)
		blockPath(t, a.PlistPath())

		require.ErrorContains(t, a.Uninstall(t.Context()), "remove plist")
	})
}

func TestExitCode_ReadsTheExitStatusOfAFailedCommand(t *testing.T) {
	err := exec.CommandContext(t.Context(), os.Args[0], "-test.no-such-flag").Run()
	require.Error(t, err)

	assert.Equal(t, 2, exitCode(err))
	assert.Zero(t, exitCode(errors.New("not an exit error")))
}

func TestSystemdInstall_UnitWriteFailuresAreReturned(t *testing.T) {
	tests := map[string]func(t *testing.T, a Agent) Agent{
		"unit dir": func(t *testing.T, a Agent) Agent {
			t.Helper()
			a.ConfigDir = filepath.Join(a.Home, "cfg")
			fileAt(t, a.ConfigDir)
			return a
		},
		"timer": func(t *testing.T, a Agent) Agent {
			t.Helper()
			blockPath(t, a.TimerPath())
			return a
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{}
			a := setup(t, newOSAgent(t, "linux", posixExe, s))

			require.Error(t, a.Install(t.Context()))

			assert.False(t, scheduled(s, "daemon-reload", "enable"))
			assert.False(t, FirstRunPending(a.StateDir))
		})
	}
}

func TestSystemdInstall_UnreadableCrontabIsOnlyAWarning(t *testing.T) {
	s := &scriptedExec{respond: func(name string, _ []string) ([]byte, error) {
		if name == "crontab" {
			return []byte("not allowed"), errExit
		}
		return nil, nil
	}}
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Install(t.Context()))

	assert.Contains(t, output(a), "warning: could not remove an earlier crontab entry")
	assert.FileExists(t, a.TimerPath())
}

func TestSystemdUninstall_FailuresKeepTheMarker(t *testing.T) {
	tests := map[string]struct {
		setup   func(t *testing.T, a Agent)
		respond func(string, []string) ([]byte, error)
		want    string
	}{
		"daemon-reload": {
			respond: failing(func(_ string, a []string) bool { return len(a) > 1 && a[1] == "daemon-reload" }),
			want:    "systemctl daemon-reload",
		},
		"timer removal": {
			setup: func(t *testing.T, a Agent) {
				t.Helper()
				require.NoError(t, os.Remove(a.TimerPath()))
				blockPath(t, a.TimerPath())
			},
			want: "remove timer",
		},
		"service removal": {
			setup: func(t *testing.T, a Agent) {
				t.Helper()
				require.NoError(t, os.Remove(a.ServicePath()))
				blockPath(t, a.ServicePath())
			},
			want: "remove service",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{respond: noCrontab}
			a := newOSAgent(t, "linux", posixExe, s)
			require.NoError(t, a.Install(t.Context()))
			if tt.setup != nil {
				tt.setup(t, a)
			}
			if tt.respond != nil {
				s.respond = tt.respond
			}

			require.ErrorContains(t, a.Uninstall(t.Context()), tt.want)

			assert.True(t, FirstRunPending(a.StateDir))
		})
	}
}

func TestCronInstall_StepFailuresAreReturned(t *testing.T) {
	noManager := func(_ string, a []string) bool { return len(a) > 1 && a[1] == "show-environment" }
	tests := map[string]struct {
		fail func(name string, args []string) bool
		want string
		prep func(t *testing.T, a Agent)
	}{
		"crontab unreadable": {
			fail: func(name string, args []string) bool {
				return noManager(name, args) || (name == "crontab" && args[0] == "-l")
			},
			want: "crontab -l",
		},
		"crontab rejects the file": {
			fail: func(name string, args []string) bool {
				return noManager(name, args) || (name == "crontab" && args[0] != "-l")
			},
			want: "crontab install",
		},
		"staging file cannot be written": {
			fail: noManager,
			prep: func(t *testing.T, a Agent) {
				t.Helper()
				blockPath(t, filepath.Join(a.StateDir, "crontab.new"))
			},
			want: "crontab.new",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{respond: failing(tt.fail)}
			a := newOSAgent(t, "linux", posixExe, s)
			a.Interval = time.Hour
			if tt.prep != nil {
				tt.prep(t, a)
			}

			require.ErrorContains(t, a.Install(t.Context()), tt.want)

			assert.False(t, FirstRunPending(a.StateDir))
		})
	}
}

func TestCronUninstall_StepFailuresAreReturned(t *testing.T) {
	line, err := RenderCronLine(posixExe, "/home/u", "/log", time.Hour)
	require.NoError(t, err)
	tests := map[string]struct {
		crontab string
		fail    func(name string, args []string) bool
		want    string
	}{
		"remove the last entry": {
			crontab: line,
			fail:    func(name string, args []string) bool { return name == "crontab" && args[0] == "-r" },
			want:    "crontab -r",
		},
		"rewrite the remaining entries": {
			crontab: "0 1 * * * other\n" + line,
			fail:    func(name string, args []string) bool { return name == "crontab" && args[0] != "-l" },
			want:    "crontab install",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{respond: func(n string, args []string) ([]byte, error) {
				if tt.fail(n, args) {
					return []byte("boom"), errExit
				}
				if n == "crontab" && args[0] == "-l" {
					return []byte(tt.crontab), nil
				}
				return nil, errExit
			}}
			a := newOSAgent(t, "linux", posixExe, s)

			_, err := a.uninstallCron(t.Context())

			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestTaskInstall_DefinitionWriteFailureIsReturned(t *testing.T) {
	s := &scriptedExec{}
	a := newOSAgent(t, "windows", windowsExe, s)
	blockPath(t, a.TaskXMLPath())

	require.Error(t, a.Install(t.Context()))

	assert.False(t, scheduled(s, "/Create"))
	assert.False(t, FirstRunPending(a.StateDir))
}

func TestTaskUninstall_Failures(t *testing.T) {
	t.Run("cancelled delete is not confirmed by a listing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		s := &scriptedExec{respond: func(string, []string) ([]byte, error) {
			cancel()
			return []byte("boom"), errExit
		}}
		a := newOSAgent(t, "windows", windowsExe, s)

		require.ErrorContains(t, a.Uninstall(ctx), "schtasks delete")

		assert.Len(t, s.calls, 1)
	})
	t.Run("definition that cannot be removed", func(t *testing.T) {
		s := &scriptedExec{}
		a := newOSAgent(t, "windows", windowsExe, s)
		blockPath(t, a.TaskXMLPath())

		require.ErrorContains(t, a.Uninstall(t.Context()), "remove task definition")
	})
}

func TestUninstall_StateFileThatCannotBeRemovedIsReported(t *testing.T) {
	s := &scriptedExec{respond: noCrontab}
	a := newOSAgent(t, "linux", posixExe, s)
	require.NoError(t, a.Install(t.Context()))
	blockPath(t, filepath.Join(a.StateDir, TickStateName))

	require.ErrorContains(t, a.Uninstall(t.Context()), "remove "+TickStateName)
}

func TestWriteFileAtomic_Failures(t *testing.T) {
	t.Run("parent is a file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "parent")
		fileAt(t, parent)

		require.ErrorContains(t, writeFileAtomic(filepath.Join(parent, "f"), []byte("x")), "create dir")
	})
	t.Run("target is a directory", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "f")
		blockPath(t, target)

		require.ErrorContains(t, writeFileAtomic(target, []byte("x")), "install f")
		entries, err := os.ReadDir(filepath.Dir(target))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "temp file is cleaned up")
	})
}

func TestExecCommand_MissingBinaryIsRecognised(t *testing.T) {
	out, err := ExecCommand(t.Context(), "go", "version")
	require.NoError(t, err)
	assert.Contains(t, string(out), "go version")

	_, err = ExecCommand(t.Context(), "bilgie-no-such-binary")
	require.Error(t, err)
	assert.True(t, binaryMissing(err))
}
