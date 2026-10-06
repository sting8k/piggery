//go:build windows

package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
