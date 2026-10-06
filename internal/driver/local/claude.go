package local

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// The Claude Code codec: `claude -p` stream-json through the
// `claude` symlink (comm "claude", stable across updates, C0). Stdin carries the process
// commands (initialize, interrupt, set_model) and the worker's mail: each batch core hands over
// (deliver) is one user message; its command_lifecycle completed or cancelled is the batch's end.
// No piggery hooks run in the worker; piggery mcp gives it the tools. Stdout is logged for tail.
//
// Wire shapes: initialize, the user message, interrupt with cancel_queued, result and
// command_lifecycle are captured (testdata/fixtures/claude-2.1.283, C0-C3, sj-*). set_model, apply_flag_settings and the
// can_use_tool answer follow the Agent SDK docs and are not captured yet.

// ClaudeHarness is what the Claude driver's workers run.
const ClaudeHarness = "claude"

// ClaudeToolPrefix is how Claude names piggery's tools: MCP server "piggery".
const ClaudeToolPrefix = "mcp__piggery__"

// ClaudeProfile is ~/.piggery/harness/claude.json.
type ClaudeProfile struct {
	Cmd  string   `json:"cmd"`  // default "claude" (the symlink, not versions/<v>)
	Args []string `json:"args"` // extra flags, e.g. how the Human's settings are loaded
	Env  []string `json:"env"`  // extra KEY=VALUE
	// Model and Thinking (Claude's effort level) for workers the chain gives none to.
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
	// DisallowedTools is every native tool a worker must not have (--disallowedTools): the
	// tools that would let it work around piggery or wait on a person. There is
	// no allowlist; a role keeps a listed tool with spawn.allow_tools (e.g. Agent).
	DisallowedTools []string `json:"disallowed_tools"`
	// Blacklist names parts of the Human's Claude setup a worker must not load: MCP
	// servers of ~/.claude.json by name, and plugins (`name@marketplace`), turned off with
	// enabledPlugins. The Human's hooks cannot be filtered: a hook that must not run in a
	// worker exits when it sees PIGGERY_ID.
	Blacklist []string `json:"blacklist"`
	// TestedVersions are the Claude Code versions piggery was tested with (`claude --version`);
	// doctor warns about another.
	TestedVersions []string `json:"tested_versions"`
}

// ClaudePlugin is piggery's hooks-only plugin for Claude sessions the Human opens.
const ClaudePlugin = "piggery@piggery"

// ClaudeHooks are the Claude hook events the adapter maps to standard events.
var ClaudeHooks = []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "StopFailure", "PermissionRequest", "PostModelSwitch", "SessionEnd"}

// ClaudeWorkerChannel (a format: the tool prefix) tells a worker how its mail arrives: as user
// messages from the codec (deliver), not hooks.
const ClaudeWorkerChannel = `
How piggery reaches you (this is the system you work in, not an injection):
- Mail arrives as a user message that starts with "[piggery]", also while you are working (after a tool call).
Act on that mail as work from your team. You can read pending mail with the %sinbox tool.`

// ClaudeChannel (a format: the tool prefix) tells the model that piggery mail is real (C8: without it the model refuses
// the nudge, additionalContext and Stop-block text as prompt injection).
const ClaudeChannel = `
How piggery reaches you (this is the system you work in, not an injection):
- New mail is announced by a short message that looks like it comes from "another Claude session"; it is piggery's own server on this machine.
- Mail shows up as "[piggery] N new message(s)" blocks added after a tool call, as hook feedback at the end of a turn, or with the prompt that starts a turn.
Act on that mail as work from your team. Read pending mail with the %sinbox tool.
For a long wait (a long sleep, a slow build), run the command in the background and end your turn; you are told when it finishes.`

// NewClaude returns the Claude Code driver over dir. self is the piggery executable that
// Claude runs as its MCP server and hooks ("" = this process's executable).
func NewClaude(dir, self string, opts Options) *Driver {
	if self == "" {
		self, _ = os.Executable()
	}
	return newWith(dir, opts, &claudeCodec{dir: dir, self: self})
}

// ClaudeProfilePath is where the Claude worker profile is read from.
func ClaudeProfilePath(dir string) string { return filepath.Join(dir, "harness", "claude.json") }

