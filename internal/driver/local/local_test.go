package local

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
//   - stubborn: ignore stdin EOF and SIGTERM, start a grandchild in the same process group and
//     another in a process group of its own (as pi's bash tool does for each command).
//   - polite: on SIGTERM exit 0 at once (as pi does), leaving a grandchild in a process group of
//     its own for the driver to sweep.
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
	if strings.HasPrefix(mode, "omp") {
		ompHelper(mode)
	}
	switch mode {
	case "sleep":
		time.Sleep(time.Hour)
	case "argv": // the arguments after "--", as JSON, to the file PGDRV_OUT (Windows tests)
		b, _ := json.Marshal(os.Args[slices.Index(os.Args, "--")+1:])
		os.WriteFile(os.Getenv("PGDRV_OUT"), b, 0o600)
		os.Exit(0)
	case "exit259": // STILL_ACTIVE as an exit code (Windows tests)
		os.Exit(259)
	case "replay":
		var leaked []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "PIGGERY_") && !strings.HasPrefix(kv, "PIGGERY_ID=") && !strings.HasPrefix(kv, "PIGGERY_TOKEN=") &&
				!strings.HasPrefix(kv, "PIGGERY_RUN_ID=") {
				leaked = append(leaked, kv)
			}
		}
		emit(map[string]any{"type": "probe_env", "id": os.Getenv("PIGGERY_ID"), "pid": os.Getpid(), "token": os.Getenv("PIGGERY_TOKEN"), "run": os.Getenv("PIGGERY_RUN_ID"), "leaked": leaked, "args": os.Args,
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
			case "get_available_models": // pi's own answer (pi 0.99.1), under this request's id
				b, _ := os.ReadFile(os.Getenv("PGDRV_PI_MODELS"))
				var resp map[string]any
				json.Unmarshal(b, &resp)
				resp["id"] = cmd.ID
				emit(resp)
			}
		}
		os.Exit(0)
	case "polite":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		own := sleeper()
		isolate(own)
		if err := own.Start(); err != nil {
			os.Exit(3)
		}
		emit(map[string]any{"type": "probe_child", "pid": own.Process.Pid, "own": own.Process.Pid})
		<-term
		os.Exit(0)
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		child := sleeper()
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		own := sleeper()
		isolate(own)
		if err := own.Start(); err != nil {
			os.Exit(3)
		}
		emit(map[string]any{"type": "probe_child", "pid": child.Process.Pid, "own": own.Process.Pid})
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
	setHome(t, t.TempDir())           // no human pi setup: the worker agent dir is built from nothing
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

func TestStopEscalatesToKillingTheWholeTree(t *testing.T) {
	d, dir := newDriver(t, "stubborn", Options{StopWait: 300 * time.Millisecond, TermWait: 300 * time.Millisecond})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p2", RunID: "r2", Token: "tok", Cwd: dir, HarnessRef: "sess-2"}); err != nil {
		t.Fatal(err)
	}
	child, _ := waitRecord(t, d, "p2", "probe_child")

	ex, err := d.Stop(ctx, "p2")
	if err != nil || ex.Code != -1 || ex.Signal != "SIGKILL" {
		t.Fatalf("stop = %+v, %v; want SIGKILL after stdin close and SIGTERM were ignored", ex, err)
	}
	expectGone(t, child, "stop")
}

// expectGone fails unless both grandchildren of the stubborn helper (in the worker's group, and
// in a group of their own) are dead soon after what ended the worker.
func expectGone(t *testing.T, child map[string]any, what string) {
	t.Helper()
	for _, k := range []string{"pid", "own"} {
		pid := int(child[k].(float64))
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if !procAlive(pid) {
				break
			}
			if time.Now().After(deadline) {
				killPID(pid)
				t.Fatalf("grandchild %d (%s) survived %s", pid, k, what)
			}
		}
	}
}

