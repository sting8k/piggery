//go:build unix

package server

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// hookRunnable: a regular file with an execute bit.
func hookRunnable(fi os.FileInfo) bool { return fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 }

// hookCommand is the command that runs the hook file: the file itself.
func hookCommand(ctx context.Context, path string) *exec.Cmd { return exec.CommandContext(ctx, path) }

// hookKill gives the hook a process group of its own, so its timeout kills whatever it started too.
// attach runs once the hook has started, release when it has ended: nothing to do here.
func hookKill(cmd *exec.Cmd) (attach func() error, release func()) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return func() error { return nil }, func() {}
}

// warnSkippedHooks: every file with an execute bit runs, so none is skipped.
func (s *server) warnSkippedHooks() {}
