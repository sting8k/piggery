//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts cmd (the daemon) with no console and in a process group of its own, so closing
// the terminal or Ctrl-C there does not end it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
}

// daemonDown: err says nobody listens at the daemon's address (no pipe of that name).
func daemonDown(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND)
}
