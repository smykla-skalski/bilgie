package auto

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	posixExe   = "/usr/local/bin/bilgie"
	windowsExe = `C:\Users\me\bin\bilgie.exe`
	windowsLog = `C:\Users\me\.local\state\bilgie\auto.log`
)

func TestBackendNamesShareOneStem(t *testing.T) {
	assert.Equal(t, "dev.mskalski."+agentName, AgentLabel)
	assert.Equal(t, agentName, SystemdUnit)
	assert.Equal(t, agentName, CronTag)
	assert.Equal(t, agentName, TaskName)
}

func TestIsAbsPath_FollowsTargetOS(t *testing.T) {
	tests := []struct {
		goos, path string
		want       bool
	}{
		{"linux", "/usr/bin/x", true},
		{"darwin", "/opt/homebrew/bin/x", true},
		{"linux", "bilgie", false},
		{"linux", `C:\x\y.exe`, false},
		{"windows", `C:\x\y.exe`, true},
		{"windows", `c:/x/y.exe`, true},
		{"windows", `\\server\share\y.exe`, true},
		{"windows", "/usr/bin/x", false},
		{"windows", `y.exe`, false},
		{"windows", `1:\y.exe`, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isAbsPath(tt.goos, tt.path), tt.goos+" "+tt.path)
	}
}

func TestInstall_ArmsFirstRunDryRunOnEveryOS(t *testing.T) {
	for goos, exe := range map[string]string{"darwin": posixExe, "linux": posixExe, "windows": windowsExe} {
		t.Run(goos, func(t *testing.T) {
			a := newOSAgent(t, goos, exe, &scriptedExec{})

			require.NoError(t, a.Install(t.Context()))
			assert.True(t, FirstRunPending(a.StateDir))
			assert.Contains(t, output(a), "the first run is a dry-run")

			require.NoError(t, a.Uninstall(t.Context()))
			assert.False(t, FirstRunPending(a.StateDir))
		})
	}
}

func TestInstall_RejectsBadBinaryOnEveryOS(t *testing.T) {
	for goos, exes := range map[string][]string{
		"darwin":  {"bilgie", "/var/x/go-build1/b001/exe/bilgie", windowsExe},
		"linux":   {"bilgie", "/var/x/go-build1/b001/exe/bilgie", windowsExe},
		"windows": {"bilgie.exe", `C:\x\go-build1\b001\exe\bilgie.exe`, posixExe},
	} {
		for _, exe := range exes {
			s := &scriptedExec{}
			a := newOSAgent(t, goos, exe, s)

			require.Error(t, a.Install(t.Context()), goos+" "+exe)
			assert.Empty(t, s.calls)
			assert.False(t, FirstRunPending(a.StateDir))
		}
	}
}

func TestInstall_RejectsUnsupportedOS(t *testing.T) {
	a := newOSAgent(t, "plan9", posixExe, &scriptedExec{})

	require.ErrorContains(t, a.Install(t.Context()), "not supported on plan9")
	require.ErrorContains(t, a.Uninstall(t.Context()), "not supported on plan9")
}

func TestRenderPlist_Golden(t *testing.T) {
	data, err := RenderPlist("/opt/homebrew/bin/bilgie", "/Users/me", "/Users/me/Library/Logs/bilgie/auto.log", 45*time.Minute)
	require.NoError(t, err)

	assertGolden(t, "launchd.plist", string(data))
}

func TestRenderSystemd_Golden(t *testing.T) {
	service, err := RenderSystemdService(posixExe, "/home/me", "/home/me/.local/state/bilgie/auto.log")
	require.NoError(t, err)
	timer, err := RenderSystemdTimer(45 * time.Minute)
	require.NoError(t, err)

	assertGolden(t, "systemd.service", string(service))
	assertGolden(t, "systemd.timer", string(timer))
}

func TestRenderSystemd_EscapesSpecialCharacters(t *testing.T) {
	service, err := RenderSystemdService(`/opt/my "tools"/100%/$x/bilgie`, "/home/me", "/home/me/100%/auto.log")
	require.NoError(t, err)

	text := string(service)
	assert.Contains(t, text, `ExecStart="/opt/my \"tools\"/100%%/$$x/bilgie" tick`)
	assert.Contains(t, text, "StandardOutput=append:/home/me/100%%/auto.log")
}

