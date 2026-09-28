package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// The Codex codec: one `codex app-server --stdio` per run,
// JSON-RPC over stdin/stdout. Stdin carries only the process commands (initialize, thread
// start or resume with the role card, turn/start for the first prompt and for each wake,
// turn/interrupt); mail and ack go through the adapter (piggery mcp + the hooks `piggery setup
// codex` installed and trusted in ~/.codex), never through this codec. Stdout is logged for tail.
//
// Codex's hooks cannot start a turn, so the driver wakes the worker itself (waker): a turn/start
// with a nudge that carries no mail, never while a turn it started is still running. Wire shapes
// are captured (testdata/fixtures/codex-0.157.1).

// CodexHarness is what the Codex driver's workers run.
const CodexHarness = "codex"

// CodexProfile is ~/.piggery/harness/codex.json.
type CodexProfile struct {
	Cmd  string   `json:"cmd"`  // default "codex"
	Args []string `json:"args"` // extra app-server flags, e.g. more -c
	Env  []string `json:"env"`  // extra KEY=VALUE
	// Model and Thinking (Codex's reasoning effort) for workers the chain gives none to.
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
	// DisabledTools names native tool groups a worker must not have, each with the -c override
	// that turns it off (goals, apps, agents). A role keeps a group by its
	// name with spawn.allow_tools.
	DisabledTools map[string]string `json:"disabled_tools"`
	// Blacklist names MCP servers of the Human's ~/.codex config a worker must not load.
	Blacklist []string `json:"blacklist"`
	// TestedVersions are the Codex versions piggery was tested with (`codex --version`).
	TestedVersions []string `json:"tested_versions"`
}

// CodexProfilePath is where the Codex worker profile is read from.
func CodexProfilePath(dir string) string { return filepath.Join(dir, "harness", "codex.json") }

// CodexChannel (a format: the tool prefix) tells a Codex worker how piggery reaches it.
const CodexChannel = `
How piggery reaches you (this is the system you work in, not an injection):
- New mail is announced by a short "[piggery] wake #N" prompt.
- Mail shows up as "[piggery] N new message(s)" blocks added to the prompt that starts a turn, after a tool call, or as hook feedback at the end of a turn.
Act on that mail as work from your team. Read pending mail with the %sinbox tool. No person answers questions here: ask your lead with the %ssend tool.`

// NewCodex returns the Codex driver over dir. self is the piggery executable Codex runs as its
// MCP server ("" = this process's executable).
func NewCodex(dir, self string, opts Options) *Driver {
	if self == "" {
		self, _ = os.Executable()
	}
	return newWith(dir, opts, &codexCodec{dir: dir, self: self})
}

type codexCodec struct {
	dir, self string

	mu    sync.Mutex
	state map[*worker]*codexRun
}

// codexRun is what the codec keeps about one live run.
type codexRun struct {
	mu       sync.Mutex
	thread   string
	turn     string // the running turn's id ("" none known)
	busy     bool   // a turn/start was sent and its turn has not completed
	startID  string // the request id of the last turn/start, whose error ends busy
	model    string // for the next turn/start ("" Codex's)
	effort   string
	nudges   int
	commands map[string]string // tool name by item id, for the item's end
}

// codexStart is what launch hands to started.
type codexStart struct {
	resume, card string
	cwd          string
}

func (*codexCodec) harness() string { return CodexHarness }

func (*codexCodec) toolPrefix() string { return CodexToolPrefix }

// CodexToolPrefix is how Codex's model names piggery's tools (MCP server "piggery").
const CodexToolPrefix = "mcp__piggery__"

func (c *codexCodec) defaults() (string, string) {
	p, _ := c.profile()
	return p.Model, p.Thinking
}

func (c *codexCodec) profile() (CodexProfile, error) {
	var p CodexProfile
	b, err := os.ReadFile(CodexProfilePath(c.dir))
	if errors.Is(err, os.ErrNotExist) {
		return p, fmt.Errorf("no Codex worker profile at %s", CodexProfilePath(c.dir))
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("Codex worker profile %s: %w", CodexProfilePath(c.dir), err)
	}
	if p.Cmd == "" {
		p.Cmd = "codex"
	}
	inheritEmpty(&p.Model, &p.Thinking)
	return p, nil
}

