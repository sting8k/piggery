package local

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

const fixture = "../../../testdata/fixtures/pi-0.87.1-rpc-glm-5.3-flash.jsonl"

// TestHelperProcess is the fake worker (not a test): PGDRV_HELPER selects its behaviour.
//   - replay: report env/args, replay captured pi rpc stdout, ask a confirm dialog, echo the
//     answer read from stdin, then exit 0 when stdin closes.
//   - stubborn: ignore stdin EOF and SIGTERM, start a grandchild in the same process group.
//   - rpc: echo each stdin line as probe_in; answer set_model by id (modelId "bad" refused);
//     keep a thinking level from --thinking and set_thinking_level, "bogus" running as "high"
//     as pi does (accepted with a warning), and report it in get_state.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("PGDRV_HELPER")
	if mode == "" {
		return
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	switch mode {
	case "replay":
		var leaked []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "PIGGERY_") && !strings.HasPrefix(kv, "PIGGERY_ID=") && !strings.HasPrefix(kv, "PIGGERY_TOKEN=") &&
				!strings.HasPrefix(kv, "PIGGERY_RUN_ID=") {
				leaked = append(leaked, kv)
			}
		}
		emit(map[string]any{"type": "probe_env", "id": os.Getenv("PIGGERY_ID"), "token": os.Getenv("PIGGERY_TOKEN"), "run": os.Getenv("PIGGERY_RUN_ID"), "leaked": leaked, "args": os.Args,
			"agent_dir": os.Getenv("PI_CODING_AGENT_DIR")})
		b, err := os.ReadFile(os.Getenv("PGDRV_FIXTURE"))
		if err != nil {
			os.Exit(3)
		}
		os.Stdout.Write(b)
		emit(map[string]any{"type": "extension_ui_request", "id": "dlg-1", "method": "confirm", "title": "allow?"})
		in := bufio.NewScanner(os.Stdin)
		if in.Scan() {
			emit(map[string]any{"type": "probe_ui_response", "line": in.Text()})
		}
		for in.Scan() {
		}
		os.Exit(0)
	case "rpc":
		level := "medium"
		think := func(l string) {
			if level = l; l == "bogus" {
				level = "high"
			}
		}
		for i, a := range os.Args {
			if a == "--thinking" && i+1 < len(os.Args) {
				think(os.Args[i+1])
			}
		}
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			emit(map[string]any{"type": "probe_in", "line": in.Text()})
			var cmd struct{ ID, Type, ModelId, Level string }
			json.Unmarshal(in.Bytes(), &cmd)
			switch cmd.Type {
			case "set_model":
				ok := cmd.ModelId != "bad"
				emit(map[string]any{"type": "response", "id": cmd.ID, "command": "set_model", "success": ok, "error": map[bool]string{false: "Model not found"}[ok]})
			case "set_thinking_level":
				think(cmd.Level)
				emit(map[string]any{"type": "response", "id": cmd.ID, "command": cmd.Type, "success": true})
			case "get_state":
				emit(map[string]any{"type": "response", "id": cmd.ID, "command": cmd.Type, "success": true,
					"data": map[string]any{"thinkingLevel": level}})
			}
		}
		os.Exit(0)
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		emit(map[string]any{"type": "probe_child", "pid": child.Process.Pid})
		for {
			time.Sleep(time.Hour) // not select{}: with no other goroutine that is a fatal deadlock
		}
	}
	os.Exit(2)
}