func TestRenderSystemd_RejectsBadInput(t *testing.T) {
	_, err := RenderSystemdTimer(time.Millisecond)
	require.Error(t, err)
	_, err = RenderSystemdService("/bin/x\ny", "/h", "/l")
	require.Error(t, err)
}

func TestRenderCronLine_Golden(t *testing.T) {
	line, err := RenderCronLine(posixExe, "/home/me", "/home/me/.local/state/bilgie/auto.log", 30*time.Minute)
	require.NoError(t, err)

	assertGolden(t, "cron.line", line)
}

func TestRenderCronLine_QuotesAndSchedules(t *testing.T) {
	tests := []struct {
		interval time.Duration
		want     string
	}{
		{time.Minute, "*/1 * * * * "},
		{30 * time.Minute, "*/30 * * * * "},
		{time.Hour, "0 */1 * * * "},
		{6 * time.Hour, "0 */6 * * * "},
		{24 * time.Hour, "0 0 * * * "},
	}
	for _, tt := range tests {
		line, err := RenderCronLine(posixExe, "/home/me", "/log", tt.interval)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(line, tt.want), "%s: %s", tt.interval, line)
	}

	line, err := RenderCronLine("/opt/it's 100%/bilgie", "/home/me", "/log", time.Hour)
	require.NoError(t, err)
	assert.Contains(t, line, `'/opt/it'\''s 100\%/bilgie'`)
}

func TestRenderCronLine_RejectsIntervalsCronCannotRepeatEvenly(t *testing.T) {
	for _, interval := range []time.Duration{
		30 * time.Second, 90 * time.Second, 45 * time.Minute, 7 * time.Minute,
		90 * time.Minute, 5 * time.Hour, 48 * time.Hour,
	} {
		_, err := RenderCronLine(posixExe, "/h", "/l", interval)
		require.Error(t, err, interval.String())
	}
}

func TestRenderTaskXML_Golden(t *testing.T) {
	start := time.Date(2026, 10, 8, 12, 1, 0, 0, time.UTC)

	plain, err := RenderTaskXML(windowsExe, windowsLog, 45*time.Minute, start)
	require.NoError(t, err)
	special, err := RenderTaskXML(`C:\Program Files\a&b\bilgie.exe`, windowsLog, 90*time.Minute, start)
	require.NoError(t, err)

	assertGolden(t, "task.xml", plain)
	assertGolden(t, "task_special.xml", special)
}

func TestRenderTaskXML_RejectsSubMinuteInterval(t *testing.T) {
	_, err := RenderTaskXML(windowsExe, windowsLog, 30*time.Second, time.Now())
	require.Error(t, err)
}

func TestEncodeTaskXML_IsUTF16WithBOM(t *testing.T) {
	got := EncodeTaskXML("a€")

	assert.Equal(t, []byte{0xFF, 0xFE, 'a', 0, 0xAC, 0x20}, got)
}

func TestSystemdUnits_PassSystemdAnalyzeVerify(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd-analyze only exists on Linux")
	}
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not available")
	}
	self, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	service, err := RenderSystemdService(self, dir, filepath.Join(dir, "auto.log"))
	require.NoError(t, err)
	timer, err := RenderSystemdTimer(45 * time.Minute)
	require.NoError(t, err)
	servicePath := filepath.Join(dir, SystemdUnit+".service")
	timerPath := filepath.Join(dir, SystemdUnit+".timer")
	require.NoError(t, os.WriteFile(servicePath, service, 0o600))
	require.NoError(t, os.WriteFile(timerPath, timer, 0o600))

	out, err := exec.CommandContext(t.Context(), analyze, "--user", "verify", servicePath, timerPath).CombinedOutput()

	require.NoError(t, err, string(out))
}

