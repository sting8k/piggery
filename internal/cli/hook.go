package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/proto"
)

// `piggery hook <harness> <HookEvent>` is the hook half of the Claude Code and Codex adapters:
// the harness runs it for each hook with the event's JSON on
// stdin; it maps the event to one standard adapter event (verb harness.event, the daemon counts
// batches and acks) and prints what the harness should do with the answer. It keeps no state and
// no rule; what differs per harness is only its table (hookHarnesses). It never makes the harness
// fail: an unknown event or a daemon error prints nothing and exits 0.
//
// Two identities: a worker's hooks carry PIGGERY_ID/TOKEN (errors go to stderr, which the harness
// only logs); a session the Human opened (tier 2, installed by `piggery setup claude|codex`) has
// none and is its harness process (host, shared with its MCP server): SessionStart places it with
// join.auto, every other hook authenticates by host. That one is silent on any error: no daemon,
// no piggery.

// claudeHookInput is the part of a hook's stdin the adapters read (Claude's fields, and Codex's
// where they differ: turn_id).
type claudeHookInput struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	Source         string `json:"source"`            // SessionStart: startup|resume|clear|compact
	ToolName       string `json:"tool_name"`         // PreToolUse
	Notification   string `json:"notification_type"` // Notification: idle_prompt, permission_prompt, …
	Reason         string `json:"reason"`            // SessionEnd: clear, prompt_input_exit, …
	PromptID       string `json:"prompt_id"`
	TurnID         string `json:"turn_id"` // Codex: the turn key (like Claude's prompt_id)
	StopHookActive bool   `json:"stop_hook_active"`
	ToModel        string `json:"to_model"`
	Model          string `json:"model"`
	// AgentID is set only when the hook fires inside a subagent (Agent tool): same session_id and
	// prompt_id as the turn that called it (capture hooks-subagent.jsonl).
	AgentID string `json:"agent_id"`
	// TranscriptPath is the harness's own transcript of the session (Claude: its projects file,
	// Codex: its rollout); null for a Codex ephemeral thread, which writes none.
	TranscriptPath string `json:"transcript_path"`
}

// claudeHookEvent maps a Claude hook event and its input to the standard event (ok false: not one
// the adapter reports).
func claudeHookEvent(hook string, in claudeHookInput) (core.HarnessEventArgs, bool) {
	a := core.HarnessEventArgs{PromptID: in.PromptID}
	// A subagent is not the participant: its tool calls are no boundary of the participant's turn
	// (mail given there would land in the subagent's context, not the model that reads mail). Its
	// permission prompt still waits on the Human and holds the session, so it is reported; the
	// wait ends at the parent's next tool result (the Agent call) or Stop.
	if in.AgentID != "" && hook != "PermissionRequest" {
		return a, false
	}
	switch hook {
	case "SessionStart":
		// source clear|compact: the conversation is gone, so the daemon hands back the role card
		// (printed as additionalContext); startup has it in the MCP server's instructions.
		a.Event, a.HarnessRef, a.Model, a.Source = core.HarnessSessionStart, in.SessionID, in.Model, in.Source
	case "UserPromptSubmit":
		a.Event = core.HarnessTurnStart
	case "PostToolUse":
		a.Event = core.HarnessToolBoundary
	case "Stop":
		a.Event, a.Outcome, a.StopHookActive = core.HarnessTurnEnd, core.HarnessOutcomeOK, in.StopHookActive
	case "StopFailure":
		a.Event, a.Outcome = core.HarnessTurnEnd, core.HarnessOutcomeFailed
	case "PermissionRequest":
		a.Event = core.HarnessPermission
	case "Notification":
		if in.Notification != "idle_prompt" {
			return a, false
		}
		// Claude waits for its user (about a minute after a Stop). Not after Esc (captured): a
		// turn left open by Esc closes at the next prompt.
		a.Event = core.HarnessIdle
	case "PostModelSwitch":
		a.Event, a.Model = core.HarnessModelChanged, in.ToModel
	case "SessionEnd":
		if in.Reason == "clear" {
			// /clear ends the old session id inside the same claude process, right before
			// SessionStart source=clear: the participant lives on (fixture hooks-session.jsonl).
			return a, false
		}
		a.Event = core.HarnessSessionEnd
	default:
		return a, false
	}
	return a, true
}