func newDriver(t *testing.T, mode string, opts Options) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	// An old profile with --no-extensions: it must not reach the worker.
	prof, _ := json.Marshal(Profile{Cmd: os.Args[0], Args: []string{"-test.run=^TestHelperProcess$", "--no-extensions", "--"}})
	if err := os.MkdirAll(filepath.Dir(ProfilePath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProfilePath(dir), prof, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGDRV_HELPER", mode)
	abs, err := filepath.Abs(fixture) // the worker runs in Spec.Cwd, not the package dir
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGDRV_FIXTURE", abs)
	t.Setenv("PIGGERY_DISABLED", "1") // must not reach the worker
	t.Setenv("HOME", t.TempDir())     // no human pi setup: the worker agent dir is built from nothing
	t.Setenv("PI_CODING_AGENT_DIR", "")
	return New(dir, opts), dir
}

// waitRecord polls Tail until a record of type typ appears.
func waitRecord(t *testing.T, d *Driver, pid, typ string) (map[string]any, []json.RawMessage) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		recs, _ := d.Tail(pid, 0)
		for _, r := range recs {
			var m map[string]any
			if json.Unmarshal(r, &m) == nil && m["type"] == typ {
				return m, recs
			}
		}
	}
	t.Fatalf("no %s record", typ)
	return nil, nil
}