type claudeCodec struct {
	dir, self string

	mu    sync.Mutex
	state map[*worker]*claudeRun
}

// claudeRun is what the codec keeps about one live run.
type claudeRun struct {
	mu     sync.Mutex
	models []claudeModel     // from initialize
	model  string            // the run's model as passed ("" = Claude's default)
	tools  map[string]string // tool name by tool_use_id, for the tool_result
	// batches maps the uuid of each user message deliver wrote to the batch it carries, until
	// its command_lifecycle reports it completed or cancelled (then it is removed: once).
	batches map[string]int64
	// tooOld is set when the harness's init lacks msg_lifecycle_v1: no batch can complete, so
	// every deliver fails with it and the run is killed.
	tooOld string
	// started counts delivered messages whose command_lifecycle started and has not ended; a turn
	// whose init comes with none started is the harness's own (a background task's
	// task_notification, capture sj-bgdone): unbatched is set until its result.
	started   int
	unbatched bool
	// bgTasks are the running background tasks (the latest background_tasks_changed), which
	// abort stops: an interrupt alone leaves them running, and one ending opens a turn.
	bgTasks []string
}

type claudeModel struct {
	Value           string   `json:"value"`
	ResolvedModel   string   `json:"resolvedModel"`
	SupportsEffort  *bool    `json:"supportsEffort"`
	SupportedEffort []string `json:"supportedEffortLevels"`
}

func (*claudeCodec) harness() string { return ClaudeHarness }

func (*claudeCodec) toolPrefix() string { return ClaudeToolPrefix }

func (c *claudeCodec) defaults() (string, string) {
	p, _ := c.profile()
	return p.Model, p.Thinking
}

func (c *claudeCodec) profile() (ClaudeProfile, error) {
	var p ClaudeProfile
	b, err := os.ReadFile(ClaudeProfilePath(c.dir))
	if errors.Is(err, os.ErrNotExist) {
		return p, fmt.Errorf("no Claude worker profile at %s", ClaudeProfilePath(c.dir))
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("Claude worker profile %s: %w", ClaudeProfilePath(c.dir), err)
	}
	if p.Cmd == "" {
		p.Cmd = "claude"
	}
	inheritEmpty(&p.Model, &p.Thinking)
	return p, nil
}

func (c *claudeCodec) launch(s core.Spec) (launch, error) {
	prof, err := c.profile()
	if err != nil {
		return launch{}, err
	}
	settings, err := c.writeSettings(s, prof.Blacklist)
	if err != nil {
		return launch{}, err
	}
	mcp, err := c.mcpConfig(prof.Blacklist)
	if err != nil {
		os.RemoveAll(filepath.Dir(settings))
		return launch{}, err
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions", "--mcp-config", mcp, "--strict-mcp-config",
		// The Human's user setup (skills, plugins, hooks), never the repo's project/local settings;
		// piggery's settings file adds its hooks and turns blacklisted plugins off.
		"--setting-sources", "user", "--settings", settings}
	var disallowed []string
	for _, name := range prof.DisallowedTools {
		if !slices.Contains(s.AllowTools, name) {
			disallowed = append(disallowed, name)
		}
	}
	if len(disallowed) > 0 {
		args = append(args, "--disallowedTools", strings.Join(disallowed, " "))
	}
	if s.Resume {
		args = append(args, "--resume", s.HarnessRef)
	} else {
		args = append(args, "--session-id", s.HarnessRef)
	}
	model := firstNonEmpty(s.Model, prof.Model)
	if model != "" {
		args = append(args, "--model", model)
	}
	effort := firstNonEmpty(s.Thinking, prof.Thinking)
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	card, err := claudeCardArgs(filepath.Dir(settings), s.RoleCard+fmt.Sprintf(ClaudeWorkerChannel, ClaudeToolPrefix))
	if err != nil {
		os.RemoveAll(filepath.Dir(settings))
		return launch{}, err
	}
	args = append(args, card...)
	args = append(args, prof.Args...)
	return launch{
		cmd:      prof.Cmd,
		args:     args,
		env:      append([]string{"CLAUDE_CODE_DISABLE_AGENT_VIEW=1"}, prof.Env...),
		model:    model,
		thinking: effort,
		cleanup:  func() { os.RemoveAll(filepath.Dir(settings)) },
	}, nil
}