// claudeHookOutput is the JSON Claude reads from a hook's stdout for this answer (nil: print
// nothing). Mail goes in as additionalContext where the event takes it; at the end of a turn
// Block keeps the turn going with the mail as the reason (Stop decision control).
func claudeHookOutput(hook string, r core.HarnessEventResult) any {
	if r.Text == "" {
		return nil
	}
	switch hook {
	case "Stop":
		if r.Block {
			return map[string]any{"decision": "block", "reason": r.Text}
		}
		return nil
	case "SessionStart", "UserPromptSubmit", "PostToolUse", "PostModelSwitch":
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": hook, "additionalContext": r.Text}}
	}
	return nil
}

// codexHookEvent maps a Codex hook to the standard event; turn_id is the batch key.
func codexHookEvent(hook string, in claudeHookInput) (core.HarnessEventArgs, bool) {
	a := core.HarnessEventArgs{PromptID: in.TurnID}
	// A native subagent's hooks carry the parent's session_id, their own turn_id and agent_id;
	// mail given there reaches only the subagent (captured). Same rule as
	// Claude: only its permission prompt is reported, since the Human is being asked.
	if in.AgentID != "" && hook != "PermissionRequest" {
		return a, false
	}
	switch hook {
	case "SessionStart":
		a.Event, a.HarnessRef, a.Model, a.Source = core.HarnessSessionStart, in.SessionID, in.Model, in.Source
	case "UserPromptSubmit":
		a.Event = core.HarnessTurnStart
	case "PostToolUse":
		a.Event = core.HarnessToolBoundary
	case "Stop":
		a.Event, a.Outcome, a.StopHookActive = core.HarnessTurnEnd, core.HarnessOutcomeOK, in.StopHookActive
	case "Interrupt":
		// Esc or turn/interrupt: the turn is over, nothing acked (Claude has no such hook).
		a.Event, a.Outcome = core.HarnessTurnEnd, core.HarnessOutcomeIntr
	case "PermissionRequest":
		a.Event = core.HarnessPermission
	case "SessionEnd":
		// After /clear the old session id ends about 30 s after the new one started (same
		// process): the daemon ignores an end whose ref is not the participant's current one.
		a.Event, a.HarnessRef = core.HarnessSessionEnd, in.SessionID
	default:
		return a, false
	}
	return a, true
}

// codexHookOutput is what Codex reads back: the same additionalContext and Stop block shapes as
// Claude's, except that Stop must always answer JSON when it exits 0 (hooks docs).
func codexHookOutput(hook string, r core.HarnessEventResult) any {
	if out := claudeHookOutput(hook, r); out != nil {
		return out
	}
	if hook == "Stop" {
		return map[string]any{}
	}
	return nil
}

func (e *env) hook(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("%w: hook claude|codex <HookEvent>", errUsage)
	}
	if h, ok := harnessNamed(args[0]); !ok || h.hookEvent == nil {
		return fmt.Errorf("%w: hook claude|codex <HookEvent>", errUsage)
	}
	e.runHook(args[0], args[1], os.Stdin, os.Stderr)
	return nil
}

