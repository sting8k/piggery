//go:build windows

package server_test

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/driver/local"
)

// procGone: no process has pid.
func procGone(pid int) bool { return local.ProcessStartTime(pid) == 0 }

// workerProfile is the pi profile (JSON) of a worker that is this test binary running
// TestServerWorker: "brief" exits after a moment, "stdin" when its stdin closes, "nested" runs
// TestWorkerPeerClient as its own child and as the child of a process of its own (see
// TestNestedHarnessCannotSpeakForTheWorker). Windows has no /bin/sh to script it with.
func workerProfile(t *testing.T, kind string) []byte {
	t.Helper()
	t.Setenv("PGTEST_WORKER", kind)
	prof, _ := json.Marshal(map[string]any{"cmd": os.Args[0], "args": []string{"-test.run=^TestServerWorker$", "--"}})
	return prof
}

// writeNotifyHooks puts three hooks in notify.d: a slow one (30 s of ping, then it touches
// slowDone), a fast one that appends its stdin to out, and a file Windows cannot run by its
// extension that would touch legacyOut.
func writeNotifyHooks(t *testing.T, hooks, slowDone, out, legacyOut string) {
	t.Helper()
	put := func(name, text string) {
		if err := os.MkdirAll(hooks, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(text), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	put("a-slow.cmd", "@ping -n 30 127.0.0.1 >nul\r\n@echo x> \""+slowDone+"\"\r\n")
	put("b-fast.cmd", "@findstr \"^\" >> \""+out+"\"\r\n")
	put("c-not-runnable.txt", "@echo x> \""+legacyOut+"\"\r\n")
}

// TestServerWorker is the worker of the daemon tests on Windows (not a test): PGTEST_WORKER picks
// what it does.
func TestServerWorker(t *testing.T) {
	client := func(tag string) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerPeerClient$")
		cmd.Env = append(os.Environ(), "PGTEST_TAG="+tag)
		cmd.Run()
	}
	switch os.Getenv("PGTEST_WORKER") {
	case "":
		t.Skip("a worker run by the daemon tests")
	case "brief":
		time.Sleep(300 * time.Millisecond)
	case "stdin":
		io.Copy(io.Discard, os.Stdin)
	case "sleep":
		time.Sleep(time.Hour)
	case "nested":
		client("direct")
		relay := exec.Command(os.Args[0], "-test.run=^TestServerWorker$")
		relay.Env = append(os.Environ(), "PGTEST_WORKER=relay")
		relay.Run()
		io.Copy(io.Discard, os.Stdin)
	case "relay":
		client("nested")
	}
	os.Exit(0)
}