func TestSystemdInstall_WritesUnitsAndEnablesTimer(t *testing.T) {
	s := &scriptedExec{}
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Install(t.Context()))

	assert.Equal(t, []string{
		"systemctl --user show-environment",
		"systemctl --user daemon-reload",
		"systemctl --user enable bilgie.timer",
		"systemctl --user restart bilgie.timer",
		"loginctl show-user 501 --property=Linger --value",
		"crontab -l",
	}, s.commands())
	assert.Equal(t, filepath.Join(a.Home, ".config", "systemd", "user", "bilgie.timer"), a.TimerPath())
	service, err := os.ReadFile(a.ServicePath())
	require.NoError(t, err)
	assert.Contains(t, string(service), `ExecStart="`+posixExe+`" tick`)
	timer, err := os.ReadFile(a.TimerPath())
	require.NoError(t, err)
	assert.Contains(t, string(timer), "OnUnitInactiveSec=2700s")
	assert.True(t, FirstRunPending(a.StateDir))
}

func TestSystemdInstall_StepFailureIsReturned(t *testing.T) {
	for _, step := range []string{"daemon-reload", "enable", "restart"} {
		s := &scriptedExec{respond: func(name string, args []string) ([]byte, error) {
			if name == "systemctl" && args[1] == step {
				return []byte("Failed to connect to bus"), errors.New("exit status 1")
			}
			return nil, nil
		}}
		a := newOSAgent(t, "linux", posixExe, s)

		err := a.Install(t.Context())

		require.ErrorContains(t, err, "systemctl "+step)
		assert.ErrorContains(t, err, "Failed to connect to bus")
	}
}

func TestSystemdInstall_ReplacesExistingUnits(t *testing.T) {
	a := newOSAgent(t, "linux", posixExe, &scriptedExec{})
	require.NoError(t, a.Install(t.Context()))
	a.Interval = time.Hour

	require.NoError(t, a.Install(t.Context()))

	timer, err := os.ReadFile(a.TimerPath())
	require.NoError(t, err)
	assert.Contains(t, string(timer), "OnUnitInactiveSec=3600s")
	assert.NotContains(t, string(timer), "2700s")
}

func noCrontab(name string, args []string) ([]byte, error) {
	if name == "crontab" && len(args) == 1 && args[0] == "-l" {
		return []byte("no crontab for me"), errors.New("exit status 1")
	}
	return nil, nil
}

func TestSystemdUninstall_RemovesUnitsAndMarker(t *testing.T) {
	s := &scriptedExec{respond: noCrontab}
	a := newOSAgent(t, "linux", posixExe, s)
	require.NoError(t, a.Install(t.Context()))
	s.reset()

	require.NoError(t, a.Uninstall(t.Context()))

	assert.NoFileExists(t, a.ServicePath())
	assert.NoFileExists(t, a.TimerPath())
	assert.False(t, FirstRunPending(a.StateDir))
	assert.Equal(t, []string{
		"systemctl --user disable --now bilgie.timer",
		"systemctl --user daemon-reload",
		"crontab -l",
	}, s.commands())
	assert.NotContains(t, output(a), "not installed")
}

func TestSystemdUninstall_NotInstalledIsNotAnError(t *testing.T) {
	s := &scriptedExec{respond: func(name string, args []string) ([]byte, error) {
		switch {
		case name == "systemctl" && args[1] == "disable":
			return []byte("Failed to disable unit: Unit file bilgie.timer does not exist."), errors.New("exit status 1")
		case name == "systemctl" && args[1] == "show":
			return []byte("not-found\n"), nil
		}
		return noCrontab(name, args)
	}}
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Contains(t, output(a), "agent was not installed")
	assert.Equal(t, []string{
		"systemctl --user disable --now bilgie.timer",
		"systemctl --user show --property=LoadState --value bilgie.timer",
		"crontab -l",
	}, s.commands())
}

