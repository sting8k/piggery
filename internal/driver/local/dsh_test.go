package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

const dshFixture = "../../../testdata/fixtures/dsh/0.2.0-rc.1/worker-stdout.jsonl"

// TestDshHelperProcess is the fake `dsh --profile sdk` (not a test), PGDRV_DSH selects its behaviour:
//   - ok: report argv and the environment as a probe record, replay a real worker's stdout (the
//     plugin's ready, records and results between dsh's own frames), then report every stdin
//     line and answer piggery/set_model by id (a model "bad/…" refused).
//   - fail: what the plugin does when its agent cannot start: the error as a record, exit 1.
//   - silent: never says ready.
func TestDshHelperProcess(t *testing.T) {
	mode := os.Getenv("PGDRV_DSH")
	if mode == "" {
		return
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	rec := func(v any) { emit(map[string]any{"jsonrpc": "2.0", "method": "piggery/record", "params": v}) }
	switch mode {
	case "fail":
		rec(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "dsh worker did not start: provider \"hp\" model \"m\" does not support reasoning effort \"bogus\""}})
		os.Exit(1)
	case "silent":
		bufio.NewScanner(os.Stdin).Scan()
		os.Exit(0)
	}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "PIGGERY_") {
			env[k] = v
		}
	}
	rec(map[string]any{"type": "probe_env", "args": os.Args, "env": env})
	b, err := os.ReadFile(os.Getenv("PGDRV_FIXTURE"))
	if err != nil {
		os.Exit(3)
	}
	os.Stdout.Write(b)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		rec(map[string]any{"type": "probe_in", "line": in.Text()})
		var m struct {
			Method string
			Params struct{ ID, Model string }
		}
		json.Unmarshal(in.Bytes(), &m)
		if m.Method == "piggery/models" {
			emit(map[string]any{"jsonrpc": "2.0", "method": "piggery/result", "params": map[string]any{"id": m.Params.ID, "ok": true, "models": []string{"hp/glm-5.3-flash", "deepseek-official/deepseek-flash"}}})
		}
		if m.Method == "piggery/set_model" {
			ok := !strings.HasPrefix(m.Params.Model, "bad/")
			emit(map[string]any{"jsonrpc": "2.0", "method": "piggery/result", "params": map[string]any{"id": m.Params.ID, "ok": ok, "error": map[bool]string{false: "no such model"}[ok]}})
		}
	}
	os.Exit(0)
}

func newDshDriver(t *testing.T, mode string, prof DshProfile, opts ...Options) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	prof.Cmd = useFakeHarness(t, "TestDshHelperProcess")
	b, _ := json.Marshal(prof)
	os.MkdirAll(filepath.Dir(DshProfilePath(dir)), 0o700)
	if err := os.WriteFile(DshProfilePath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(dshFixture)
	t.Setenv("PGDRV_DSH", mode)
	t.Setenv("PGDRV_FIXTURE", abs)
	return NewDsh(dir, append(opts, Options{})[0]), dir
}

// A spawn runs `dsh <args> --profile sdk --patch <overlay>` with the worker's session, model and
// level, the native tools cut down by the profile minus the role's allow_tools, and resume marked;
// the overlay turns the DeepSeek session-log upload off, disables the blacklisted rows and loads
// piggery's plugin from a copy of the binary's. Start returns once the plugin says ready, and the
// log holds the plugin's records only (dsh's own frames are not kept).
func TestDshStart(t *testing.T) {
	d, dir := newDshDriver(t, "ok", DshProfile{Args: []string{"-y", "dsh@x"}, Model: "hp/m", Thinking: "inherit",
		DisabledTools: []string{"subagent", "workflow", "send_message"}, Blacklist: []string{"mcp-memory"}, Env: []string{"FOO=bar"}})
	ctx := context.Background()
	spec := core.Spec{ParticipantID: "p1", RunID: "r1", Token: "tok", Cwd: dir, HarnessRef: "sess-1", Thinking: "high", Resume: true, AllowTools: []string{"workflow"}}
	if _, err := d.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Stop(ctx, "p1") })
	env, recs := waitRecord(t, d, "p1", "probe_env")
	var args []string
	for _, a := range env["args"].([]any) {
		args = append(args, a.(string))
	}
	patch := DshWorkerPatchPath(dir)
	if got := args[slices.Index(args, "--")+1:]; !slices.Equal(got, []string{"-y", "dsh@x", "--profile", "sdk", "--patch", patch}) {
		t.Fatalf("args %q", got)
	}
	e := env["env"].(map[string]any)
	for k, want := range map[string]string{"PIGGERY_ID": "p1", "PIGGERY_RUN_ID": "r1", "PIGGERY_DSH_SESSION": "sess-1", "PIGGERY_DSH_MODEL": "hp/m",
		"PIGGERY_DSH_THINKING": "high", "PIGGERY_DSH_DENY": "subagent,send_message", "PIGGERY_DSH_RESUME": "1"} {
		if e[k] != want {
			t.Errorf("%s = %v, want %q", k, e[k], want)
		}
	}
	ov, _ := os.ReadFile(patch)
	for _, want := range []string{"- id: session-log-deepseek\n  config:\n    enabled: false", "- id: 'mcp-memory'\n  disabled: true", "name: '" + DshEntry(dir) + "'"} {
		if !strings.Contains(string(ov), want) {
			t.Errorf("overlay lacks %q:\n%s", want, ov)
		}
	}
	if v, ok := DshExtVersion(DshExtDir(dir)); !ok || v != IntegrationVersion("dsh") {
		t.Errorf("plugin copy version %d managed %v", v, ok)
	}
	// The real run's stdout: records only, no dsh frames.
	for _, r := range recs {
		if bytes.Contains(r, []byte("session.")) || bytes.Contains(r, []byte("jsonrpc")) {
			t.Fatalf("a dsh frame is in the log: %s", r)
		}
	}
	waitRecord(t, d, "p1", "agent_end")
	var ends, texts int
	// The fixture is read line by line: wait for its last agent_end, not the first.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		recs, _ = d.Tail("p1", 0)
		if ends = countType(recs, "agent_end"); ends >= 3 || time.Now().After(deadline) {
			break
		}
	}
	ends = 0
	for _, r := range recs {
		var m struct {
			Type    string
			Message struct {
				Role    string
				Content []struct{ Text string }
				Usage   struct{ TotalTokens int }
			}
		}
		json.Unmarshal(r, &m)
		switch {
		case m.Type == "agent_end":
			ends++
		case m.Type == "message_end" && m.Message.Role == "assistant" && len(m.Message.Content) > 0 && m.Message.Usage.TotalTokens > 0:
			texts++
		}
	}
	if ends != 3 || texts < 2 {
		t.Errorf("records: %d agent_end, %d assistant messages with usage", ends, texts)
	}
}

