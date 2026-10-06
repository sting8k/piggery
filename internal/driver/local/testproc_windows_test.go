//go:build windows

package local

import (
	"context"
	"os"
	"slices"
	"syscall"
	"testing"
)

// procAlive: pid is a running process.
func procAlive(pid int) bool {
	_, _, alive, _ := psInfo(context.Background(), pid)
	return alive
}

func killPID(pid int) { newKill()(pid, syscall.SIGKILL) }

// claudeCard is the role card a Claude worker was started with args: the text of the file they
// name, and never an argument (a command line is limited to 32,767 characters here).
func claudeCard(t *testing.T, args []string) string {
	t.Helper()
	i := slices.Index(args, "--append-system-prompt-file")
	if i < 0 || i+1 >= len(args) || slices.Contains(args, "--append-system-prompt") {
		t.Fatalf("args %q: want the role card in a file, not in an argument", args)
	}
	b, err := os.ReadFile(args[i+1])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
