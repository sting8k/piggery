package local

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// lstartLayout is `ps -o lstart` (local time).
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// ProcessStartTime is the OS start time of pid in unix ms, or 0 when unknown. Recovery
// compares it with the live process before killing anything, so a reused pid
// is never mistaken for the worker; a Claude session's host key uses it the same way.
// `ps -o lstart=` exists on macOS and Linux (procps) and prints local time with one-second
// resolution.
func ProcessStartTime(pid int) int64 {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	t, err := time.ParseInLocation(lstartLayout, strings.Join(strings.Fields(string(out)), " "), time.Local)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// ParentPID is pid's parent, or 0 when unknown (the process is gone).
func ParentPID(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}
