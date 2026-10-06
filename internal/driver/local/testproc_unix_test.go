//go:build unix

package local

import (
	"errors"
	"syscall"
)

// procAlive: pid is a process (a zombie counts until it is reaped).
func procAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func killPID(pid int) { syscall.Kill(pid, syscall.SIGKILL) }
