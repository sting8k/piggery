//go:build windows

package server

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/driver/local"
)

// A .cmd hook in a directory whose name has spaces and parentheses runs, and gets the notice line
// on its stdin untouched (& | ^ and quotes in it included).
func TestCmdHookInAwkwardDirGetsStdin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b (c)")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "hook.cmd")
	if err := os.WriteFile(hook, []byte("@findstr \"^\" > \"%~dp0out.txt\"\r\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"body":"a & b | c ^ d \"e\""}` + "\n"
	s := &server{log: slog.Default()}
	if err := s.runHook(context.Background(), hook, []byte(line)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "out.txt")); strings.TrimSpace(string(got)) != strings.TrimSpace(line) {
		t.Fatalf("hook stdin = %q, want %q", got, line)
	}
}

// A hook that outlives its timeout is ended with what it started: here a .ps1 (run through
// powershell -File) starts a process of its own and sleeps.
func TestPs1HookTimeoutEndsItsTree(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.ps1")
	script := "$p = Start-Process -FilePath ping -ArgumentList '-n','60','127.0.0.1' -WindowStyle Hidden -PassThru\r\n" +
		"Set-Content -Path \"$PSScriptRoot\\child.pid\" -Value $p.Id\r\nStart-Sleep -Seconds 60\r\n"
	if err := os.WriteFile(hook, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s := &server{log: slog.Default()}
	if err := s.runHook(ctx, hook, []byte("{}\n")); err == nil {
		t.Fatal("hook ended by itself; want the timeout to end it")
	}
	b, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatalf("the hook never started its child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("child pid %q: %v", b, err)
	}
	defer exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	for deadline := time.Now().Add(5 * time.Second); local.ProcessStartTime(pid) != 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the hook's child %d survived the timeout", pid)
		}
	}
}