// mcpConfig is the worker's --mcp-config (with --strict-mcp-config, the only MCP it gets): the
// Human's user-scope servers of ~/.claude.json minus the blacklist, and piggery's own. Project
// servers (.mcp.json, projects[].mcpServers) are not loaded.
func (c *claudeCodec) mcpConfig(blacklist []string) (string, error) {
	servers := map[string]json.RawMessage{}
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil {
		var user struct {
			McpServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &user); err != nil {
			return "", fmt.Errorf("~/.claude.json: %w", err)
		}
		for name, s := range user.McpServers {
			if !slices.Contains(blacklist, name) {
				servers[name] = s
			}
		}
	}
	servers["piggery"], _ = json.Marshal(map[string]any{"type": "stdio", "command": c.self, "args": []string{"mcp"}})
	out, err := json.Marshal(map[string]any{"mcpServers": servers})
	return string(out), err
}

// writeSettings writes the run's --settings file: the adapter's hooks, each running
// `piggery hook claude <HookEvent>` (added to the Human's), and every blacklisted plugin off.
func (c *claudeCodec) writeSettings(s core.Spec, blacklist []string) (string, error) {
	dir := filepath.Join(RunRoot(c.dir, "claude"), s.ParticipantID, s.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// No piggery hooks: the codec delivers the worker's mail and reads each batch's end.
	// piggery's own plugin (hooks of a Human's Claude session, `piggery setup
	// claude`) is always off, so none of them run in a worker either.
	set := map[string]any{}
	off := map[string]bool{ClaudePlugin: false}
	for _, name := range blacklist {
		if strings.Contains(name, "@") { // plugins are name@marketplace; MCP servers never are
			off[name] = false
		}
	}
	set["enabledPlugins"] = off
	b, _ := json.MarshalIndent(set, "", " ")
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}

// ShellQuote quotes s for a POSIX shell (hook commands).
func ShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// started checks the run's effort against the model's supported levels (Claude silently runs
// a lower level). It sends no prompt: core delivers the worker's task as its first batch.
func (c *claudeCodec) started(ctx context.Context, w *worker, l launch) error {
	var init struct {
		Models []claudeModel `json:"models"`
	}
	if err := w.control(ctx, startTimeout, map[string]any{"subtype": "initialize"}, &init); err != nil {
		return err
	}
	if l.thinking != "" {
		if err := checkEffort(init.Models, l.model, l.thinking); err != nil {
			return err
		}
	}
	c.mu.Lock()
	if c.state == nil {
		c.state = map[*worker]*claudeRun{}
	}
	c.state[w] = &claudeRun{models: init.Models, model: l.model, tools: map[string]string{}}
	c.mu.Unlock()
	go func() { // forget the run once it ends
		<-w.done
		c.mu.Lock()
		delete(c.state, w)
		c.mu.Unlock()
	}()
	return nil
}

// claudeUserMessage is a stream-json user message. It has no priority: idle, it starts a turn;
// during a turn it joins it at the next tool boundary, and the tool running is not stopped
// ("now" would abort it and cancel the running message; capture sj-now).
func claudeUserMessage(uuid, text string) map[string]any {
	return map[string]any{"type": "user", "uuid": uuid, "session_id": "", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
}

// deliver writes a batch of mail as one user message, keyed by a new uuid that the batch's
// command_lifecycle lines carry back (batchOutcome). It never waits for stdout.
func (c *claudeCodec) deliver(w *worker, d core.Delivery) error {
	r := c.run(w)
	if r == nil {
		return errors.New("claude: no live run to deliver to")
	}
	uuid, err := randomUUID()
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.tooOld != "" {
		r.mu.Unlock()
		return errors.New(r.tooOld)
	}
	if r.batches == nil {
		r.batches = map[string]int64{}
	}
	r.batches[uuid] = d.Batch
	r.mu.Unlock()
	if err := w.send(claudeUserMessage(uuid, d.Text)); err != nil {
		r.mu.Lock()
		delete(r.batches, uuid)
		r.mu.Unlock()
		return err
	}
	return nil
}

// batchOutcome reads a command_lifecycle line (msg_lifecycle_v1): the batch a user message from
// deliver carried and whether it completed (the mail may be acked) or was cancelled (it may
// not); ok is false for any other line, a state that is not final (queued, started) and a
// message deliver did not write or already reported. A result line is never an outcome: its
// uuid is new, one result can end several messages, and a cancelled message still ends with
// one (captures sj-none, sj-now, sj-abort).
func (r *claudeRun) batchOutcome(line []byte) (batch int64, completed, ok bool) {
	var m struct {
		Type    string `json:"type"`
		Command string `json:"command_uuid"`
		State   string `json:"state"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "command_lifecycle" || (m.State != "completed" && m.State != "cancelled") {
		return 0, false, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	batch, ok = r.batches[m.Command]
	if ok {
		delete(r.batches, m.Command)
	}
	return batch, m.State == "completed", ok
}

// turnEvent follows the harness's turns for the ones no delivered batch started, so the worker's
// state stays right: an init while no message of ours has started is such a turn
// (core.TurnStarted), and the next result ends it (core.TurnEnded). It also keeps the running
// background tasks.
func (r *claudeRun) turnEvent(line []byte) string {
	var m struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		State   string `json:"state"`
		Tasks   []struct {
			ID string `json:"task_id"`
		} `json:"tasks"`
	}
	if json.Unmarshal(line, &m) != nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case m.Type == "command_lifecycle" && m.State == "started":
		r.started++
	case m.Type == "command_lifecycle" && (m.State == "completed" || m.State == "cancelled"):
		r.started = max(r.started-1, 0)
	case m.Type == "system" && m.Subtype == "background_tasks_changed":
		r.bgTasks = r.bgTasks[:0]
		for _, t := range m.Tasks {
			r.bgTasks = append(r.bgTasks, t.ID)
		}
	case m.Type == "system" && m.Subtype == "init" && r.started == 0 && !r.unbatched:
		r.unbatched = true
		return core.TurnStarted
	case m.Type == "result" && r.unbatched:
		r.unbatched = false
		return core.TurnEnded
	}
	return ""
}

// checkLifecycle reads the harness's init line (the first comes with the first user message:
// initialize does not carry capabilities). Without msg_lifecycle_v1 no batch can complete:
// the run is too old, every open batch is cancelled (its mail is given again) and the returned
// reason fails later delivers and kills the run.
func (r *claudeRun) checkLifecycle(line []byte) (ended []core.DeliveryEnd, fatal string) {
	var m struct {
		Type         string   `json:"type"`
		Subtype      string   `json:"subtype"`
		Version      string   `json:"claude_code_version"`
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "system" || m.Subtype != "init" || slices.Contains(m.Capabilities, "msg_lifecycle_v1") {
		return nil, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tooOld != "" {
		return nil, ""
	}
	r.tooOld = fmt.Sprintf("claude %s is too old: msg_lifecycle_v1 missing (piggery needs claude 2.1.283 or later)", m.Version)
	for uuid, b := range r.batches {
		ended = append(ended, core.DeliveryEnd{Batch: b, Outcome: core.DeliveryCancelled})
		delete(r.batches, uuid)
	}
	return ended, r.tooOld
}

// checkEffort fails unless model ("" = Claude's default) runs level as asked.
func checkEffort(models []claudeModel, model, level string) error {
	if model == "" {
		model = "default"
	}
	for _, m := range models {
		if m.Value != model && m.ResolvedModel != model {
			continue
		}
		if slices.Contains(m.SupportedEffort, level) {
			return nil
		}
		if len(m.SupportedEffort) == 0 {
			return fmt.Errorf("thinking level: Claude model %q takes no effort level, not %q", model, level)
		}
		return fmt.Errorf("thinking level: Claude model %q runs effort %s, not %q", model, strings.Join(m.SupportedEffort, "|"), level)
	}
	return fmt.Errorf("thinking level: cannot check effort %q: Claude does not list model %q", level, model)
}

func (c *claudeCodec) run(w *worker) *claudeRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state[w]
}

func (c *claudeCodec) record(w *worker, line []byte) record {
	var rec struct {
		Type     string `json:"type"`
		Response struct {
			RequestID string `json:"request_id"`
		} `json:"response"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type == "stream_event" {
		return record{}
	}
	run := c.run(w)
	r := record{keep: true, std: claudeStandard(run, line)}
	if run != nil {
		switch rec.Type {
		case "command_lifecycle":
			if b, completed, ok := run.batchOutcome(line); ok {
				out := core.DeliveryCancelled
				if completed {
					out = core.DeliveryCompleted
				}
				r.ended = []core.DeliveryEnd{{Batch: b, Outcome: out}}
			}
		case "system":
			r.ended, r.fatal = run.checkLifecycle(line)
		}
		r.turn = run.turnEvent(line)
	}
	switch rec.Type {
	case "control_response":
		r.answers = rec.Response.RequestID
	case "control_request":
		// Workers run bypassPermissions; a prompt that still reaches the host is refused
		// rather than left waiting forever (SDK shape; not captured).
		if rec.Request.Subtype == "can_use_tool" {
			r.reply, _ = json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": rec.RequestID,
				"response": map[string]any{"behavior": "deny", "message": "piggery workers get no permission prompts"}}})
		}
	}
	return r
}