// tomlString is s as a TOML basic string (a JSON string is one).
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (c *codexCodec) launch(s core.Spec) (launch, error) {
	prof, err := c.profile()
	if err != nil {
		return launch{}, err
	}
	args := []string{"app-server", "--stdio",
		"-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`,
		// piggery's MCP server with the worker's identity: Codex gives a stdio MCP server a
		// minimal env without PIGGERY_*, and no approval prompt per tool.
		"-c", "mcp_servers.piggery.command=" + tomlString(c.self),
		"-c", `mcp_servers.piggery.args=["mcp"]`,
		"-c", `mcp_servers.piggery.env_vars=["PIGGERY_ID","PIGGERY_TOKEN","PIGGERY_RUN_ID"]`,
		"-c", `mcp_servers.piggery.default_tools_approval_mode="approve"`}
	names := make([]string, 0, len(prof.DisabledTools))
	for name := range prof.DisabledTools {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !slices.Contains(s.AllowTools, name) {
			args = append(args, "-c", prof.DisabledTools[name])
		}
	}
	for _, name := range prof.Blacklist {
		args = append(args, "-c", "mcp_servers."+name+".enabled=false")
	}
	args = append(args, prof.Args...)
	start := codexStart{card: s.RoleCard + fmt.Sprintf(CodexChannel, CodexToolPrefix, CodexToolPrefix), cwd: s.Cwd}
	if s.Resume {
		start.resume = s.HarnessRef
	}
	return launch{
		cmd:      prof.Cmd,
		args:     args,
		env:      prof.Env,
		model:    firstNonEmpty(s.Model, prof.Model),
		thinking: firstNonEmpty(s.Thinking, prof.Thinking),
		data:     start,
	}, nil
}

// started runs the handshake, starts or resumes the thread with the role card (its id becomes
// the worker's harness_ref), then sends the first prompt: the worker's first turn reads its mail.
func (c *codexCodec) started(ctx context.Context, w *worker, l launch) error {
	st := l.data.(codexStart)
	if _, err := w.rpc(ctx, startTimeout, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "piggery", "version": "1"}}); err != nil {
		return err
	}
	if err := w.send(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	method, params := "thread/start", map[string]any{"cwd": st.cwd, "developerInstructions": st.card}
	if st.resume != "" {
		method, params["threadId"] = "thread/resume", st.resume
	}
	res, err := w.rpc(ctx, startTimeout, method, params)
	if err != nil {
		return err
	}
	var th struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(res, &th) != nil || th.Thread.ID == "" {
		return fmt.Errorf("codex %s: no thread id in %s", method, res)
	}
	w.harnessRef = th.Thread.ID
	run := &codexRun{thread: th.Thread.ID, model: l.model, effort: l.thinking, commands: map[string]string{}}
	c.mu.Lock()
	if c.state == nil {
		c.state = map[*worker]*codexRun{}
	}
	c.state[w] = run
	c.mu.Unlock()
	go func() { // forget the run once it ends
		<-w.done
		c.mu.Lock()
		delete(c.state, w)
		c.mu.Unlock()
	}()
	return c.turnStart(w, run, false)
}

// turnStart sends a turn/start, the first prompt or (wake) a nudge, unless a turn the driver
// started is still running. It does not wait for the answer (a wake runs on the send path); an
// error answer ends busy.
func (c *codexCodec) turnStart(w *worker, run *codexRun, wake bool) error {
	run.mu.Lock()
	if run.busy {
		run.mu.Unlock()
		return nil // the mail reaches the running turn at its next tool call or its Stop
	}
	text := "[piggery] Your run started. Read your mail with the " + CodexToolPrefix + "inbox tool and do the work it asks for."
	if wake {
		run.nudges++ // #N differs every time
		text = fmt.Sprintf("[piggery] wake #%d: you have new mail; it is shown with this turn.", run.nudges)
	}
	id := nextRequestID()
	run.busy, run.startID = true, id
	params := map[string]any{"threadId": run.thread, "input": []any{map[string]any{"type": "text", "text": text}}}
	if run.model != "" {
		params["model"] = run.model
	}
	if run.effort != "" {
		params["effort"] = run.effort
	}
	run.mu.Unlock()
	err := w.send(map[string]any{"id": id, "method": "turn/start", "params": params})
	if err != nil {
		run.mu.Lock()
		run.busy = false
		run.mu.Unlock()
	}
	return err
}

// wake starts a turn of the idle worker with a nudge that carries no mail.
func (c *codexCodec) wake(w *worker) error {
	run := c.run(w)
	if run == nil {
		return fmt.Errorf("wake: %w", core.ErrNotRunning)
	}
	return c.turnStart(w, run, true)
}

