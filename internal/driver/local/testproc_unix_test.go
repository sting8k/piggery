//go:build unix

package local

import (
	"errors"
	"slices"
	"syscall"
	"testing"
)

// procAlive: pid is a process (a zombie counts until it is reaped).
func procAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func killPID(pid int) { syscall.Kill(pid, syscall.SIGKILL) }

// claudeCard is the role card a Claude worker was started with args: the argument's text.
func claudeCard(t *testing.T, args []string) string {
	t.Helper()
	i := slices.Index(args, "--append-system-prompt")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("args %q: no --append-system-prompt", args)
	}
	return args[i+1]
}