func TestInspectAndKillVerified(t *testing.T) {
	d, dir := newDriver(t, "replay", Options{TermWait: 2 * time.Second})
	var signals []syscall.Signal
	d.kill = func(pid int, sig syscall.Signal) error { signals = append(signals, sig); return newKill()(pid, sig) }
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
	switch {
	case err != nil:
		t.Fatalf("kill verified = %+v, %v, signals %v", ex, err, signals)
	case hasTerm && (ex.Signal != "SIGTERM" || !slices.Equal(signals, []syscall.Signal{syscall.SIGTERM})):
		t.Fatalf("kill verified = %+v, signals %v; want SIGTERM alone", ex, signals)
	case !hasTerm && (ex.Signal != "SIGKILL" || len(signals) == 0 || slices.ContainsFunc(signals, func(s syscall.Signal) bool { return s != syscall.SIGKILL })):
		t.Fatalf("kill verified = %+v, signals %v; want SIGKILL alone (Windows has no SIGTERM)", ex, signals)
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

// get_available_models is listed as provider/id, the form set_model takes, in pi's order; the
// driver declares the capability; a worker that is not running has no list.
func TestModelsAreListed(t *testing.T) {
	d, dir := newDriver(t, "rpc", Options{})
	ctx := context.Background()
	capture, err := filepath.Abs("../../../testdata/fixtures/pi-0.99.1-get-available-models.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGDRV_PI_MODELS", capture)
	if !slices.Contains(d.Capabilities(), core.CapListModels) {
		t.Fatal("pi's driver does not declare list_models")
	}
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p8", RunID: "r8", Token: "tok", Cwd: dir, HarnessRef: "sess-8"}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Models(ctx, "p8"); err != nil || !slices.Equal(got, []string{"example/glm-5.3-flash", "example/glm-5.3-pro"}) {
		t.Fatalf("models = %v, %v", got, err)
	}
	d.Stop(ctx, "p8")
	if _, err := d.Models(ctx, "p8"); !errors.Is(err, core.ErrNotRunning) {
		t.Fatalf("models of a stopped worker: %v; want ErrNotRunning", err)
	}
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

// Kill asks with SIGTERM: a worker that exits on it returns at once, well before the grace, with
// its own exit (not SIGKILL's), and what it left behind is swept.
func TestKillAsksBeforeForcing(t *testing.T) {
	if !hasTerm {
		t.Skip("Windows has no SIGTERM: a kill there is the job's terminate, nothing to ask first")
	}
	d, dir := newDriver(t, "polite", Options{KillWait: time.Minute})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p7", RunID: "r7", Token: "tok", Cwd: dir, HarnessRef: "sess-7"}); err != nil {
		t.Fatal(err)
	}
	child, _ := waitRecord(t, d, "p7", "probe_child")
	start := time.Now()
	ex, err := d.Kill(ctx, "p7")
	if err != nil || ex.Code != 0 || time.Since(start) > 10*time.Second {
		t.Fatalf("kill = %+v, %v after %s; want the worker's own exit soon after SIGTERM", ex, err, time.Since(start))
	}
	expectGone(t, child, "kill")
}

// A worker that ignores SIGTERM is SIGKILLed after the grace, its grandchildren included, even
// one in a process group of its own.
func TestKillForcesAfterTheGrace(t *testing.T) {
	d, dir := newDriver(t, "stubborn", Options{StopWait: time.Minute, TermWait: time.Minute, KillWait: 300 * time.Millisecond})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p4", RunID: "r4", Token: "tok", Cwd: dir, HarnessRef: "sess-4"}); err != nil {
		t.Fatal(err)
	}
	child, _ := waitRecord(t, d, "p4", "probe_child")
	start := time.Now()
	ex, err := d.Kill(ctx, "p4")
	if err != nil || ex.Signal != "SIGKILL" || time.Since(start) > 5*time.Second {
		t.Fatalf("kill = %+v, %v after %s; want SIGKILL after the grace", ex, err, time.Since(start))
	}
	expectGone(t, child, "kill")
}

// A worker that has stopped reading its stdin does not hang whoever writes to it: the write is
// given up on after sendWait, later writes fail at once, and the worker can still be killed (on
// Windows a pipe takes no deadline, and the write given up on is still in the pipe).
func TestSendToAWorkerThatDoesNotReadGivesUp(t *testing.T) {
	d, dir := newDriver(t, "stubborn", Options{KillWait: 300 * time.Millisecond})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p8", RunID: "r8", Token: "tok", Cwd: dir, HarnessRef: "sess-8"}); err != nil {
		t.Fatal(err)
	}
	child, _ := waitRecord(t, d, "p8", "probe_child")
	w, err := d.live("p8")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = w.send(strings.Repeat("x", 1<<20)) // more than any pipe buffers
	if err == nil || !strings.Contains(err.Error(), "not read within") || time.Since(start) > sendWait+5*time.Second {
		t.Fatalf("send = %v after %s; want it given up on after %s", err, time.Since(start), sendWait)
	}
	start = time.Now()
	if err := d.Abort("p8"); err == nil || !strings.Contains(err.Error(), "broken") || time.Since(start) > time.Second {
		t.Fatalf("send after a write given up on = %v after %s; want it refused at once", err, time.Since(start))
	}
	killed := make(chan error, 1)
	go func() {
		_, err := d.Kill(ctx, "p8")
		killed <- err
	}()
	select {
	case err := <-killed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("kill hangs behind the write given up on")
	}
	expectGone(t, child, "kill")
}
