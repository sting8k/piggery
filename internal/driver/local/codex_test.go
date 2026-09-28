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

const codexFixtures = "../../../testdata/fixtures/codex-0.157.1/"

// TestCodexHelperProcess is the fake `codex app-server` (not a test): initialize and
// thread/start answer with the captured responses (w1), thread/resume with the thread asked
// for; turn/start answers and starts turn t-N, which runs until turn/interrupt completes it.
// argv and every stdin line are reported as probe records; probe_turn marks a started turn.
func TestCodexHelperProcess(t *testing.T) {
	if os.Getenv("PGDRV_CODEX") == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(os.Getenv("PGDRV_FIXTURES"), "w1-exec-hookprompt-interrupt.jsonl"))
	if err != nil {
		os.Exit(3)
	}
	w1 := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	answer := func(line []byte, id any) { // a captured response, for the id asked
		var m map[string]any
		json.Unmarshal(line, &m)
		m["id"] = id
		emit(m)
	}
	emit(map[string]any{"type": "probe_env", "args": os.Args, "id": os.Getenv("PIGGERY_ID")})
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(nil, 1<<20)
	turns, cur := 0, ""
	for in.Scan() {
		emit(map[string]any{"type": "probe_in", "line": in.Text()})
		var msg struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
			} `json:"params"`
		}
		json.Unmarshal(in.Bytes(), &msg)
		switch msg.Method {
		case "initialize":
			answer(w1[0], msg.ID)
		case "thread/start":
			answer(w1[1], msg.ID)
		case "thread/resume":
			emit(map[string]any{"id": msg.ID, "result": map[string]any{"thread": map[string]string{"id": msg.Params.ThreadID}}})
		case "turn/start":
			turns++
			cur = fmt.Sprintf("t-%d", turns)
			emit(map[string]any{"id": msg.ID, "result": map[string]any{"turn": map[string]string{"id": cur, "status": "inProgress"}}})
			emit(map[string]any{"method": "turn/started", "params": map[string]any{"turn": map[string]string{"id": cur}}})
			emit(map[string]any{"type": "probe_turn", "id": cur})
		case "turn/interrupt":
			emit(map[string]any{"id": msg.ID, "result": map[string]any{}})
			emit(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"id": cur, "status": "interrupted"}}})
			emit(map[string]any{"type": "probe_done", "id": cur})
		}
	}
	os.Exit(0)
}

func newCodexDriver(t *testing.T, prof CodexProfile) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "codex")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestCodexHelperProcess$' -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	prof.Cmd = wrapper
	b, _ := json.Marshal(prof)
	os.MkdirAll(filepath.Dir(CodexProfilePath(dir)), 0o700)
	if err := os.WriteFile(CodexProfilePath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(codexFixtures)
	t.Setenv("PGDRV_CODEX", "1")
	t.Setenv("PGDRV_FIXTURES", abs)
	return NewCodex(dir, "/opt/piggery", Options{}), dir
}

// methodsIn is the JSON-RPC methods the worker read on stdin, in order.
func methodsIn(t *testing.T, d *Driver, pid string) ([]string, []map[string]any) {
	var ms []string
	in := probeIn(t, d, pid)
	for _, l := range in {
		m, _ := l["method"].(string)
		ms = append(ms, m)
	}
	return ms, in
}

