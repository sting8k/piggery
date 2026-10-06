//go:build windows

package local

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// psInfo reads the OS start time (unix ms) and the program (the image's full path: Windows keeps
// no argv a reader could split) of pid. alive=false when there is no such process or it has
// exited; err when it exists but cannot be read (another user's).
//
// A process is alive while its handle is not signalled (WaitForSingleObject with no wait): its exit
// code cannot say, 259 (STILL_ACTIVE) is also what a process that exited with 259 leaves. Reading the
// handle needs SYNCHRONIZE; a process that refuses it (another user's) is read with the query right
// alone, and then a 259 is taken as running.
func psInfo(_ context.Context, pid int) (start int64, command string, alive bool, err error) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	synced := err == nil
	if err == windows.ERROR_ACCESS_DENIED {
		h, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	}
	if err == windows.ERROR_INVALID_PARAMETER {
		return 0, "", false, nil // no such process
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("read process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if synced {
		if ev, err := windows.WaitForSingleObject(h, 0); err == nil && ev != uint32(windows.WAIT_TIMEOUT) {
			return 0, "", false, nil // exited, its handle kept open by someone
		}
	} else if code, ok := exitCode(h); ok && code != 259 {
		return 0, "", false, nil
	}
	var ct, et, kt, ut windows.Filetime
	if err := windows.GetProcessTimes(h, &ct, &et, &kt, &ut); err != nil {
		return 0, "", false, fmt.Errorf("read process %d times: %w", pid, err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return 0, "", false, fmt.Errorf("read process %d image: %w", pid, err)
	}
	return ct.Nanoseconds() / 1_000_000, windows.UTF16ToString(buf[:n]), true, nil
}

// ProcessImage is the file name of pid's program without its .exe suffix and in lower case ("claude"
// for claude.exe); "" when pid is not there or cannot be read.
func ProcessImage(pid int) string {
	_, image, alive, err := psInfo(context.Background(), pid)
	if err != nil || !alive {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(filepath.Base(image)), ".exe")
}

func exitCode(h windows.Handle) (uint32, bool) {
	var code uint32
	return code, windows.GetExitCodeProcess(h, &code) == nil
}

// startedProgram is the program recorded for a worker started as cmd: the image the OS runs for
// pid, which is not cmd's own when cmd is a shim (pi.cmd runs as cmd.exe, an npm .cmd then starts
// node.exe under it). Inspect compares against what the OS shows, so that is what is recorded;
// cmd only when the process cannot be read (it has already exited: recovery finds it dead).
func startedProgram(pid int, cmd string) string {
	if _, image, alive, err := psInfo(context.Background(), pid); err == nil && alive {
		return image
	}
	return cmd
}

// sameProgram: the image command is the recorded program, by file name.
func sameProgram(command, recorded string) bool {
	return programName(filepath.Base(command)) == programName(filepath.Base(recorded))
}

// programName is how a program is compared with the recorded one: a file name has no case and no
// executable suffix on Windows (pi.cmd, pi.exe and PI are one program).
func programName(s string) string {
	s = strings.ToLower(s)
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
		s = strings.TrimSuffix(s, ext)
	}
	return s
}