func TestSystemdUninstall_FailureNotConfirmedAbsentKeepsState(t *testing.T) {
	tests := map[string]func() ([]byte, error){
		"still loaded":   func() ([]byte, error) { return []byte("loaded\n"), nil },
		"show fails too": func() ([]byte, error) { return nil, errors.New("bus unreachable") },
	}
	for name, show := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{}
			a := newOSAgent(t, "linux", posixExe, s)
			require.NoError(t, a.Install(t.Context()))
			s.respond = func(name string, args []string) ([]byte, error) {
				if name == "crontab" {
					return noCrontab(name, args)
				}
				switch args[1] {
				case "disable":
					return []byte("Access denied"), errors.New("exit status 1")
				case "show":
					return show()
				}
				return nil, nil
			}

			err := a.Uninstall(t.Context())

			require.ErrorContains(t, err, "systemctl disable")
			assert.FileExists(t, a.TimerPath())
			assert.FileExists(t, a.ServicePath())
			assert.True(t, FirstRunPending(a.StateDir))
		})
	}
}

func TestLinuxUninstall_ToleratesMissingSystemctlAndCrontab(t *testing.T) {
	s := &scriptedExec{respond: func(name string, _ []string) ([]byte, error) {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}}
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Contains(t, output(a), "agent was not installed")
}

func TestLinuxInstall_FallsBackToCronWithoutUserManager(t *testing.T) {
	var loaded string
	s := &scriptedExec{}
	s.respond = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "systemctl":
			return []byte("Failed to connect to bus"), errors.New("exit status 1")
		case name == "crontab" && args[0] == "-l":
			return []byte("MAILTO=me@example.com\n0 3 * * * backup.sh\n"), nil
		case name == "crontab":
			data, err := os.ReadFile(args[0])
			loaded = string(data)
			return nil, err
		}
		return nil, nil
	}
	a := newOSAgent(t, "linux", posixExe, s)
	a.Interval = 30 * time.Minute

	require.NoError(t, a.Install(t.Context()))

	last := s.calls[len(s.calls)-1]
	assert.Equal(t, "crontab", last[0])
	assert.NotEqual(t, "-l", last[1])
	assert.True(t, strings.HasPrefix(loaded, "MAILTO=me@example.com\n0 3 * * * backup.sh\n*/30 * * * * env PATH="), loaded)
	assert.True(t, strings.HasSuffix(loaded, " # bilgie\n"), loaded)
	assert.NoFileExists(t, filepath.Join(a.StateDir, "crontab.new"))
	assert.NoFileExists(t, a.TimerPath())
	assert.True(t, FirstRunPending(a.StateDir))
}

func TestCronInstall_ReplacesItsOwnEntryAndKeepsOthers(t *testing.T) {
	var loaded string
	s := &scriptedExec{}
	s.respond = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "systemctl":
			return nil, errors.New("exit status 1")
		case name == "crontab" && args[0] == "-l":
			return []byte("0 3 * * * backup.sh\n*/10 * * * * old auto # bilgie\n"), nil
		case name == "crontab":
			data, err := os.ReadFile(args[0])
			loaded = string(data)
			return nil, err
		}
		return nil, nil
	}
	a := newOSAgent(t, "linux", posixExe, s)
	a.Interval = 30 * time.Minute

	require.NoError(t, a.Install(t.Context()))

	assert.Equal(t, 1, strings.Count(loaded, "# bilgie"), loaded)
	assert.Contains(t, loaded, "0 3 * * * backup.sh\n")
	assert.NotContains(t, loaded, "old auto")
}

func TestCronInstall_ErrorsWithoutAnyScheduler(t *testing.T) {
	s := &scriptedExec{respond: func(name string, _ []string) ([]byte, error) {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}}
	a := newOSAgent(t, "linux", posixExe, s)
	a.Interval = 30 * time.Minute

	require.ErrorContains(t, a.Install(t.Context()), "cannot schedule")
}

func TestCronInstall_RejectsSubMinuteInterval(t *testing.T) {
	s := &scriptedExec{respond: func(name string, _ []string) ([]byte, error) {
		if name == "systemctl" {
			return nil, errors.New("exit status 1")
		}
		return noCrontab(name, nil)
	}}
	a := newOSAgent(t, "linux", posixExe, s)
	a.Interval = 20 * time.Second

	require.ErrorContains(t, a.Install(t.Context()), "whole number of minutes")
}