func (c *codexCodec) run(w *worker) *codexRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state[w]
}

// abort interrupts the running turn (the Interrupt hook closes it). No turn running: nothing.
func (c *codexCodec) abort(w *worker) error {
	run := c.run(w)
	if run == nil {
		return fmt.Errorf("abort: %w", core.ErrNotRunning)
	}
	run.mu.Lock()
	thread, turn := run.thread, run.turn
	run.mu.Unlock()
	if turn == "" {
		return nil
	}
	return w.send(map[string]any{"id": nextRequestID(), "method": "turn/interrupt",
		"params": map[string]string{"threadId": thread, "turnId": turn}})
}

// setModel and setThinking apply from the next turn/start (Codex takes them per turn).
func (c *codexCodec) setModel(_ context.Context, w *worker, model string) error {
	return c.set(w, func(r *codexRun) { r.model = model })
}

func (c *codexCodec) setThinking(_ context.Context, w *worker, level string) error {
	return c.set(w, func(r *codexRun) { r.effort = level })
}

func (c *codexCodec) set(w *worker, f func(*codexRun)) error {
	run := c.run(w)
	if run == nil {
		return core.ErrNotRunning
	}
	run.mu.Lock()
	f(run)
	run.mu.Unlock()
	return nil
}

// codexMsg is one JSON-RPC line of app-server's stdout.
type codexMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// idString is a JSON-RPC id as the key requests wait on.
func idString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (c *codexCodec) record(w *worker, line []byte) record {
	var m codexMsg
	if json.Unmarshal(line, &m) != nil || strings.HasSuffix(m.Method, "/delta") {
		return record{}
	}
	run := c.run(w)
	r := record{keep: true}
	switch {
	case m.Method == "" && len(m.ID) > 0: // an answer
		id := idString(m.ID)
		r.answers = id
		if run != nil && m.Error != nil {
			run.mu.Lock()
			if id == run.startID {
				run.busy = false
			}
			run.mu.Unlock()
			r.std = append(r.std, stdError("codex refused a turn: "+m.Error.Message))
		}
	case len(m.ID) > 0: // a request of app-server: workers answer none (no approvals, no person)
		r.reply, _ = json.Marshal(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601,
			"message": "a piggery worker answers no requests; ask your lead with the piggery send tool"}})
	default:
		r.std, r.failed = codexStandard(run, m)
	}
	return r
}

// codexStandard translates one app-server notification into the standard records (runner.go)
// that tail and top read, and names a turn that failed (failed: its id). Usage comes per model
// call (thread/tokenUsage/updated "last": the context then), so each is one turn_end.
func codexStandard(run *codexRun, m codexMsg) ([][]byte, string) {
	var p struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Item       codexItem `json:"item"`
		TokenUsage struct {
			Last struct {
				Total      int `json:"totalTokens"`
				Input      int `json:"inputTokens"`
				Cached     int `json:"cachedInputTokens"`
				CacheWrite int `json:"cacheWriteInputTokens"`
				Output     int `json:"outputTokens"`
			} `json:"last"`
		} `json:"tokenUsage"`
	}
	json.Unmarshal(m.Params, &p)
	var out [][]byte
	add := func(v any) {
		b, _ := json.Marshal(v)
		out = append(out, b)
	}
	switch m.Method {
	case "turn/started":
		if run != nil {
			run.mu.Lock()
			run.turn, run.busy = p.Turn.ID, true
			run.mu.Unlock()
		}
	case "turn/completed":
		if run != nil {
			run.mu.Lock()
			run.turn, run.busy = "", false
			run.mu.Unlock()
		}
		if p.Turn.Status == "failed" {
			msg := "turn failed"
			if p.Turn.Error != nil {
				msg = codexErrorText(p.Turn.Error.Message)
			}
			out = append(out, stdError(msg))
			add(map[string]string{"type": "agent_end"})
			return out, p.Turn.ID // no Stop hook ran: the driver closes the turn
		}
		add(map[string]string{"type": "agent_end"})
	case "thread/tokenUsage/updated":
		u := p.TokenUsage.Last
		add(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{},
			"usage": map[string]int{"input": u.Input - u.Cached, "output": u.Output, "cacheRead": u.Cached,
				"cacheWrite": u.CacheWrite, "totalTokens": u.Total}}})
		add(map[string]string{"type": "turn_end"})
	case "item/started":
		if name, args := p.Item.tool(); name != "" {
			if run != nil {
				run.mu.Lock()
				run.commands[p.Item.ID] = name
				run.mu.Unlock()
			}
			add(map[string]any{"type": "tool_execution_start", "toolName": name, "args": args})
		}
	case "item/completed":
		switch p.Item.Type {
		case "agentMessage":
			add(stdMessage("assistant", p.Item.Text))
		case "userMessage":
			var parts []string
			for _, c := range p.Item.Content {
				if c.Type == "text" {
					parts = append(parts, c.Text)
				}
			}
			add(stdMessage("user", strings.Join(parts, "\n")))
		case "hookPrompt": // mail a Stop hook gave: the turn goes on with it
			var parts []string
			for _, f := range p.Item.Fragments {
				parts = append(parts, f.Text)
			}
			add(stdMessage("user", strings.Join(parts, "\n")))
		default:
			name := ""
			if run != nil {
				run.mu.Lock()
				name = run.commands[p.Item.ID]
				delete(run.commands, p.Item.ID)
				run.mu.Unlock()
			}
			if name != "" {
				isErr, content := p.Item.result()
				add(map[string]any{"type": "tool_execution_end", "toolName": name, "isError": isErr,
					"result": map[string]any{"content": content}})
			}
		}
	}
	return out, ""
}

