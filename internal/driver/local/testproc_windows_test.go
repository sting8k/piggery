//go:build windows

package local

import (
	"context"
	"syscall"
)

// procAlive: pid is a running process.
func procAlive(pid int) bool {
	_, _, alive, _ := psInfo(context.Background(), pid)
	return alive
}

func killPID(pid int) { newKill()(pid, syscall.SIGKILL) }
