//go:build windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/driver/local"
)

// An interactive session is found on Windows, where there is no ps: a process named claude.exe is
// the host of the hook (its child) that asks, by the parent link, the image name and the start time,
// and its command line is readable. The test binary copied to claude.exe plays the host and runs
// itself as the hook (TestProcessHostRole).
func TestProcessHostFindsAClaudeExe(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "claude.exe")
	linkExe(t, dir, "claude") // a copy named claude.exe
	out := filepath.Join(dir, "out")
	cmd := exec.Command(host, "-test.run=^TestProcessHostRole$")
	cmd.Env = append(os.Environ(), "PGHOST_MODE=host", "PGHOST_OUT="+out)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("claude:%d:%d", cmd.Process.Pid, local.ProcessStartTime(cmd.Process.Pid)) // while it runs
	if err := cmd.Wait(); err != nil {
		t.Fatalf("host: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got, args, _ := strings.Cut(string(b), "\n")
	if got != want {
		t.Fatalf("processHost = %q; want %q", got, want)
	}
	if !strings.Contains(args, "-test.run=^TestProcessHostRole$") {
		t.Fatalf("the host's command line = %q; want its arguments", args)
	}
}

// TestProcessHostRole is the host and the hook of the test above (not a test).
func TestProcessHostRole(t *testing.T) {
	switch os.Getenv("PGHOST_MODE") {
	case "":
		t.Skip("run by TestProcessHostFindsAClaudeExe")
	case "host":
		leaf := exec.Command(os.Args[0], "-test.run=^TestProcessHostRole$")
		leaf.Env = append(os.Environ(), "PGHOST_MODE=leaf")
		if b, err := leaf.CombinedOutput(); err != nil {
			t.Fatalf("leaf: %v\n%s", err, b)
		}
	case "leaf":
		line := processHost(os.Getppid(), "claude") + "\n" + strings.Join(processArgs(os.Getppid()), " ")
		os.WriteFile(os.Getenv("PGHOST_OUT"), []byte(line), 0o600)
	}
}
