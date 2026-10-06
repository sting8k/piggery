//go:build windows

package local

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// A worker started through a .cmd shim (as an npm-installed pi is) runs as the shim's interpreter
// with the real program under it: after a daemon restart (no job handle, a new Driver) recovery
// still calls it ours, and killing it ends that program too.
func TestRecoverWorkerStartedThroughCmdShim(t *testing.T) {
	d, dir := newDriver(t, "replay", Options{})
	shim := filepath.Join(t.TempDir(), "pi.cmd")
	// No ^ in the args: cmd would read it as an escape.
	if err := os.WriteFile(shim, []byte("@\""+os.Args[0]+"\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prof, _ := json.Marshal(Profile{Cmd: shim, Args: []string{"-test.run=TestHelperProcess$", "--"}})
	if err := os.WriteFile(ProfilePath(dir), prof, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	proc, err := d.Start(ctx, core.Spec{ParticipantID: "p9", RunID: "r9", Token: "tok", Cwd: dir, HarnessRef: "sess-9"})
	if err != nil {
		t.Fatal(err)
	}
	probe, _ := waitRecord(t, d, "p9", "probe_env")
	inner := int(probe["pid"].(float64))
	defer killPID(inner)
	if inner == proc.PID {
		t.Fatalf("the shim and the program under it have one pid %d: not a shim run", inner)
	}

	closeJob(proc.PID) // the daemon restarted: its job handle is gone
	d2 := New(dir, Options{})
	if st, err := d2.Inspect(ctx, proc); err != nil || st != core.ProcOurs {
		t.Fatalf("inspect the live worker after a restart = %q, %v (recorded %v); want ours", st, err, proc.Cmdline[:1])
	}
	if _, err := d2.KillVerified(ctx, proc); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); procAlive(inner); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("program %d under the shim survived the kill", inner)
		}
	}
}

// A process that exited with code 259 (STILL_ACTIVE) is dead, not alive, while someone still holds
// its handle: here the test, which has not waited for it.
func TestExitCode259IsNotAliveProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "PGDRV_WRAP=", "PGDRV_HELPER=exit259")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for alive := true; alive; time.Sleep(50 * time.Millisecond) {
		_, _, alive, _ = psInfo(context.Background(), cmd.Process.Pid)
		if alive && time.Now().After(deadline) {
			t.Fatal("a process that exited with code 259 is still reported alive")
		}
	}
}

// What Start passes an npm .cmd shim reaches the program as it was: cmd.exe would read the line a
// batch file is started with by its own rules (os/exec documents that Go does not quote for it),
// cut it at the first newline and expand %VAR%, and a Claude worker's role card has both. So
// prepareCommand runs the shim's target directly. The shim here is npm's own form (cmd-shim), its
// node.exe this test binary (the shim runs the node.exe beside it first) and its script an argument.
func TestArgumentsSurviveAnNpmShim(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, os.Args[0], filepath.Join(dir, "node.exe"))
	script := filepath.Join(dir, "node_modules", "pkg", "cli.js")
	if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(script, nil, 0o600)
	shim := "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nSETLOCAL\r\nCALL :find_dp0\r\n\r\n" +
		"IF EXIST \"%dp0%\\node.exe\" (\r\n  SET \"_prog=%dp0%\\node.exe\"\r\n) ELSE (\r\n  SET \"_prog=node\"\r\n)\r\n\r\n" +
		"endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & \"%_prog%\"  \"-test.run=^TestHelperProcess$\" -- \"%dp0%\\node_modules\\pkg\\cli.js\" %*\r\n"
	shimPath := filepath.Join(dir, "tool.cmd")
	if err := os.WriteFile(shimPath, []byte(shim), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "argv.json")
	args := []string{"plain", "two words", "line one\nline two", "a&b", "x|y", "100%", "%OS%", `say "hi"`, "c^d", "(paren)", "<>"}
	cmd := exec.Command(shimPath, args...)
	if err := prepareCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "node.exe" {
		t.Fatalf("the shim was not looked through: %s", cmd.Path)
	}
	cmd.Env = append(os.Environ(), "PGDRV_WRAP=", "PGDRV_HELPER=argv", "PGDRV_OUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shim: %v\n%s", err, b)
	}
	var got []string
	b, err := os.ReadFile(out)
	if want := append([]string{script}, args...); err != nil || json.Unmarshal(b, &got) != nil || !slices.Equal(got, want) {
		t.Fatalf("the program got %q (%v); want %q", got, err, want)
	}
}

// A .cmd of another form is run as it is, but an argument it cannot pass on is refused, not cut.
func TestUnknownCmdShimRefusesANewline(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "odd.cmd")
	os.WriteFile(shim, []byte("@echo off\r\n"), 0o600)
	if err := prepareCommand(exec.Command(shim, "one\ntwo")); err == nil {
		t.Fatal("an argument with a newline went to a .cmd of an unknown form")
	}
	if err := prepareCommand(exec.Command(shim, "one two")); err != nil {
		t.Fatalf("plain arguments refused: %v", err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o700); err != nil {
		t.Fatal(err)
	}
}
