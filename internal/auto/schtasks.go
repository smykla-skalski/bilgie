package auto

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

const taskStartDelay = time.Minute

// TaskXMLPath is where the Task Scheduler definition is kept on disk.
func (a Agent) TaskXMLPath() string {
	return filepath.Join(a.StateDir, a.id().task+"-task.xml")
}

func (a Agent) installTask(ctx context.Context) error {
	definition, err := RenderTaskXML(a.Exe, a.logPath(), a.Interval, a.now().Add(taskStartDelay))
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.TaskXMLPath(), EncodeTaskXML(definition)); err != nil {
		return err
	}
	out, err := a.Exec(ctx, "schtasks", "/Create", "/TN", a.id().task, "/XML", a.TaskXMLPath(), "/F")
	if err != nil {
		return fmt.Errorf("schtasks create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(a.Out, "installed task %s (every %s)\n", a.id().task, a.Interval)
	return nil
}

// taskExists lists every task and looks for ours. Listing is used instead of
// querying the task by name because the error text for a missing task is
// localised; a failed listing proves nothing, so it is returned as an error.
func (a Agent) taskExists(ctx context.Context) (bool, error) {
	out, err := a.Exec(ctx, "schtasks", "/Query", "/FO", "CSV", "/NH")
	if err != nil {
		return false, fmt.Errorf("schtasks query: %w: %s", err, strings.TrimSpace(string(out)))
	}
	reader := csv.NewReader(strings.NewReader(string(out)))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	records, err := reader.ReadAll()
	if err != nil {
		return false, fmt.Errorf("parse schtasks query: %w", err)
	}
	for _, rec := range records {
		if len(rec) > 0 && strings.EqualFold(strings.TrimPrefix(rec[0], `\`), a.id().task) {
			return true, nil
		}
	}
	return false, nil
}

// deleteTask removes the task. A task that does not exist is the state the
// caller wants; when the delete fails, a successful listing without the task
// confirms it is absent and anything else keeps the delete error.
func (a Agent) deleteTask(ctx context.Context) (bool, error) {
	out, err := a.Exec(ctx, "schtasks", "/Delete", "/TN", a.id().task, "/F")
	if err == nil {
		return true, nil
	}
	deleteErr := fmt.Errorf("schtasks delete: %w: %s", err, strings.TrimSpace(string(out)))
	if ctx.Err() != nil {
		return false, deleteErr
	}
	exists, listErr := a.taskExists(ctx)
	if listErr != nil || exists {
		return false, deleteErr
	}
	return false, nil
}

func (a Agent) uninstallTask(ctx context.Context) (bool, error) {
	deleted, err := a.deleteTask(ctx)
	if err != nil {
		return false, err
	}
	fileRemoved, err := removeIfExists(a.TaskXMLPath())
	if err != nil {
		return false, fmt.Errorf("remove task definition: %w", err)
	}
	if deleted {
		fmt.Fprintf(a.Out, "removed task %s\n", a.id().task)
	}
	return deleted || fileRemoved, nil
}

// taskDuration formats a whole number of minutes as an ISO 8601 duration.
func taskDuration(interval time.Duration) (string, error) {
	if interval < time.Minute || interval%time.Minute != 0 {
		return "", fmt.Errorf("interval must be a whole number of minutes for Task Scheduler, got %s", interval)
	}
	minutes := int(interval / time.Minute)
	hours, rest := minutes/60, minutes%60
	var b strings.Builder
	b.WriteString("PT")
	if hours > 0 {
		fmt.Fprintf(&b, "%dH", hours)
	}
	if rest > 0 {
		fmt.Fprintf(&b, "%dM", rest)
	}
	return b.String(), nil
}

func xmlText(s string) string {
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(s))
	return esc.String()
}

// RenderTaskXML builds the Task Scheduler definition that runs `exe tick`,
// through cmd.exe so its output is appended to logPath, every interval from start on, at below-normal priority and only while the
// user is logged on, so it needs no stored password.
func RenderTaskXML(exe, logPath string, interval time.Duration, start time.Time) (string, error) {
	repeat, err := taskDuration(interval)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(exe+logPath, "%\"\r\n") {
		return "", fmt.Errorf("paths must not contain %%, quotes or line breaks")
	}
	arguments := `/d /s /c ""` + exe + `" ` + agentCommand + ` >> "` + logPath + `" 2>&1"`
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>%s automatic cache cleanup</Description>
  </RegistrationInfo>
  <Triggers>
    <TimeTrigger>
      <Repetition>
        <Interval>%s</Interval>
        <StopAtDurationEnd>false</StopAtDurationEnd>
      </Repetition>
      <StartBoundary>%s</StartBoundary>
      <Enabled>true</Enabled>
    </TimeTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <AllowHardTerminate>true</AllowHardTerminate>
    <ExecutionTimeLimit>PT1H</ExecutionTimeLimit>
    <Priority>7</Priority>
    <Hidden>true</Hidden>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>cmd.exe</Command>
      <Arguments>%s</Arguments>
    </Exec>
  </Actions>
</Task>
`, agentName, repeat, start.Format("2006-01-02T15:04:05"), xmlText(arguments)), nil
}

// EncodeTaskXML converts the definition to the UTF-16 little-endian text
// with a byte order mark that schtasks expects for /XML input.
func EncodeTaskXML(definition string) []byte {
	return append([]byte{0xFF, 0xFE}, utf16LE(definition)...)
}

func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2*len(units))
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}
