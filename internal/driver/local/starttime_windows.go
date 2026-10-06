//go:build windows

package local

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProcessStartTime is the OS start time of pid in unix ms, or 0 when unknown: its creation time
// (OpenProcess + GetProcessTimes, 100 ns resolution). Recovery compares it with the live process
// before killing anything, so a reused pid is never mistaken for the worker; a Claude session's
// host key uses it the same way.
func ProcessStartTime(pid int) int64 {
	_, ns := procStart(pid)
	return ns / 1_000_000
}

// ParentPID is pid's parent, or 0 when unknown (the process is gone, or its parent was created
// after it: the pid was reused).
func ParentPID(pid int) int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if int(e.ProcessID) == pid {
			ppid := int(e.ParentProcessID)
			if _, pc := procStart(ppid); pc > 0 {
				if _, cc := procStart(pid); cc > 0 && pc > cc {
					return 0
				}
			}
			return ppid
		}
	}
	return 0
}
