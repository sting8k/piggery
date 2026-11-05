//go:build unix

package local

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// lstartLayout is `ps -o lstart` (local time) in the C locale.
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// psCommand runs ps with its times in the C locale: lstart is parsed with lstartLayout, and a
// localized LC_TIME (vi_VN prints "T4 Thg 11  4 16:32:04 2026") would make every start time
// unknown. Only the time changes: LC_ALL would override LC_TIME, so it goes, and what it set for
// characters stays as LC_CTYPE; under LC_ALL=C ps would escape a non-ASCII command (macOS prints
// "café" as "cafM-CM-)") and a worker's program would no longer match the one recorded.
func psCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ps", args...)
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "LC_ALL=") })
	if all := os.Getenv("LC_ALL"); all != "" {
		env = append(env, "LC_CTYPE="+all)
	}
	cmd.Env = append(env, "LC_TIME=C")
	return cmd
}

// ProcessStartTime is the OS start time of pid in unix ms, or 0 when unknown. Recovery
// compares it with the live process before killing anything, so a reused pid
// is never mistaken for the worker; a Claude session's host key uses it the same way.
// `ps -o lstart=` exists on macOS and Linux (procps) and prints local time with one-second
// resolution.
func ProcessStartTime(pid int) int64 {
	out, err := psCommand(context.Background(), "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
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
