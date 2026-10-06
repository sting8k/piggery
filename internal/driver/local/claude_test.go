package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

const claudeFixtures = "../../../testdata/fixtures/claude-2.1.283/"

// TestClaudeHelperProcess is the fake `claude -p` (not a test), replaying captured stdout:
// initialize answers with C0's response, a user message with C1's first turn (its
// command_lifecycle keyed by the message's uuid),
// interrupt with C3's receipt; set_model and apply_flag_settings succeed. Every stdin line and
// the argv are reported as probe records.
func TestClaudeHelperProcess(t *testing.T) {
	if os.Getenv("PGDRV_CLAUDE") == "" {
		return
	}
	dir := os.Getenv("PGDRV_FIXTURES")
	fixture := func(name string) [][]byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			os.Exit(3)
		}
		return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	// Mark the request id of a captured control_response as the one asked.
	answer := func(line []byte, id string) {
		var m map[string]any
		json.Unmarshal(line, &m)
		m["response"].(map[string]any)["request_id"] = id
		emit(m)
	}
	emit(map[string]any{"type": "probe_env", "args": os.Args, "agent_view": os.Getenv("CLAUDE_CODE_DISABLE_AGENT_VIEW"),
		"id": os.Getenv("PIGGERY_ID")})
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(nil, 1<<20)
	for in.Scan() {
		emit(map[string]any{"type": "probe_in", "line": in.Text()})
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		json.Unmarshal(in.Bytes(), &msg)
		switch {
		case msg.Type == "user":
			var u struct{ UUID string }
			json.Unmarshal(in.Bytes(), &u)
			for _, l := range fixture("c1-stdout.jsonl")[:8] {
				os.Stdout.Write(append(bytes.ReplaceAll(l, []byte("aaaaaaaa-0000-4000-8000-000000000001"), []byte(u.UUID)), '\n'))
			}
			if bytes.Contains(in.Bytes(), []byte("BG")) { // the turn left a background task running
				emit(map[string]any{"type": "system", "subtype": "background_tasks_changed",
					"tasks": []any{map[string]any{"task_id": "bt1", "task_type": "local_bash"}}})
			}
		case msg.Request.Subtype == "initialize":
			answer(fixture("c0-stdout.jsonl")[0], msg.RequestID)
		case msg.Request.Subtype == "interrupt":
			answer(fixture("c3-stdout.jsonl")[8], msg.RequestID)
		default:
			emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": msg.RequestID}})
		}
	}
	os.Exit(0)
}

func newClaudeDriver(t *testing.T, prof ClaudeProfile, opts ...Options) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	// Claude's flags come first: a wrapper puts the test binary's own flags before them.
	prof.Cmd = useFakeHarness(t, "TestClaudeHelperProcess")
	b, _ := json.Marshal(prof)
	os.MkdirAll(filepath.Dir(ClaudeProfilePath(dir)), 0o700)
	if err := os.WriteFile(ClaudeProfilePath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(claudeFixtures)
	t.Setenv("PGDRV_CLAUDE", "1")
	t.Setenv("PGDRV_FIXTURES", abs)
	return NewClaude(dir, "/opt/piggery", append(opts, Options{})[0]), dir
}

// probeIn returns the JSON lines the worker read on stdin.
func probeIn(t *testing.T, d *Driver, pid string) []map[string]any {
	t.Helper()
	recs, _ := d.Tail(pid, 0)
	var out []map[string]any
	for _, r := range recs {
		var m struct{ Type, Line string }
		json.Unmarshal(r, &m)
		if m.Type == "probe_in" {
			var line map[string]any
			if err := json.Unmarshal([]byte(m.Line), &line); err != nil {
				t.Fatalf("stdin line is not one JSON object: %q", m.Line)
			}
			out = append(out, line)
		}
	}
	return out
}