func cronUninstallExec(crontab string, loaded *string) *scriptedExec {
	return &scriptedExec{respond: func(name string, args []string) ([]byte, error) {
		switch {
		case name == "systemctl" && args[1] == "show":
			return []byte("not-found\n"), nil
		case name == "systemctl":
			return []byte("not loaded"), errors.New("exit status 1")
		case name == "crontab" && args[0] == "-l":
			return []byte(crontab), nil
		case name == "crontab" && args[0] != "-r":
			data, err := os.ReadFile(args[0])
			*loaded = string(data)
			return nil, err
		}
		return nil, nil
	}}
}

func TestCronUninstall_KeepsOtherEntries(t *testing.T) {
	var loaded string
	s := cronUninstallExec("0 3 * * * backup.sh\n*/45 * * * * x auto # bilgie\n", &loaded)
	a := newOSAgent(t, "linux", posixExe, s)
	require.NoError(t, MarkFirstRunPending(a.StateDir))

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Equal(t, "0 3 * * * backup.sh\n", loaded)
	assert.NotContains(t, output(a), "not installed")
	assert.False(t, FirstRunPending(a.StateDir))
}

func TestCronUninstall_RemovesWholeCrontabWhenOnlyOurs(t *testing.T) {
	var loaded string
	s := cronUninstallExec("*/45 * * * * x auto # bilgie\n", &loaded)
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Contains(t, s.commands(), "crontab -r")
	assert.Empty(t, loaded)
}

func TestCronUninstall_NoEntryLeavesCrontabAlone(t *testing.T) {
	var loaded string
	s := cronUninstallExec("0 3 * * * backup.sh\n", &loaded)
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Equal(t, []string{
		"systemctl --user disable --now bilgie.timer",
		"systemctl --user show --property=LoadState --value bilgie.timer",
		"crontab -l",
	}, s.commands())
	assert.Contains(t, output(a), "agent was not installed")
}

func TestLinuxUninstall_UnreadableCrontabIsAWarning(t *testing.T) {
	s := &scriptedExec{respond: func(name string, args []string) ([]byte, error) {
		if name == "crontab" {
			return []byte("crontab: you are not allowed to use this program"), errors.New("exit status 1")
		}
		if args[1] == "show" {
			return []byte("not-found\n"), nil
		}
		return nil, nil
	}}
	a := newOSAgent(t, "linux", posixExe, s)
	require.NoError(t, MarkFirstRunPending(a.StateDir))

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Contains(t, output(a), "warning: could not check the crontab")
	assert.False(t, FirstRunPending(a.StateDir))
}

func TestCronInstall_RemovesSystemdUnitsOfAnEarlierInstall(t *testing.T) {
	s := &scriptedExec{}
	a := newOSAgent(t, "linux", posixExe, s)
	a.Interval = 30 * time.Minute
	require.NoError(t, a.Install(t.Context()))
	s.respond = func(name string, _ []string) ([]byte, error) {
		if name == "systemctl" {
			return nil, errors.New("exit status 1")
		}
		return noCrontab(name, nil)
	}

	require.NoError(t, a.Install(t.Context()))

	assert.NoFileExists(t, a.TimerPath())
	assert.NoFileExists(t, a.ServicePath())
}

func TestInstall_FailedInstallDoesNotArmTheMarker(t *testing.T) {
	s := &scriptedExec{respond: func(name string, _ []string) ([]byte, error) {
		if name == "systemctl" {
			return nil, errors.New("exit status 1")
		}
		return noCrontab(name, nil)
	}}
	a := newOSAgent(t, "linux", posixExe, s)
	a.Interval = 45 * time.Minute

	require.Error(t, a.Install(t.Context()))

	assert.False(t, FirstRunPending(a.StateDir))
}

func TestInstall_FailedReinstallKeepsAnArmedMarker(t *testing.T) {
	a := newOSAgent(t, "linux", posixExe, &scriptedExec{})
	require.NoError(t, a.Install(t.Context()))
	a.Exec = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("exit status 1") }
	a.Interval = 45 * time.Minute

	require.Error(t, a.Install(t.Context()))

	assert.True(t, FirstRunPending(a.StateDir))
}