// claudeStandard translates one Claude stdout line into the standard records (runner.go) that
// tail and top read: text and usage of the main agent's messages (a subagent's are skipped),
// its tool calls and results, and per result one turn_end per model call (num_turns), an error
// line when it failed, and agent_end. An empty thinking block (Claude's default) shows nothing.
// usage is per message; the result's modelUsage and total_cost_usd are cumulative and not read.
func claudeStandard(run *claudeRun, line []byte) [][]byte {
	var m struct {
		Type          string  `json:"type"`
		ParentToolUse *string `json:"parent_tool_use_id"`
		Message       struct {
			Content json.RawMessage `json:"content"`
			Usage   *struct {
				Input      int `json:"input_tokens"`
				Output     int `json:"output_tokens"`
				CacheRead  int `json:"cache_read_input_tokens"`
				CacheWrite int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		IsError        bool     `json:"is_error"`
		NumTurns       int      `json:"num_turns"`
		Result         string   `json:"result"`
		Errors         []string `json:"errors"`
		TerminalReason string   `json:"terminal_reason"`
	}
	if json.Unmarshal(line, &m) != nil || m.ParentToolUse != nil {
		return nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     map[string]any  `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	var s string
	if json.Unmarshal(m.Message.Content, &s) == nil {
		m.Message.Content, _ = json.Marshal([]map[string]string{{"type": "text", "text": s}})
	}
	json.Unmarshal(m.Message.Content, &blocks)
	var out [][]byte
	add := func(v any) {
		b, _ := json.Marshal(v)
		out = append(out, b)
	}
	texts := func() []map[string]string {
		var t []map[string]string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				t = append(t, map[string]string{"type": "text", "text": b.Text})
			}
		}
		return t
	}
	switch m.Type {
	case "assistant":
		msg := map[string]any{"role": "assistant", "content": texts()}
		if u := m.Message.Usage; u != nil {
			msg["usage"] = map[string]int{"input": u.Input, "output": u.Output, "cacheRead": u.CacheRead, "cacheWrite": u.CacheWrite}
		}
		add(map[string]any{"type": "message_end", "message": msg})
		for _, b := range blocks {
			if b.Type != "tool_use" {
				continue
			}
			name := strings.ToLower(b.Name)
			if run != nil {
				run.mu.Lock()
				run.tools[b.ID] = name
				run.mu.Unlock()
			}
			args := b.Input
			if p, ok := args["file_path"]; ok { // tail shows read/edit/write by their path
				args = maps.Clone(args)
				args["path"] = p
				delete(args, "file_path")
			}
			add(map[string]any{"type": "tool_execution_start", "toolName": name, "args": args})
		}
	case "user":
		if t := texts(); len(t) > 0 {
			add(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": t}})
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			name := ""
			if run != nil {
				run.mu.Lock()
				name = run.tools[b.ToolUseID]
				run.mu.Unlock()
			}
			content := b.Content
			if json.Unmarshal(content, &s) == nil {
				content, _ = json.Marshal([]map[string]string{{"type": "text", "text": s}})
			}
			add(map[string]any{"type": "tool_execution_end", "toolName": name, "isError": b.IsError,
				"result": map[string]any{"content": content}})
		}
	case "result":
		for range m.NumTurns {
			add(map[string]string{"type": "turn_end"})
		}
		if m.IsError {
			msg := firstNonEmpty(m.Result, strings.Join(m.Errors, "; "), m.TerminalReason)
			add(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant",
				"stopReason": "error", "errorMessage": msg}})
		}
		add(map[string]string{"type": "agent_end"})
	}
	return out
}

// abort stops the run's background tasks (stop_task each: an interrupt leaves them running, and
// one ending later opens a turn that answers the aborted mail; capture
// sj-bgstop-*: status killed, no turn after), then interrupts the current turn and cancels the
// messages queued behind it (C3), so the worker does not wake itself. It waits for no receipt.
func (c *claudeCodec) abort(w *worker) error {
	if r := c.run(w); r != nil {
		r.mu.Lock()
		tasks := slices.Clone(r.bgTasks)
		r.mu.Unlock()
		for _, id := range tasks {
			if err := w.send(map[string]any{"type": "control_request", "request_id": nextRequestID(),
				"request": map[string]any{"subtype": "stop_task", "task_id": id}}); err != nil {
				return err
			}
		}
	}
	return w.send(map[string]any{"type": "control_request", "request_id": nextRequestID(),
		"request": map[string]any{"subtype": "interrupt", "cancel_queued": true}})
}

func (c *claudeCodec) setModel(ctx context.Context, w *worker, model string) error {
	if err := w.control(ctx, commandTimeout, map[string]any{"subtype": "set_model", "model": model}, nil); err != nil {
		return err
	}
	if r := c.run(w); r != nil {
		r.mu.Lock()
		r.model = model
		r.mu.Unlock()
	}
	return nil
}

// models are the values initialize listed for the run (what set_model takes; no extra call).
func (c *claudeCodec) models(_ context.Context, w *worker) ([]string, error) {
	r := c.run(w)
	if r == nil {
		return nil, core.ErrNotRunning
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m.Value)
	}
	return out, nil
}

// setThinking sets the effort level for the next turn, refusing a level the run's model does
// not run (Claude would run a lower one without saying).
func (c *claudeCodec) setThinking(ctx context.Context, w *worker, level string) error {
	r := c.run(w)
	if r == nil {
		return fmt.Errorf("thinking level: %w", core.ErrNotRunning)
	}
	r.mu.Lock()
	models, model := r.models, r.model
	r.mu.Unlock()
	if err := checkEffort(models, model, level); err != nil {
		return err
	}
	return w.control(ctx, commandTimeout, map[string]any{"subtype": "apply_flag_settings",
		"settings": map[string]any{"effortLevel": level}}, nil)
}

func nextRequestID() string { return "piggery-" + strconv.FormatInt(nextID.Add(1), 10) }

// randomUUID is a version 4 UUID (Claude's message uuid).
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// control sends one control_request and waits for its control_response; an error response is
// an error naming the subtype. out (may be nil) receives the response payload.
func (w *worker) control(ctx context.Context, timeout time.Duration, req map[string]any, out any) error {
	id := nextRequestID()
	sub, _ := req["subtype"].(string)
	line, err := w.request(ctx, timeout, id, sub, map[string]any{"type": "control_request", "request_id": id, "request": req})
	if err != nil {
		return err
	}
	var resp struct {
		Response struct {
			Subtype  string          `json:"subtype"`
			Error    string          `json:"error"`
			Response json.RawMessage `json:"response"`
		} `json:"response"`
	}
	json.Unmarshal(line, &resp)
	if resp.Response.Subtype != "success" {
		return fmt.Errorf("claude refused %s: %s", sub, firstNonEmpty(resp.Response.Error, resp.Response.Subtype))
	}
	if out != nil && len(resp.Response.Response) > 0 {
		return json.Unmarshal(resp.Response.Response, out)
	}
	return nil
}
