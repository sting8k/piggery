//go:build unix

package cli

import (
	"errors"
	"os/exec"
	"syscall"
)

// detach starts cmd (the daemon) in a session of its own, so closing the terminal does not end it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// daemonDown: err says nobody listens at the daemon's address (no socket, or a socket nobody
// accepts on).
func daemonDown(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}
