package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// On a clean HOME, `--admin team up` must autostart the daemon (which creates admin.token)
// before reading the token. Regression: the CLI read admin.token first and failed.
func TestAdminTeamUpOnCleanHome(t *testing.T) {
	bin, home := buildWithHome(t)
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--admin", "team", "up", "p2p", "--cwd", repo)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("team up on clean home: %v\n%s", err, out)
	}
	// The shipped supervisor-executor manifest uses instructions_file, which core rejects: the CLI must
	// inline it (relative to the manifest) before team up.
	cmd = exec.Command(bin, "--admin", "team", "up", "supervisor-executor", "--cwd", repo)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("team up supervisor-executor: %v\n%s", err, out)
	}
}

// buildWithHome builds the binary and gives it a clean HOME whose daemon is stopped at cleanup.
func buildWithHome(t *testing.T) (bin, home string) {
	t.Helper()
	bin = filepath.Join(t.TempDir(), "piggery")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Short HOME: unix socket paths are limited to 104 bytes on macOS.
	home, err := os.MkdirTemp("/tmp", "pg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		exec.Command("pkill", "-TERM", "-f", "^"+bin+" serve$").Run()
		sock := filepath.Join(home, ".piggery", "piggery.sock")
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(sock); os.IsNotExist(err) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		os.RemoveAll(home)
	})
	return bin, home
}

// `piggery mcp` through main, as Claude runs it for a worker: it answers initialize and lists the
// role's tools (Claude calls send mcp__piggery__send). Regression: main refused `mcp` before the
// CLI ("mcp is not available in v1") and a live Claude worker had no piggery tools.
func TestMCPThroughMain(t *testing.T) {
	bin, home := buildWithHome(t)
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	run := func(env []string, stdin string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(append(os.Environ(), "HOME="+home), env...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("piggery %v: %v\n%s", args, err, out)
		}
		return out
	}
	run(nil, "", "--admin", "team", "up", "p2p", "--cwd", repo)
	var j struct {
		ID, Token string
		RunID     string `json:"run_id"`
	}
	if err := json.Unmarshal(run(nil, "", "--admin", "--json", "join", "--team", "p2p", "--role", "peer", "--name", "w1"), &j); err != nil {
		t.Fatal(err)
	}
	out := run([]string{"PIGGERY_ID=" + j.ID, "PIGGERY_TOKEN=" + j.Token, "PIGGERY_RUN_ID=" + j.RunID}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
`, "mcp")
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"serverInfo":{"name":"piggery"`) {
		t.Fatalf("mcp output:\n%s", out)
	}
	var list struct {
		Result struct{ Tools []struct{ Name string } }
	}
	json.Unmarshal([]byte(lines[1]), &list)
	var names []string
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "send,inbox,who,agent" {
		t.Fatalf("tools/list = %v\n%s", names, out)
	}
}

// `piggery restart` shuts the daemon down and starts one again from this binary: the new daemon
// answers, with another pid. With no daemon running it only starts one.
func TestRestart(t *testing.T) {
	bin, home := buildWithHome(t)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	pid := func() int {
		t.Helper()
		var ps struct{ PID int }
		if err := json.Unmarshal([]byte(run("--admin", "ps", "--json")), &ps); err != nil || ps.PID == 0 {
			t.Fatalf("ps --json: %v", err)
		}
		return ps.PID
	}
	if out := run("--admin", "restart"); !strings.HasPrefix(out, "was not running; started pid ") {
		t.Fatalf("restart with no daemon: %q", out)
	}
	before := pid()
	out := run("--admin", "restart")
	after := pid()
	if after == before || !strings.Contains(out, "restarted: pid ") {
		t.Fatalf("restart: %q; pid %d -> %d, want a new daemon", out, before, after)
	}
}