func TestWorkerEnvTailAndDialogCancel(t *testing.T) {
	exits := make(chan core.Exit, 2)
	d, dir := newDriver(t, "replay", Options{OnExit: func(p, r string, e core.Exit) {
		if p == "p1" && r == "r1" {
			exits <- e
		}
	}})
	ctx := context.Background()
	proc, err := d.Start(ctx, core.Spec{ParticipantID: "p1", RunID: "r1", Token: "tok", Cwd: dir, HarnessRef: "sess-1", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if proc.PGID != proc.PID || proc.PID <= 0 {
		t.Fatalf("proc = %+v, want its own process group", proc)
	}
	if age := time.Since(time.UnixMilli(proc.StartTime)); age < -time.Minute || age > time.Minute {
		t.Fatalf("start time %d is not the OS start time of a fresh process", proc.StartTime)
	}
	if !slices.Contains(proc.Cmdline, "sess-1") || !slices.Contains(proc.Cmdline, "m1") || slices.Contains(proc.Cmdline, "--no-extensions") {
		t.Fatalf("cmdline %v: want --session-id/--model, no --no-extensions", proc.Cmdline)
	}

	resp, recs := waitRecord(t, d, "p1", "probe_ui_response")
	var ans map[string]any
	if err := json.Unmarshal([]byte(resp["line"].(string)), &ans); err != nil ||
		ans["type"] != "extension_ui_response" || ans["id"] != "dlg-1" || ans["cancelled"] != true {
		t.Fatalf("dialog answer = %v", resp["line"])
	}
	env, _ := waitRecord(t, d, "p1", "probe_env")
	agentDir := filepath.Join(AgentDirRoot(dir), "p1", "r1")
	if env["id"] != "p1" || env["token"] != "tok" || env["run"] != "r1" || env["leaked"] != nil || env["agent_dir"] != agentDir {
		t.Fatalf("worker env = %v", env)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "extensions")); err != nil {
		t.Fatalf("the run's agent dir: %v", err)
	}
	// Tail keeps every record except streaming deltas: 53 captured - 17 message_update, plus
	// probe_env, the dialog request, and probe_ui_response.
	for _, r := range recs {
		if s := string(r); strings.Contains(s, `"type":"message_update"`) || strings.Contains(s, `"type":"tool_execution_update"`) {
			t.Fatalf("tail kept a streaming delta: %.80s", s)
		}
	}
	if len(recs) != 36+3 {
		t.Fatalf("tail has %d records, want 39", len(recs))
	}

	ex, err := d.Stop(ctx, "p1")
	if err != nil || ex.Code != 0 || ex.Signal != "" {
		t.Fatalf("stop = %+v, %v; want a clean exit on stdin close", ex, err)
	}
	select {
	case e := <-exits:
		if e.Code != 0 {
			t.Fatalf("OnExit = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnExit not called")
	}
	if _, err := os.Stat(agentDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent dir after the run ended: %v; want removed", err)
	}
	// Stop on a worker that already exited signals nothing (its pgid may be reused) and
	// returns the recorded exit.
	var signals []syscall.Signal
	d.kill = func(_ int, sig syscall.Signal) error { signals = append(signals, sig); return nil }
	if again, err := d.Stop(ctx, "p1"); err != nil || again != ex || len(signals) != 0 {
		t.Fatalf("stop after exit = %+v, %v, signals %v; want the recorded exit and no signal", again, err, signals)
	}
	if last, _ := d.Tail("p1", 1); len(last) != 1 || !strings.Contains(string(last[0]), "probe_ui_response") {
		t.Fatalf("tail after stop = %s", last)
	}
}

func TestStopEscalatesToKillingTheProcessGroup(t *testing.T) {
	d, dir := newDriver(t, "stubborn", Options{StopWait: 300 * time.Millisecond, TermWait: 300 * time.Millisecond})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p2", RunID: "r2", Token: "tok", Cwd: dir, HarnessRef: "sess-2"}); err != nil {
		t.Fatal(err)
	}
	child, _ := waitRecord(t, d, "p2", "probe_child")
	gc := int(child["pid"].(float64))

	ex, err := d.Stop(ctx, "p2")
	if err != nil || ex.Code != -1 || ex.Signal != "SIGKILL" {
		t.Fatalf("stop = %+v, %v; want SIGKILL after stdin close and SIGTERM were ignored", ex, err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(gc, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			syscall.Kill(gc, syscall.SIGKILL)
			t.Fatalf("grandchild %d in the worker's process group survived stop", gc)
		}
	}
}

func TestInspectAndKillVerified(t *testing.T) {
	d, dir := newDriver(t, "replay", Options{TermWait: 2 * time.Second})
	var signals []syscall.Signal
	d.kill = func(pid int, sig syscall.Signal) error { signals = append(signals, sig); return syscall.Kill(pid, sig) }
	ctx := context.Background()
	proc, err := d.Start(ctx, core.Spec{ParticipantID: "p4", RunID: "r4", Token: "tok", Cwd: dir, HarnessRef: "sess-4"})
	if err != nil {
		t.Fatal(err)
	}
	waitRecord(t, d, "p4", "probe_env")
	if st, err := d.Inspect(ctx, proc); err != nil || st != core.ProcOurs {
		t.Fatalf("inspect live worker = %q, %v; want ours", st, err)
	}

	// Same pid, different start time: another process now owns the pid. Never signal it.
	reused := proc
	reused.StartTime -= 1000
	if st, err := d.Inspect(ctx, reused); err != nil || st != core.ProcReused {
		t.Fatalf("inspect reused pid = %q, %v", st, err)
	}
	if _, err := d.KillVerified(ctx, reused); err != nil || len(signals) != 0 {
		t.Fatalf("kill of a reused pid: %v, signals %v; want no signal", err, signals)
	}
	if st, _ := d.Inspect(ctx, proc); st != core.ProcOurs {
		t.Fatalf("worker after refusing to kill a reused pid = %q; want still alive", st)
	}

	ex, err := d.KillVerified(ctx, proc)
	if err != nil || ex.Signal != "SIGTERM" || !slices.Equal(signals, []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("kill verified = %+v, %v, signals %v", ex, err, signals)
	}
	if st, err := d.Inspect(ctx, proc); err != nil || st != core.ProcDead {
		t.Fatalf("inspect killed worker = %q, %v; want dead", st, err)
	}
}

// A declared thinking level reaches pi as --thinking, and the worker runs only if pi runs that
// level (get_state); a live set_thinking_level pi does not keep is refused and the level it
// ran before is set again.
func TestThinkingLevelIsChecked(t *testing.T) {
	d, dir := newDriver(t, "rpc", Options{})
	ctx := context.Background()
	spec := func(id, level string) core.Spec {
		return core.Spec{ParticipantID: id, RunID: "r-" + id, Token: "tok", Cwd: dir, HarnessRef: "sess-" + id, Thinking: level}
	}
	if _, err := d.Start(ctx, spec("p5", "bogus")); err == nil || !strings.Contains(err.Error(), `pi ran "high", not "bogus"`) {
		t.Fatalf("start with a level pi does not run: %v", err)
	}
	if err := d.Abort("p5"); !errors.Is(err, core.ErrNotRunning) {
		t.Fatalf("the refused worker is still running: %v", err)
	}
	proc, err := d.Start(ctx, spec("p6", "max"))
	if err != nil || !slices.Contains(proc.Cmdline, "--thinking") || !slices.Contains(proc.Cmdline, "max") {
		t.Fatalf("start with max = %v, %v", proc.Cmdline, err)
	}
	if err := d.SetThinking(ctx, "p6", "low"); err != nil {
		t.Fatalf("set low: %v", err)
	}
	if err := d.SetThinking(ctx, "p6", "bogus"); err == nil || !strings.Contains(err.Error(), `pi ran "high", not "bogus"`) {
		t.Fatalf("set bogus: %v; want refused", err)
	}
	w, _ := d.live("p6")
	if ran, err := w.thinking(ctx, time.Second); err != nil || ran != "low" {
		t.Fatalf("after the refused set pi runs %q, %v; want low back", ran, err)
	}
	d.Stop(ctx, "p6")
}

// abort and set_model each write one JSON line on stdin (under the stdin lock); set_model
// waits for pi's response by id and reports a refusal.
func TestAbortAndSetModel(t *testing.T) {
	d, dir := newDriver(t, "rpc", Options{})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p3", RunID: "r3", Token: "tok", Cwd: dir, HarnessRef: "sess-3"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Abort("p3"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetModel(ctx, "p3", "HP/glm-5.3-flash"); err != nil {
		t.Fatalf("set_model accepted by pi: %v", err)
	}
	if err := d.SetModel(ctx, "p3", "HP/bad"); err == nil || !strings.Contains(err.Error(), "Model not found") {
		t.Fatalf("set_model refused by pi: %v; want the refusal reported", err)
	}
	recs, _ := d.Tail("p3", 0)
	var lines []map[string]any
	for _, r := range recs {
		var m struct{ Type, Line string }
		json.Unmarshal(r, &m)
		if m.Type == "probe_in" {
			var cmd map[string]any
			if err := json.Unmarshal([]byte(m.Line), &cmd); err != nil {
				t.Fatalf("stdin line is not one JSON object: %q", m.Line)
			}
			lines = append(lines, cmd)
		}
	}
	if len(lines) != 3 || lines[0]["type"] != "abort" || lines[1]["type"] != "set_model" || lines[1]["provider"] != "HP" ||
		lines[1]["modelId"] != "glm-5.3-flash" || lines[1]["id"] == "" {
		t.Fatalf("stdin lines = %v", lines)
	}
	d.Stop(ctx, "p3")
	if err := d.Abort("p3"); !errors.Is(err, core.ErrNotRunning) {
		t.Fatalf("abort after exit: %v; want ErrNotRunning", err)
	}
}

// Kill ends a group that ignores stdin EOF and SIGTERM at once, grandchild included.
func TestKillIsImmediate(t *testing.T) {
	d, dir := newDriver(t, "stubborn", Options{StopWait: time.Minute, TermWait: time.Minute})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p4", RunID: "r4", Token: "tok", Cwd: dir, HarnessRef: "sess-4"}); err != nil {
		t.Fatal(err)
	}
	child, _ := waitRecord(t, d, "p4", "probe_child")
	gc := int(child["pid"].(float64))
	start := time.Now()
	ex, err := d.Kill(ctx, "p4")
	if err != nil || ex.Signal != "SIGKILL" || time.Since(start) > 5*time.Second {
		t.Fatalf("kill = %+v, %v after %s; want SIGKILL at once", ex, err, time.Since(start))
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(gc, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			syscall.Kill(gc, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived kill", gc)
		}
	}
}