func (e *env) runHook(harness, hook string, stdin io.Reader, stderr io.Writer) {
	h, _ := harnessNamed(harness)
	id, tok := os.Getenv("PIGGERY_ID"), os.Getenv("PIGGERY_TOKEN")
	var in claudeHookInput
	raw, _ := io.ReadAll(stdin)
	_ = json.Unmarshal(raw, &in)
	session := id == "" || tok == ""
	if session && os.Getenv("PIGGERY_DISABLED") == "1" {
		return // started from inside a piggery session's tool (pi sets this): not a member
	}
	if hook == "PreToolUse" {
		// piggery's own tools need no permission prompt (the plugin's matcher is mcp__piggery__.*).
		if strings.HasPrefix(in.ToolName, mcpPrefix) {
			fmt.Fprintln(e.stdout, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`)
		}
		return
	}
	a, ok := h.hookEvent(hook, in)
	if !ok {
		return // an event the adapter does not report
	}
	if session {
		stderr = io.Discard
	}
	c, err := Dial(e.dir, false)
	if err != nil {
		fmt.Fprintf(stderr, "piggery hook: %v\n", err)
		return
	}
	defer c.Close()
	var join func(source string) error
	if session {
		host := sessionHost(harness)
		if host == "" {
			return
		}
		if sharedAppServer(host) { // one app-server, many threads: one host per thread
			if in.SessionID == "" {
				return // fail closed: never the plain pid host
			}
			host = threadHost(host, in.SessionID)
		}
		join = func(source string) error {
			var jr core.JoinResult
			a := core.JoinAutoArgs{Cwd: in.Cwd, Harness: harness, Mode: "interactive",
				HarnessRef: in.SessionID, Source: source, Host: host}
			// The session's transcript, in the format named after its harness (transcript_<harness>.go);
			// none: the stored one stays. A subagent's hook never joins (hookEvent drops it).
			if in.TranscriptPath != "" {
				a.Transcript = &core.Transcript{Path: in.TranscriptPath, Format: harness}
			}
			_, err := c.CallInto(proto.VerbJoinAuto, a, &jr)
			return err
		}
		if hook == "SessionStart" && join(in.Source) != nil {
			return // e.g. this session is a headless worker's: not ours to drive
		}
		c.AsHost(host)
	} else {
		c.AsParticipant(id, tok)
	}
	var r core.HarnessEventResult
	_, err = c.CallInto(proto.VerbHarnessEvent, a, &r)
	// The daemon does not know this session (it restarted: sessions are gone). A Codex session's
	// MCP server has no session id to join with, so the hook joins, and sends its event again.
	if join != nil && ruleID(err) == "host.unknown" && hook != "SessionStart" && in.SessionID != "" && join("resume") == nil {
		_, err = c.CallInto(proto.VerbHarnessEvent, a, &r)
	}
	if err != nil {
		fmt.Fprintf(stderr, "piggery hook %s: %v\n", hook, err)
		return
	}
	if out := h.hookOutput(hook, r); out != nil {
		b, _ := json.Marshal(out)
		fmt.Fprintln(e.stdout, string(b))
	}
}

// sessionHost is the host of the session this process serves: its nearest parent process named
// after one of the harnesses (a session host: claude, codex), as "<name>:<pid>:<start time>".
var sessionHost = func(harnesses ...string) string { return processHost(os.Getppid(), harnesses...) }

// threadHost is the host of one thread under a shared app-server: the app-server's host and the
// thread's session id (the hook's session_id; piggery mcp reads the same value from _meta.sessionId
// of a tool call). The daemon treats it as any other host string.
func threadHost(host, session string) string { return host + "/" + session }

// sharedAppServer reports whether host (from processHost) is a Codex app-server that serves many
// threads (Codex Desktop: one `codex app-server --listen ... --managed-daemon` with one piggery mcp
// child): `app-server` in its arguments. A piggery worker's app-server has PIGGERY_ID, so it never
// gets here (it is not a session). A var for tests.
var sharedAppServer = func(host string) bool {
	name, rest, _ := strings.Cut(host, ":")
	pid, _, _ := strings.Cut(rest, ":")
	if name != "codex" {
		return false
	}
	n, _ := strconv.Atoi(pid)
	return slices.Contains(processArgs(n), "app-server")
}

// processHost names the harness process a session lives in, "<name>:<pid>:<start time>" (start
// time: a reused pid is another host). Its hooks and its MCP server are its children (captured
// for the Claude and Codex TUI; a shell may sit in between), and it outlives /clear, which
// changes the session id but not the process. With no names, any session host. "" when no such
// process is found within a few parents.
func processHost(pid int, names ...string) string {
	if len(names) == 0 {
		names = hostHarnesses()
	}
	for range 4 {
		if pid <= 1 {
			return ""
		}
		ppid, name, ok := parentAndName(pid)
		if !ok {
			return ""
		}
		if slices.Contains(names, name) {
			if st := local.ProcessStartTime(pid); st != 0 {
				return fmt.Sprintf("%s:%d:%d", name, pid, st)
			}
			return ""
		}
		pid = ppid
	}
	return ""
}