// A spawn runs app-server with the worker's rules (no approvals, piggery's MCP server with the
// worker's identity, the profile's tool groups off but the role's, blacklisted servers off),
// starts the thread with the role card (its id is the worker's harness_ref) and sends the first
// prompt with the model. A wake while a turn the driver started still runs sends nothing (the
// mail reaches that turn through its hooks); after the turn completes it starts the next one.
// A respawn resumes the thread.
func TestCodexStartAndWake(t *testing.T) {
	d, dir := newCodexDriver(t, CodexProfile{Blacklist: []string{"mine"},
		DisabledTools: map[string]string{"goals": "features.goals=false", "agents": "agents.enabled=false"}})
	ctx := context.Background()
	spec := core.Spec{ParticipantID: "p1", RunID: "r1", Token: "tok", Cwd: dir, Model: "gpt-x", Thinking: "high",
		RoleCard: "You are w1.", AllowTools: []string{"agents"}}
	proc, err := d.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if proc.HarnessRef != "01a0e5a0-499c-7593-bdf7-4ac50099542a" {
		t.Fatalf("harness ref %q; want the captured thread id", proc.HarnessRef)
	}
	env, _ := waitRecord(t, d, "p1", "probe_env")
	var args []string
	for _, a := range env["args"].([]any) {
		args = append(args, a.(string))
	}
	args = args[slices.Index(args, "--")+1:]
	for _, want := range []string{`approval_policy="never"`, `mcp_servers.piggery.command="/opt/piggery"`,
		`mcp_servers.piggery.env_vars=["PIGGERY_ID","PIGGERY_TOKEN","PIGGERY_RUN_ID"]`, "features.goals=false",
		"mcp_servers.mine.enabled=false"} {
		if !slices.Contains(args, want) {
			t.Fatalf("args %q: no %s", args, want)
		}
	}
	if args[0] != "app-server" || slices.Contains(args, "agents.enabled=false") {
		t.Fatalf("args %q: want app-server, and the role's allowed group kept", args)
	}

	waitRecord(t, d, "p1", "probe_turn")
	ms, in := methodsIn(t, d, "p1")
	if !slices.Equal(ms, []string{"initialize", "initialized", "thread/start", "turn/start"}) {
		t.Fatalf("stdin methods %v", ms)
	}
	start := in[2]["params"].(map[string]any)
	if card, _ := start["developerInstructions"].(string); !strings.HasPrefix(card, "You are w1.") ||
		!strings.Contains(card, "mcp__piggery__inbox") || start["cwd"] != dir {
		t.Fatalf("thread/start %v: want the cwd, the role card and the channel note", start)
	}
	if p := in[3]["params"].(map[string]any); p["model"] != "gpt-x" || p["effort"] != "high" {
		t.Fatalf("first turn/start %v", p)
	}

	if err := d.Wake("p1"); err != nil { // the first turn still runs: nothing sent
		t.Fatal(err)
	}
	d.Wake("p1")
	if err := d.Abort("p1"); err != nil {
		t.Fatal(err)
	}
	waitRecord(t, d, "p1", "probe_done")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if err := d.Wake("p1"); err != nil {
			t.Fatal(err)
		}
		d.Wake("p1")
		if ms, _ = methodsIn(t, d, "p1"); len(ms) >= 6 || time.Now().After(deadline) {
			break
		}
	}
	time.Sleep(100 * time.Millisecond) // a second turn/start would be on stdin by now
	ms, in = methodsIn(t, d, "p1")
	if !slices.Equal(ms, []string{"initialize", "initialized", "thread/start", "turn/start", "turn/interrupt", "turn/start"}) {
		t.Fatalf("stdin methods %v; want no turn/start while a turn runs, one after it completed", ms)
	}
	if intr := in[4]["params"].(map[string]any); intr["turnId"] != "t-1" {
		t.Fatalf("turn/interrupt %v", intr)
	}
	text := in[5]["params"].(map[string]any)["input"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "wake #1") {
		t.Fatalf("wake prompt %q", text)
	}
	d.Stop(ctx, "p1")

	spec.RunID, spec.Resume, spec.HarnessRef = "r2", true, "th-9"
	if proc, err = d.Start(ctx, spec); err != nil || proc.HarnessRef != "th-9" {
		t.Fatalf("resume: %v %v", proc.HarnessRef, err)
	}
	waitRecord(t, d, "p1", "probe_turn")
	if ms, in = methodsIn(t, d, "p1"); ms[2] != "thread/resume" || in[2]["params"].(map[string]any)["threadId"] != "th-9" {
		t.Fatalf("respawn stdin %v", in)
	}
	d.Stop(ctx, "p1")
}

