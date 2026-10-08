//go:build windows

package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows has no execute bit and no shebang: the extension says how a hook file runs.
//   - .exe, .com: the file itself.
//   - .cmd, .bat: cmd.exe /d /s /c "<file>". /s makes cmd drop exactly the outer quotes of the line,
//     so the quotes around the path stay whatever the path holds (spaces, parentheses: without /s cmd
//     keeps or drops them by a rule that those break); /d skips the user's AutoRun.
//   - .ps1: powershell -NoProfile -ExecutionPolicy Bypass -File <file> (Windows PowerShell, which
//     every Windows has). A machine policy set by group policy still wins over Bypass: the hook then
//     fails and the error is logged.
//   - .sh: not run (no defined way to run it on Windows); warnSkippedHooks says so once.
//
// The stdin JSON line reaches the hook the same way on each (cmd and powershell pass their stdin on).
func hookExt(name string) string { return strings.ToLower(filepath.Ext(name)) }

// hookRunnable: a regular file Windows can run by itself or through cmd or powershell.
func hookRunnable(fi os.FileInfo) bool {
	if !fi.Mode().IsRegular() {
		return false
	}
	switch hookExt(fi.Name()) {
	case ".exe", ".com", ".cmd", ".bat", ".ps1":
		return true
	}
	return false
}

// system32 is a program of the system directory, by path: the daemon's PATH is not trusted for it.
func system32(elem ...string) string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		return elem[len(elem)-1]
	}
	return filepath.Join(append([]string{root, "System32"}, elem...)...)
}

// hookCommand is the command that runs the hook file path, by its extension.
func hookCommand(ctx context.Context, path string) *exec.Cmd {
	switch hookExt(path) {
	case ".cmd", ".bat":
		cmd := exec.CommandContext(ctx, system32("cmd.exe"))
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c ""` + path + `""`}
		return cmd
	case ".ps1":
		return exec.CommandContext(ctx, system32("WindowsPowerShell", "v1.0", "powershell.exe"),
			"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", path)
	}
	return exec.CommandContext(ctx, path)
}

// hookKill hides the hook's console window and makes its job (a Job Object, no limits: what the hook
// leaves running when it ends normally keeps running, as on unix) the way a timeout ends it: the
// hook and everything it started. attach puts the started hook in the job, release closes the job's
// handle. A child the hook starts before attach is outside the job (os/exec cannot start a process
// suspended). Without a job a timeout ends the hook process alone.
func hookKill(cmd *exec.Cmd) (attach func() error, release func()) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	var mu sync.Mutex
	var job windows.Handle
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		if job != 0 && windows.TerminateJobObject(job, 1) == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	attach = func() error {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return err
		}
		p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if err != nil {
			windows.CloseHandle(h)
			return err
		}
		defer windows.CloseHandle(p)
		if err := windows.AssignProcessToJobObject(h, p); err != nil {
			windows.CloseHandle(h)
			return err
		}
		mu.Lock()
		job = h
		mu.Unlock()
		return nil
	}
	release = func() {
		mu.Lock()
		h := job
		job = 0
		mu.Unlock()
		if h != 0 {
			windows.CloseHandle(h)
		}
	}
	return attach, release
}

var skippedLogged sync.Map // file names already warned about

// warnSkippedHooks logs, once per file, each .sh file of hooks/notify.d: it does not run here.
func (s *server) warnSkippedHooks() {
	entries, _ := os.ReadDir(NotifyHooksDir(s.dir))
	for _, e := range entries {
		if hookExt(e.Name()) != ".sh" {
			continue
		}
		if _, seen := skippedLogged.LoadOrStore(filepath.Join(s.dir, e.Name()), true); !seen {
			s.log.Warn("notify hook skipped: a .sh file has no defined way to run on Windows (use .ps1, .cmd or .exe)", "hook", e.Name())
		}
	}
}
