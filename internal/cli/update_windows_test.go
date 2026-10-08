//go:build windows

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestUpdateHelperSleep is the running program of TestUpdateReplacesARunningBinary (not a test).
func TestUpdateHelperSleep(t *testing.T) {
	if os.Getenv("PIGGERY_TEST_SLEEP") != "" {
		time.Sleep(time.Minute)
	}
}

// The binary `piggery update` replaces is running (the CLI itself, and the daemon): the new one
// takes its place all the same, and the running one keeps running from where it was moved to.
func TestUpdateReplacesARunningBinary(t *testing.T) {
	dir := t.TempDir()
	linkExe(t, dir, "piggery")
	exe := filepath.Join(dir, "piggery.exe")
	running := exec.Command(exe, "-test.run=^TestUpdateHelperSleep$")
	running.Env = append(os.Environ(), "PIGGERY_TEST_SLEEP=1")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		running.Wait()
		close(ended)
	}()
	defer func() {
		running.Process.Kill()
		<-ended // its image is unlocked before the temp dir is removed
	}()

	tmp := filepath.Join(dir, ".piggery-update-new")
	if err := os.WriteFile(tmp, []byte("new binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := putBinary(tmp, exe, "windows"); err != nil {
		t.Fatalf("replace a running binary: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("the binary is %q after the update", b)
	}
	if _, err := os.Stat(exe + ".old"); err != nil {
		t.Fatalf("the running binary was not moved aside: %v", err)
	}
	select {
	case <-ended:
		t.Fatal("the running program ended when its binary was replaced")
	default:
	}
}