// A dsh that ends before its worker is up (a level it does not run, no agent) fails the start with
// the plugin's own reason; one that says nothing fails it at dshStartWait; nothing stays running.
func TestDshStartFails(t *testing.T) {
	d, dir := newDshDriver(t, "fail", DshProfile{})
	_, err := d.Start(context.Background(), core.Spec{ParticipantID: "p1", RunID: "r1", Cwd: dir, HarnessRef: "s", Thinking: "bogus"})
	if err == nil || !strings.Contains(err.Error(), `does not support reasoning effort "bogus"`) {
		t.Fatalf("err = %v", err)
	}
	old := dshStartWait
	dshStartWait = 300 * time.Millisecond
	t.Cleanup(func() { dshStartWait = old })
	t.Setenv("PGDRV_DSH", "silent")
	if _, err := d.Start(context.Background(), core.Spec{ParticipantID: "p2", RunID: "r2", Cwd: dir, HarnessRef: "s"}); err == nil || !strings.Contains(err.Error(), "did not report its worker up") {
		t.Fatalf("silent: err = %v", err)
	}
	if _, err := d.live("p2"); err == nil {
		t.Error("a worker whose start failed is still running")
	}
}

// Abort is a notification on stdin (the sdk's reader drops it, the plugin acts); set_model waits
// for the plugin's result by id, and a refusal is an error with dsh's words.
func TestDshControl(t *testing.T) {
	d, dir := newDshDriver(t, "ok", DshProfile{})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p1", RunID: "r1", Cwd: dir, HarnessRef: "s"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Stop(ctx, "p1") })
	if err := d.Abort("p1"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetModel(ctx, "p1", "hp/glm-5.3"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetThinking(ctx, "p1", "high"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetModel(ctx, "p1", "bad/x"); err == nil || !strings.Contains(err.Error(), "dsh refused set_model: no such model") {
		t.Fatalf("refusal: %v", err)
	}
	if err := d.SetModel(ctx, "p1", "nomodel"); err == nil {
		t.Fatal("a model without a provider was sent")
	}
	waitRecord(t, d, "p1", "probe_in")
	recs, _ := d.Tail("p1", 0)
	var lines []string
	for _, r := range recs {
		var m struct{ Type, Line string }
		json.Unmarshal(r, &m)
		if m.Type == "probe_in" {
			lines = append(lines, m.Line)
		}
	}
	if len(lines) != 4 || lines[0] != `{"jsonrpc":"2.0","method":"piggery/abort"}` ||
		!strings.Contains(lines[1], `"model":"hp/glm-5.3"`) || !strings.Contains(lines[2], `"thinking":"high"`) || strings.Contains(lines[2], `"model"`) {
		t.Fatalf("stdin lines %q", lines)
	}
	if got, err := d.Models(ctx, "p1"); err != nil || !slices.Equal(got, []string{"hp/glm-5.3-flash", "deepseek-official/deepseek-flash"}) {
		t.Fatalf("models = %v, %v", got, err)
	}
}

// The plugin copy is written once and never while current, the overlay is rewritten each start, and a directory that is not piggery's is refused.
func TestDshWorkerCopy(t *testing.T) {
	dir := t.TempDir()
	patch, err := EnsureDshWorker(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := DshEntry(dir)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(entry, old, old)
	if _, err := EnsureDshWorker(dir, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(entry); fi.ModTime().After(old.Add(time.Minute)) {
		t.Error("a current copy was rewritten")
	}
	if b, _ := os.ReadFile(patch); !strings.Contains(string(b), "- id: 'x'") {
		t.Errorf("overlay not rewritten:\n%s", b)
	}
	if up, err := UpdateDshExt(DshExtDir(dir)); err != nil || up {
		t.Fatalf("update of a current copy: %v, %v", up, err)
	}
	other := t.TempDir()
	os.MkdirAll(DshExtDir(other), 0o755)
	os.WriteFile(filepath.Join(DshExtDir(other), "index.mjs"), []byte("// mine\n"), 0o644)
	if _, err := EnsureDshWorker(other, nil); err == nil {
		t.Error("a directory that is not piggery's was replaced")
	}
}

func countType(recs []json.RawMessage, typ string) int {
	n := 0
	for _, r := range recs {
		var m struct{ Type string }
		if json.Unmarshal(r, &m) == nil && m.Type == typ {
			n++
		}
	}
	return n
}