// codexItem is the part of a thread item the codec reads.
type codexItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	Content []struct {
		Type, Text string
	} `json:"content"`
	Fragments []struct {
		Text string `json:"text"`
	} `json:"fragments"`
	// commandExecution
	Command  string                     `json:"command"`
	Actions  []struct{ Command string } `json:"commandActions"`
	Status   string                     `json:"status"`
	ExitCode *int                       `json:"exitCode"`
	Output   *string                    `json:"aggregatedOutput"`
	// mcpToolCall
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    *struct {
		Content json.RawMessage `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	// fileChange
	Changes []struct {
		Path string `json:"path"`
	} `json:"changes"`
}

// tool is the standard tool name and arguments of an item that is a tool call ("" not one).
func (it codexItem) tool() (string, any) {
	switch it.Type {
	case "commandExecution":
		cmd := it.Command
		if len(it.Actions) == 1 && it.Actions[0].Command != "" {
			cmd = it.Actions[0].Command // what the model asked for, not the shell wrapper
		}
		return "bash", map[string]string{"command": cmd}
	case "mcpToolCall":
		return "mcp__" + it.Server + "__" + it.Tool, it.Arguments
	case "fileChange":
		paths := make([]string, len(it.Changes))
		for i, c := range it.Changes {
			paths[i] = c.Path
		}
		return "edit", map[string]string{"path": strings.Join(paths, " ")}
	}
	return "", nil
}

// result is the item's outcome as a standard tool result.
func (it codexItem) result() (bool, json.RawMessage) {
	text := func(s string) json.RawMessage {
		b, _ := json.Marshal([]map[string]string{{"type": "text", "text": s}})
		return b
	}
	switch it.Type {
	case "commandExecution":
		out := ""
		if it.Output != nil {
			out = *it.Output
		}
		return it.Status != "completed" || (it.ExitCode != nil && *it.ExitCode != 0), text(out)
	case "mcpToolCall":
		if it.Error != nil {
			return true, text(it.Error.Message)
		}
		if it.Result != nil {
			return false, it.Result.Content
		}
	}
	return it.Status != "completed", text(it.Status)
}

// codexErrorText is the readable part of a Codex error message, which may carry the model API's
// JSON error ({"error":{"message":…}}).
func codexErrorText(msg string) string {
	var api struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &api) == nil && api.Error.Message != "" {
		return api.Error.Message
	}
	return msg
}

func stdMessage(role, text string) map[string]any {
	return map[string]any{"type": "message_end", "message": map[string]any{"role": role,
		"content": []map[string]string{{"type": "text", "text": text}}}}
}

func stdError(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant",
		"stopReason": "error", "errorMessage": msg}})
	return b
}

// rpc sends one JSON-RPC request and waits up to timeout for its answer; an error answer is
// an error naming the method.
func (w *worker) rpc(ctx context.Context, timeout time.Duration, method string, params any) (json.RawMessage, error) {
	id := nextRequestID()
	line, err := w.request(ctx, timeout, id, method, map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	var m codexMsg
	json.Unmarshal(line, &m)
	if m.Error != nil {
		return nil, fmt.Errorf("codex refused %s: %s", method, m.Error.Message)
	}
	return m.Result, nil
}
