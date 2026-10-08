//go:build unix

package server_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// procGone: no process has pid.
func procGone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// workerProfile is the pi profile (JSON) of a worker that is a shell script: "brief" exits after a
// moment, "stdin" when its stdin closes, "nested" runs this test binary as TestWorkerPeerClient as
// its own child and as the child of a shell of its own (see TestNestedHarnessCannotSpeakForTheWorker).
func workerProfile(t *testing.T, kind string) []byte {
	t.Helper()
	script := map[string]string{"brief": "sleep 0.3", "stdin": "cat >/dev/null"}[kind]
	if kind == "nested" {
		cl := fmt.Sprintf("%q -test.run=^TestWorkerPeerClient$", os.Args[0])
		script = fmt.Sprintf(`PGTEST_TAG=direct %s; sh -c 'PGTEST_TAG=nested %s; :'; read x`, cl, cl)
	}
	prof, _ := json.Marshal(map[string]any{"cmd": "/bin/sh", "args": []string{"-c", script, "sh"}})
	return prof
}

// writeNotifyHooks puts three hooks in notify.d: a slow one (30 s, then it touches slowDone), a fast
// one that appends its stdin to out, and a file with no execute bit that would touch legacyOut.
func writeNotifyHooks(t *testing.T, hooks, slowDone, out, legacyOut string) {
	t.Helper()
	put := func(name, text string, mode os.FileMode) {
		if err := os.MkdirAll(hooks, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	put("a-slow", "#!/bin/sh\nsleep 30\ntouch '"+slowDone+"'\n", 0o700)
	put("b-fast", "#!/bin/sh\ncat >> '"+out+"'\n", 0o700)
	put("c-not-executable", "#!/bin/sh\ntouch '"+legacyOut+"'\n", 0o600)
}