func TestTaskInstall_WritesDefinitionAndCreatesTask(t *testing.T) {
	s := &scriptedExec{}
	a := newOSAgent(t, "windows", windowsExe, s)

	require.NoError(t, a.Install(t.Context()))

	assert.Equal(t, []string{
		"schtasks /Create /TN bilgie /XML " + a.TaskXMLPath() + " /F",
	}, s.commands())
	raw, err := os.ReadFile(a.TaskXMLPath())
	require.NoError(t, err)
	assert.Equal(t, EncodeTaskXML(mustTaskXML(t, a)), raw)
	assert.True(t, FirstRunPending(a.StateDir))
}

func mustTaskXML(t *testing.T, a Agent) string {
	t.Helper()
	xmlText, err := RenderTaskXML(a.Exe, a.logPath(), a.Interval, a.now().Add(time.Minute))
	require.NoError(t, err)
	return xmlText
}

func TestTaskInstall_CreateFailureIsReturned(t *testing.T) {
	s := &scriptedExec{respond: func(_ string, args []string) ([]byte, error) {
		if args[0] != "/Create" {
			return nil, nil
		}
		return []byte("ERROR: Access is denied."), errors.New("exit status 1")
	}}
	a := newOSAgent(t, "windows", windowsExe, s)

	err := a.Install(t.Context())

	require.ErrorContains(t, err, "schtasks create")
	assert.ErrorContains(t, err, "Access is denied")
}

func TestTaskUninstall_DeletesTaskAndDefinition(t *testing.T) {
	s := &scriptedExec{}
	a := newOSAgent(t, "windows", windowsExe, s)
	require.NoError(t, a.Install(t.Context()))
	s.reset()

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Equal(t, []string{
		"schtasks /Delete /TN bilgie /F",
	}, s.commands())
	assert.NoFileExists(t, a.TaskXMLPath())
	assert.False(t, FirstRunPending(a.StateDir))
	assert.NotContains(t, output(a), "not installed")
}

func TestTaskUninstall_NotInstalledIsConfirmedByListing(t *testing.T) {
	s := &scriptedExec{respond: func(_ string, args []string) ([]byte, error) {
		if args[0] == "/Delete" {
			return []byte("FEHLER: Das System kann die angegebene Datei nicht finden."), errors.New("exit status 1")
		}
		return []byte("\"\\Other\",\"N/A\",\"Ready\"\r\n"), nil
	}}
	a := newOSAgent(t, "windows", windowsExe, s)

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Contains(t, output(a), "agent was not installed")
	assert.Equal(t, []string{
		"schtasks /Delete /TN bilgie /F",
		"schtasks /Query /FO CSV /NH",
	}, s.commands())
}

func TestTaskUninstall_DeleteFailureNotConfirmedAbsentKeepsState(t *testing.T) {
	tests := map[string]func() ([]byte, error){
		"task still listed": func() ([]byte, error) { return []byte("\"\\bilgie\",\"N/A\",\"Ready\"\r\n"), nil },
		"listing fails":     func() ([]byte, error) { return nil, errors.New("exit status 1") },
	}
	for name, list := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{}
			a := newOSAgent(t, "windows", windowsExe, s)
			require.NoError(t, a.Install(t.Context()))
			s.respond = func(_ string, args []string) ([]byte, error) {
				if args[0] == "/Delete" {
					return []byte("ERROR: Access is denied."), errors.New("exit status 1")
				}
				return list()
			}

			err := a.Uninstall(t.Context())

			require.ErrorContains(t, err, "schtasks delete")
			assert.FileExists(t, a.TaskXMLPath())
			assert.True(t, FirstRunPending(a.StateDir))
		})
	}
}

func TestRenderSystemd_EnvironmentKeepsDollarLiteral(t *testing.T) {
	service, err := RenderSystemdService(posixExe, "/home/a$b", "/log")
	require.NoError(t, err)

	assert.Contains(t, string(service), "/home/a$b/go/bin")
	assert.NotContains(t, string(service), "$$b")
}

