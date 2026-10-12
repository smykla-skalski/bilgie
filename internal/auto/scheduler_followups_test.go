package auto

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurrentCrontab_BusyBoxMissingFileIsAnEmptyCrontab(t *testing.T) {
	s := &scriptedExec{respond: func(string, []string) ([]byte, error) {
		return []byte("crontab: can't open 'root': No such file or directory"), errors.New("exit status 1")
	}}
	a := newOSAgent(t, "linux", posixExe, s)

	got, err := a.currentCrontab(t.Context())

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCurrentCrontab_OtherMissingFileFailuresStayErrors(t *testing.T) {
	s := &scriptedExec{respond: func(string, []string) ([]byte, error) {
		return []byte("crontab: /var/spool/cron/crontabs: No such file or directory"), errors.New("exit status 1")
	}}
	a := newOSAgent(t, "linux", posixExe, s)

	_, err := a.currentCrontab(t.Context())

	require.Error(t, err)
}

func TestCurrentCrontab_OtherFailuresStayErrors(t *testing.T) {
	s := &scriptedExec{respond: func(string, []string) ([]byte, error) {
		return []byte("crontab: you are not allowed to use this program"), errors.New("exit status 1")
	}}
	a := newOSAgent(t, "linux", posixExe, s)

	_, err := a.currentCrontab(t.Context())

	require.Error(t, err)
}

func TestRenderTaskXML_RedirectsOutputToTheLog(t *testing.T) {
	got, err := RenderTaskXML(windowsExe, windowsLog, 45*time.Minute, time.Now())

	require.NoError(t, err)
	assert.Contains(t, got, "<Command>cmd.exe</Command>")
	assert.Contains(t, got, `<Arguments>/d /s /c &#34;&#34;`+windowsExe+`&#34; tick &gt;&gt; &#34;`+windowsLog+`&#34; 2&gt;&amp;1&#34;</Arguments>`)
}

func TestRenderTaskXML_RejectsPathsCmdWouldExpand(t *testing.T) {
	for _, exe := range []string{`C:\100%\bilgie.exe`, `C:\a"b\bilgie.exe`} {
		_, err := RenderTaskXML(exe, windowsLog, time.Hour, time.Now())
		require.Error(t, err, exe)
	}
}

func TestRenderPlist_RejectsAnIntervalLaunchdCannotHold(t *testing.T) {
	_, err := RenderPlist(posixExe, "/Users/u", "/log", 999999*time.Hour)

	require.ErrorIs(t, err, ErrIntervalTooLong)
	_, err = RenderPlist(posixExe, "/Users/u", "/log", 24*time.Hour)
	require.NoError(t, err)
}

func TestSystemdUninstall_FindsUnitsInstalledUnderADifferentConfigDir(t *testing.T) {
	s := &scriptedExec{respond: noCrontab}
	a := newOSAgent(t, "linux", posixExe, s)
	require.NoError(t, a.Install(t.Context()))
	defaultTimer := a.TimerPath()
	require.FileExists(t, defaultTimer)

	a.ConfigDir = filepath.Join(a.Home, "elsewhere")
	require.NoError(t, a.Uninstall(t.Context()))

	assert.NoFileExists(t, defaultTimer)
	assert.NotContains(t, output(a), "agent was not installed")
}

func TestSystemdInstall_EnableFailureHintsAtAnUnseenConfigHome(t *testing.T) {
	s := &scriptedExec{respond: func(name string, args []string) ([]byte, error) {
		if name == "systemctl" && len(args) > 1 && args[1] == "enable" {
			return []byte("Failed to enable unit: unit file does not exist"), errors.New("exit status 1")
		}
		return nil, nil
	}}
	a := newOSAgent(t, "linux", posixExe, s)
	a.ConfigDir = filepath.Join(a.Home, "xdg")

	err := a.Install(t.Context())

	require.ErrorContains(t, err, "systemctl enable")
	assert.ErrorContains(t, err, "XDG_CONFIG_HOME")

	a.ConfigDir = ""
	err = a.Install(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "XDG_CONFIG_HOME")
}

func TestSystemdInstall_WarnsWhenLingerIsOff(t *testing.T) {
	s := &scriptedExec{respond: func(name string, _ []string) ([]byte, error) {
		if name == "loginctl" {
			return []byte("no\n"), nil
		}
		return nil, nil
	}}
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Install(t.Context()))

	assert.Contains(t, output(a), "loginctl enable-linger")
}

func TestSystemdInstall_StaysQuietWhenLingerIsOn(t *testing.T) {
	s := &scriptedExec{respond: func(name string, _ []string) ([]byte, error) {
		if name == "loginctl" {
			return []byte("yes\n"), nil
		}
		return nil, nil
	}}
	a := newOSAgent(t, "linux", posixExe, s)

	require.NoError(t, a.Install(t.Context()))

	assert.NotContains(t, output(a), "enable-linger")
}

func TestCronInstall_PointsAtLingerForASystemdTimer(t *testing.T) {
	s := &scriptedExec{}
	s.respond = func(name string, _ []string) ([]byte, error) {
		if name == "systemctl" {
			return nil, errors.New("no bus")
		}
		return nil, nil
	}
	a := newOSAgent(t, "linux", posixExe, s)
	a.Interval = 30 * time.Minute

	require.NoError(t, a.Install(t.Context()))

	assert.Contains(t, output(a), "loginctl enable-linger")
}
