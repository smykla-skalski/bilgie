//go:build windows

package osshim

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procEntry is the part of a Toolhelp process entry the shims use.
type procEntry struct {
	procInfo
	exe string
}

// snapshotProcesses returns a Toolhelp snapshot of every running process.
func snapshotProcesses() ([]procEntry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	entry := windows.ProcessEntry32{}
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil, err
	}

	var entries []procEntry
	for {
		entries = append(entries, procEntry{
			procInfo: procInfo{pid: entry.ProcessID, ppid: entry.ParentProcessID},
			exe:      windows.UTF16ToString(entry.ExeFile[:]),
		})
		err := windows.Process32Next(snap, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// ProcessTable returns the executable name of every running process from a
// Toolhelp snapshot. Windows exposes no cheap argv for other processes, so
// only image names such as "go.exe" are returned.
func ProcessTable(ctx context.Context) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := snapshotProcesses()
	if err != nil {
		return nil, err
	}
	procs := make([]Process, 0, len(entries))
	for i := range entries {
		proc := Process{PID: int(entries[i].pid), PPID: int(entries[i].ppid), CommandLine: entries[i].exe, Args: entries[i].exe}
		if created, err := creationTime(entries[i].pid); err == nil {
			proc.Started = time.Unix(0, created)
		}
		procs = append(procs, proc)
	}
	return procs, nil
}