func TestLinuxUninstall_CronFallbackWorksWithoutUserManager(t *testing.T) {
	var loaded string
	s := &scriptedExec{}
	s.respond = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "systemctl":
			return []byte("Failed to connect to bus"), errors.New("exit status 1")
		case name == "crontab" && args[0] == "-l":
			return []byte("0 3 * * * backup.sh\n*/30 * * * * x auto # bilgie\n"), nil
		case name == "crontab":
			data, err := os.ReadFile(args[0])
			loaded = string(data)
			return nil, err
		}
		return nil, nil
	}
	a := newOSAgent(t, "linux", posixExe, s)
	require.NoError(t, MarkFirstRunPending(a.StateDir))

	require.NoError(t, a.Uninstall(t.Context()))

	assert.Equal(t, "0 3 * * * backup.sh\n", loaded)
	assert.False(t, FirstRunPending(a.StateDir))
}

func TestLinuxUninstall_SystemdFailureStillCleansCron(t *testing.T) {
	var loaded string
	s := &scriptedExec{}
	a := newOSAgent(t, "linux", posixExe, s)
	require.NoError(t, a.Install(t.Context()))
	s.respond = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "systemctl":
			return []byte("Access denied"), errors.New("exit status 1")
		case name == "crontab" && args[0] == "-l":
			return []byte("*/30 * * * * x auto # bilgie\n0 3 * * * backup.sh\n"), nil
		case name == "crontab":
			data, err := os.ReadFile(args[0])
			loaded = string(data)
			return nil, err
		}
		return nil, nil
	}

	require.ErrorContains(t, a.Uninstall(t.Context()), "systemctl disable")

	assert.Equal(t, "0 3 * * * backup.sh\n", loaded)
	assert.True(t, FirstRunPending(a.StateDir), "the timer is still registered, so the marker stays")
}

func TestSystemdInstall_RemovesAnEarlierCronEntry(t *testing.T) {
	var loaded string
	s := &scriptedExec{}
	s.respond = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "crontab" && args[0] == "-l":
			return []byte("*/30 * * * * x auto # bilgie\n0 3 * * * backup.sh\n"), nil
		case name == "crontab":
			data, err := os.ReadFile(args[0])
			loaded = string(data)
			return nil, err
		}
		return nil, nil
	}
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Install(t.Context()))

	assert.Equal(t, "0 3 * * * backup.sh\n", loaded)
}

func TestSystemdUnits_FollowTheConfigDir(t *testing.T) {
	elsewhere := filepath.Join(t.TempDir(), "custom-cfg")
	tests := map[string]string{"set elsewhere": elsewhere, "unset": ""}
	for name, configDir := range tests {
		t.Run(name, func(t *testing.T) {
			s := &scriptedExec{respond: noCrontab}
			a := newOSAgent(t, "linux", posixExe, s)
			a.ConfigDir = configDir
			wantDir := configDir
			if wantDir == "" {
				wantDir = filepath.Join(a.Home, ".config")
			}
			wantTimer := filepath.Join(wantDir, "systemd", "user", "bilgie.timer")

			require.NoError(t, a.Install(t.Context()))
			assert.Equal(t, wantTimer, a.TimerPath())
			assert.FileExists(t, wantTimer)
			assert.FileExists(t, filepath.Join(wantDir, "systemd", "user", "bilgie.service"))
			if configDir != "" {
				assert.NoDirExists(t, filepath.Join(a.Home, ".config"))
			}

			require.NoError(t, a.Uninstall(t.Context()))
			assert.NoFileExists(t, wantTimer)
			assert.NoFileExists(t, a.ServicePath())
		})
	}
}

func TestSystemdUninstall_AbsenceIsConfirmedForTheConfigDirTimer(t *testing.T) {
	s := &scriptedExec{}
	a := newOSAgent(t, "linux", posixExe, s)
	a.ConfigDir = filepath.Join(a.Home, "xdg")
	require.NoError(t, a.Install(t.Context()))
	s.respond = func(name string, args []string) ([]byte, error) {
		if name == "crontab" {
			return noCrontab(name, args)
		}
		if args[1] == "disable" {
			return []byte("exit 1"), errors.New("exit status 1")
		}
		return nil, errors.New("bus unreachable")
	}

	require.ErrorContains(t, a.Uninstall(t.Context()), "systemctl disable")

	assert.FileExists(t, a.TimerPath(), "unit files in the config dir count as installed")
}