// A spawn runs claude -p stream-json with the worker's session, model, checked effort, role
// card plus the channel note, piggery's MCP server (no piggery hooks), and the native tools cut
// down; stdin carries only initialize until core delivers a batch: one user message with a uuid
// and no priority, whose command_lifecycle completed ends that batch once. Tail keeps stdout.
func TestClaudeStart(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"mine":{"command":"m"},"old":{"command":"o"}},
		"projects":{"/x":{"mcpServers":{"proj":{"command":"p"}}}}}`), 0o600)
	ended := make(chan string, 4)
	d, dir := newClaudeDriver(t, ClaudeProfile{DisallowedTools: []string{"Agent", "SendMessage", "AskUserQuestion"},
		Blacklist: []string{"old", "noisy@market"}}, Options{OnDelivery: func(pid, run string, batch int64, outcome string) {
		ended <- fmt.Sprintf("%s %s %d %s", pid, run, batch, outcome)
	}})
	ctx := context.Background()
	spec := core.Spec{ParticipantID: "p1", RunID: "r1", Token: "tok", Cwd: dir, HarnessRef: "sess-1",
		Model: "sonnet", Thinking: "xhigh", RoleCard: "You are w1.", AllowTools: []string{"Agent"}}
	proc, err := d.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := waitRecord(t, d, "p1", "probe_env")
	var args []string
	for _, a := range env["args"].([]any) {
		args = append(args, a.(string))
	}
	args = args[slices.Index(args, "--")+1:]
	flag := func(name string) string {
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("args %q: no %s", args, name)
		}
		return args[i+1]
	}
	if args[0] != "-p" || flag("--input-format") != "stream-json" || flag("--output-format") != "stream-json" ||
		!slices.Contains(args, "--verbose") || !slices.Contains(args, "--strict-mcp-config") || flag("--session-id") != "sess-1" || flag("--model") != "sonnet" ||
		flag("--effort") != "xhigh" || flag("--permission-mode") != "bypassPermissions" || slices.Contains(args, "--tools") ||
		flag("--setting-sources") != "user" {
		t.Fatalf("args = %q", args)
	}
	if dis := flag("--disallowedTools"); dis != "SendMessage AskUserQuestion" { // the profile's list minus the role's allow_tools
		t.Fatalf("--disallowedTools %q", dis)
	}
	if sp := flag("--append-system-prompt"); !strings.HasPrefix(sp, "You are w1.") || !strings.Contains(sp, "mcp__piggery__inbox") {
		t.Fatalf("system prompt %q: want the role card, then the piggery channel", sp)
	}
	var mcp struct {
		McpServers map[string]struct {
			Command string
			Args    []string
		}
	}
	json.Unmarshal([]byte(flag("--mcp-config")), &mcp)
	if srv := mcp.McpServers["piggery"]; srv.Command != "/opt/piggery" || !slices.Equal(srv.Args, []string{"mcp"}) ||
		mcp.McpServers["mine"].Command != "m" || len(mcp.McpServers) != 2 { // the Human's, minus "old"; no project server
		t.Fatalf("--mcp-config %s", flag("--mcp-config"))
	}
	settings, err := os.ReadFile(flag("--settings"))
	if err != nil || bytes.Contains(settings, []byte(`hook claude`)) || // the codec delivers mail: no piggery hooks
		!bytes.Contains(settings, []byte(`"noisy@market": false`)) || !bytes.Contains(settings, []byte(`"piggery@piggery": false`)) ||
		bytes.Contains(settings, []byte(`"old"`)) {
		t.Fatalf("settings %s, %v: want no piggery hooks, the blacklisted plugin and piggery's session plugin off", settings, err)
	}
	if env["agent_view"] != "1" || env["id"] != "p1" {
		t.Fatalf("env = %v", env)
	}
	if filepath.Base(proc.Cmdline[0]) != filepath.Base(os.Args[0]) { // the fake claude is this test binary
		t.Fatalf("cmdline %v", proc.Cmdline)
	}

	if in := probeIn(t, d, "p1"); len(in) != 1 || in[0]["request"].(map[string]any)["subtype"] != "initialize" {
		t.Fatalf("stdin after start = %v; want initialize only (core delivers the task)", in)
	}
	if err := d.Deliver("p1", core.Delivery{RunID: "r1", Batch: 1, Text: "[piggery] 1 new message"}); err != nil {
		t.Fatal(err)
	}
	waitRecord(t, d, "p1", "result")
	in := probeIn(t, d, "p1")
	if len(in) != 2 || in[1]["type"] != "user" || in[1]["uuid"] == "" || in[1]["parent_tool_use_id"] != nil || in[1]["priority"] != nil ||
		!strings.Contains(fmt.Sprint(in[1]["message"]), "[piggery] 1 new message") {
		t.Fatalf("stdin = %v; want initialize, then the batch as a user message with a uuid and no priority", in)
	}
	select {
	case e := <-ended:
		if e != "p1 r1 1 completed" {
			t.Fatalf("delivery ended %q", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery end after the batch's command_lifecycle completed")
	}
	if len(ended) != 0 {
		t.Fatalf("a second end: %q", <-ended)
	}
	d.Stop(ctx, "p1")
	if _, err := os.Stat(filepath.Dir(flag("--settings"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run settings after the run: %v; want removed", err)
	}

	spec.RunID, spec.Resume = "r2", true
	if _, err := d.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	env, _ = waitRecord(t, d, "p1", "probe_env")
	if !slices.Contains(env["args"].([]any), any("--resume")) || slices.Contains(env["args"].([]any), any("--session-id")) {
		t.Fatalf("resume args = %v", env["args"])
	}
	d.Stop(ctx, "p1")
}

// Claude runs a lower effort than asked without saying: the driver checks the level
// against initialize's supportedEffortLevels, at start and when set live, and refuses instead.
func TestClaudeEffortIsChecked(t *testing.T) {
	d, dir := newClaudeDriver(t, ClaudeProfile{})
	ctx := context.Background()
	spec := func(id, model, effort string) core.Spec {
		return core.Spec{ParticipantID: id, RunID: "r-" + id, Cwd: dir, HarnessRef: "s-" + id, Model: model, Thinking: effort}
	}
	if _, err := d.Start(ctx, spec("p2", "haiku", "low")); err == nil || !strings.Contains(err.Error(), "takes no effort") {
		t.Fatalf("haiku with effort: %v", err)
	}
	if err := d.Abort("p2"); !errors.Is(err, core.ErrNotRunning) {
		t.Fatalf("refused worker still running: %v", err)
	}
	if _, err := d.Start(ctx, spec("p3", "claude-opus-4-6", "xhigh")); err == nil || !strings.Contains(err.Error(), "not \"xhigh\"") {
		t.Fatalf("opus 4.6 with xhigh: %v", err)
	}
	if _, err := d.Start(ctx, spec("p4", "claude-opus-4-6", "max")); err != nil {
		t.Fatal(err)
	}
	if err := d.SetThinking(ctx, "p4", "xhigh"); err == nil {
		t.Fatal("set xhigh on opus 4.6: want refused")
	}
	// The list is what initialize gave (no control request of its own): the values set_model takes.
	if got, err := d.Models(ctx, "p4"); err != nil || got[0] != "default" || !slices.Contains(got, "claude-opus-5") {
		t.Fatalf("models = %v, %v", got, err)
	}
	if err := d.SetModel(ctx, "p4", "claude-opus-5"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetThinking(ctx, "p4", "xhigh"); err != nil {
		t.Fatalf("xhigh after switching to opus 5: %v", err)
	}
	var subs []string
	for _, l := range probeIn(t, d, "p4") {
		if r, ok := l["request"].(map[string]any); ok {
			subs = append(subs, r["subtype"].(string))
		}
	}
	if !slices.Equal(subs, []string{"initialize", "set_model", "apply_flag_settings"}) {
		t.Fatalf("control requests = %v; the refused level must not reach claude", subs)
	}
	d.Stop(ctx, "p4")
}

// Abort stops the background tasks left running, then interrupts the turn and cancels what is
// queued behind it, so the worker does not wake itself afterwards (C3; seen live: a background
// task that ended after an abort opened a turn).
func TestClaudeAbortCancelsQueued(t *testing.T) {
	d, dir := newClaudeDriver(t, ClaudeProfile{})
	ctx := context.Background()
	if _, err := d.Start(ctx, core.Spec{ParticipantID: "p5", RunID: "r5", Cwd: dir, HarnessRef: "s5"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Deliver("p5", core.Delivery{RunID: "r5", Batch: 1, Text: "BG"}); err != nil {
		t.Fatal(err)
	}
	waitRecord(t, d, "p5", "result")
	w, _ := d.live("p5")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		r := d.codec.(*claudeCodec).run(w)
		r.mu.Lock()
		n := len(r.bgTasks)
		r.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background task was not seen")
		}
	}
	if err := d.Abort("p5"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		recs, _ := d.Tail("p5", 1)
		if len(recs) == 1 && bytes.Contains(recs[0], []byte(`"cancelled"`)) {
			break // the interrupt receipt: claude read the interrupt
		}
		if time.Now().After(deadline) {
			t.Fatal("no interrupt receipt")
		}
	}
	in := probeIn(t, d, "p5")
	stop, last := in[len(in)-2]["request"].(map[string]any), in[len(in)-1]["request"].(map[string]any)
	if stop["subtype"] != "stop_task" || stop["task_id"] != "bt1" || last["subtype"] != "interrupt" || last["cancel_queued"] != true {
		t.Fatalf("abort sent %v, then %v; want stop_task bt1, then interrupt cancel_queued", stop, last)
	}
	d.Stop(ctx, "p5")
}

// Tail and top read standard records only: replaying captured Claude stdout,
// the codec gives the tool calls with their results, the context of the last assistant
// message (per-message usage, never the cumulative totals), one turn_end per model call and
// no line for an empty thinking block.
func TestClaudeStandardRecords(t *testing.T) {
	std := func(name string) []map[string]any {
		t.Helper()
		b, err := os.ReadFile(claudeFixtures + name)
		if err != nil {
			t.Fatal(err)
		}
		run := &claudeRun{tools: map[string]string{}}
		var out []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
			for _, s := range claudeStandard(run, line) {
				var m map[string]any
				json.Unmarshal(s, &m)
				out = append(out, m)
			}
		}
		return out
	}
	count := func(recs []map[string]any, typ string) int {
		n := 0
		for _, r := range recs {
			if r["type"] == typ {
				n++
			}
		}
		return n
	}
	// C2: two sleeps in one turn of 3 model calls; the second message (B) was picked up.
	c2 := std("c2-stdout.jsonl")
	var tools []string
	var ctx float64
	for _, r := range c2 {
		switch r["type"] {
		case "tool_execution_start":
			tools = append(tools, r["toolName"].(string)+" "+r["args"].(map[string]any)["command"].(string))
		case "tool_execution_end":
			if r["toolName"] != "bash" || r["isError"] != false {
				t.Fatalf("tool end %v", r)
			}
		case "message_end":
			m := r["message"].(map[string]any)
			if c, _ := m["content"].([]any); m["role"] == "assistant" && len(c) == 0 && m["usage"] == nil {
				t.Fatalf("empty assistant record without usage: %v", r)
			}
			if u, ok := m["usage"].(map[string]any); ok {
				ctx = u["input"].(float64) + u["output"].(float64) + u["cacheRead"].(float64) + u["cacheWrite"].(float64)
			}
		}
	}
	if !slices.Equal(tools, []string{"bash sleep 6", "bash sleep 6"}) || count(c2, "tool_execution_end") != 2 ||
		count(c2, "turn_end") != 3 || count(c2, "agent_end") != 1 || ctx == 0 {
		t.Fatalf("C2: tools %v, ends %d, turn_end %d, agent_end %d, ctx %v", tools, count(c2, "tool_execution_end"),
			count(c2, "turn_end"), count(c2, "agent_end"), ctx)
	}
	// C1: two turns of one call each; the second turn's context is its own message's usage.
	c1 := std("c1-stdout.jsonl")
	var last map[string]any
	for _, r := range c1 {
		if m, ok := r["message"].(map[string]any); ok && m["usage"] != nil {
			last = m["usage"].(map[string]any)
		}
	}
	if count(c1, "turn_end") != 2 || last["cacheRead"] != float64(17187) || last["cacheWrite"] != float64(54) {
		t.Fatalf("C1: turn_end %d, last usage %v", count(c1, "turn_end"), last)
	}
	// C3: the aborted turn shows as an error line.
	for _, r := range std("c3-stdout.jsonl") {
		if m, ok := r["message"].(map[string]any); ok && m["stopReason"] == "error" {
			return
		}
	}
	t.Fatal("C3: no error line for the aborted turn")
}

// Captured delivery (claude 2.1.283, sj-*: mail as user messages, A running `sleep`, B and D sent
// during or right after it): through record, each delivered batch completes or is cancelled
// exactly once, from its command_lifecycle, and no result line is taken for an outcome.
func TestClaudeBatchOutcomes(t *testing.T) {
	const a, b, d = "aaaaaaaa-0000-4000-8000-000000000001", "aaaaaaaa-0000-4000-8000-000000000002", "aaaaaaaa-0000-4000-8000-000000000003"
	for _, c := range []struct {
		fixture string
		want    []string // outcomes in stdout order
	}{
		{"sj-noreplay", []string{"1 completed"}},
		{"sj-none", []string{"2 completed", "1 completed"}}, // B folds into A's turn, done before its result
		{"sj-later", []string{"1 completed", "2 completed"}},
		{"sj-now", []string{"1 cancelled", "2 completed"}},   // priority now aborted A's tool (not what deliver sends)
		{"sj-abort", []string{"2 cancelled", "1 cancelled"}}, // interrupt cancel_queued true
		{"sj-race", []string{"1 completed", "2 completed", "3 completed"}},
		{"sj-bgstop-idle", []string{"1 completed"}}, // stop_task after the turn: no turn follows
		{"sj-bgstop-busy", []string{"1 cancelled"}},
		// the background task's end opens a turn of the harness's own: turn_started, turn_ended
		{"sj-bgdone", []string{"1 completed", "turn_started", "turn_ended"}},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(claudeFixtures + c.fixture + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			w := &worker{}
			codec := &claudeCodec{state: map[*worker]*claudeRun{w: {batches: map[string]int64{a: 1, b: 2, d: 3}, tools: map[string]string{}}}}
			var got []string
			results := 0
			for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
				if bytes.Contains(line, []byte(`"type":"result"`)) {
					results++
				}
				rec := codec.record(w, line)
				for _, e := range rec.ended {
					got = append(got, fmt.Sprintf("%d %s", e.Batch, e.Outcome))
				}
				if rec.turn != "" {
					got = append(got, rec.turn)
				}
				if rec.fatal != "" {
					t.Fatalf("fatal on 2.1.283: %s", rec.fatal)
				}
			}
			if !slices.Equal(got, c.want) || results == 0 {
				t.Fatalf("outcomes %v (results %d), want %v", got, results, c.want)
			}
		})
	}

	// A claude whose init lacks msg_lifecycle_v1: its open batch is cancelled (the mail is given
	// again), the run is fatal, and a later deliver fails without writing anything.
	w := &worker{}
	codec := &claudeCodec{state: map[*worker]*claudeRun{w: {batches: map[string]int64{a: 7}, tools: map[string]string{}}}}
	rec := codec.record(w, []byte(`{"type":"system","subtype":"init","claude_code_version":"2.1.100","capabilities":["interrupt_receipt_v1"]}`))
	if len(rec.ended) != 1 || rec.ended[0] != (core.DeliveryEnd{Batch: 7, Outcome: core.DeliveryCancelled}) || !strings.Contains(rec.fatal, "2.1.100 is too old") {
		t.Fatalf("too old: ended %v, fatal %q", rec.ended, rec.fatal)
	}
	if err := codec.deliver(w, core.Delivery{Batch: 8, Text: "x"}); err == nil || !strings.Contains(err.Error(), "msg_lifecycle_v1") {
		t.Fatalf("deliver to a too-old run: %v", err)
	}
}