// The captured stdout translates to the standard records tail and top read: shell commands and
// piggery's MCP tools as tool lines, agent text and the mail a Stop hook gave, one turn_end with
// the context per model call, agent_end per turn. A turn that failed (bad model name, no Stop
// hook) is named for the driver to close, with the model API's message.
func TestCodexStandardRecords(t *testing.T) {
	c := &codexCodec{}
	replay := func(name string) (std []map[string]any, failed []string) {
		w := &worker{}
		c.state = map[*worker]*codexRun{w: {commands: map[string]string{}}}
		b, err := os.ReadFile(filepath.Join(codexFixtures, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
			r := c.record(w, line)
			for _, s := range r.std {
				var m map[string]any
				json.Unmarshal(s, &m)
				std = append(std, m)
			}
			if r.failed != "" {
				failed = append(failed, r.failed)
			}
		}
		return std, failed
	}
	count := func(std []map[string]any, typ string) int {
		n := 0
		for _, m := range std {
			if m["type"] == typ {
				n++
			}
		}
		return n
	}
	find := func(std []map[string]any, f func(m map[string]any) bool) map[string]any {
		for _, m := range std {
			if f(m) {
				return m
			}
		}
		return nil
	}
	msg := func(m map[string]any) map[string]any { r, _ := m["message"].(map[string]any); return r }
	textOf := func(m map[string]any) string {
		b, _ := json.Marshal(msg(m)["content"])
		return string(b)
	}

	std, failed := replay("w1-exec-hookprompt-interrupt.jsonl")
	if s := find(std, func(m map[string]any) bool { return m["type"] == "tool_execution_start" }); s == nil ||
		s["toolName"] != "bash" || s["args"].(map[string]any)["command"] != "sleep 6" {
		t.Fatalf("shell command start %v", s)
	}
	if e := find(std, func(m map[string]any) bool { return m["type"] == "tool_execution_end" }); e == nil ||
		e["toolName"] != "bash" || e["isError"] != false {
		t.Fatalf("shell command end %v", e)
	}
	if find(std, func(m map[string]any) bool {
		return msg(m)["role"] == "user" && strings.Contains(textOf(m), "mail #2 from lead")
	}) == nil {
		t.Fatal("the mail a Stop hook gave is not shown")
	}
	if find(std, func(m map[string]any) bool {
		return msg(m)["role"] == "assistant" && strings.Contains(textOf(m), "four")
	}) == nil {
		t.Fatal("the agent's text is not shown")
	}
	u := find(std, func(m map[string]any) bool { return msg(m)["usage"] != nil })
	if u == nil || msg(u)["usage"].(map[string]any)["totalTokens"] != float64(13582) {
		t.Fatalf("usage %v; want the first call's context", u)
	}
	if count(std, "turn_end") != 4 || count(std, "agent_end") != 2 || len(failed) != 0 {
		t.Fatalf("turn_end %d, agent_end %d, failed %v; want 4 model calls in 2 turns", count(std, "turn_end"), count(std, "agent_end"), failed)
	}

	std, failed = replay("w2-bad-model.jsonl")
	if !slices.Equal(failed, []string{"01a0e5ce-e23d-7e13-a132-eb1d6c409f0c"}) {
		t.Fatalf("failed turns %v", failed)
	}
	if e := find(std, func(m map[string]any) bool { return msg(m)["stopReason"] == "error" }); e == nil ||
		!strings.HasPrefix(msg(e)["errorMessage"].(string), "The 'gpt-no-such-model' model is not supported") {
		t.Fatalf("error record %v; want the model API's message", e)
	}

	std, _ = replay("w3-mcp-tool.jsonl")
	if e := find(std, func(m map[string]any) bool { return m["type"] == "tool_execution_end" }); e == nil ||
		e["toolName"] != "mcp__piggery__who" || !strings.Contains(string(must(json.Marshal(e["result"]))), "Your team") {
		t.Fatalf("piggery tool end %v", e)
	}
}

func must(b []byte, _ error) []byte { return b }
