package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	piext "github.com/sting8k/piggery/extensions/pi"
	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
)

// `piggery mcp` is the MCP half of the Claude Code adapter: a
// stdio MCP server Claude starts as a child. It identifies the participant of PIGGERY_ID/
// PIGGERY_TOKEN with the run PIGGERY_RUN_ID (new_run false: the daemon's process row owns the
// run), serves the role's tools (send, inbox, who, agent and the declared ones) and keeps the
// connection open for pushes: wake -> one nudge line into Claude's inbox socket (C8: only a
// child of claude is let in under bypassPermissions), role -> identify again and tell Claude
// the tool list changed. Batches and acks are the daemon's (hooks, `piggery hook claude`).
//
// With no PIGGERY_* it serves a Claude session the Human opened: it is
// its claude process (host, shared with the session's hooks), placed with join.auto like the
// SessionStart hook does (the same host gives the same participant and run, whichever comes
// first, and after /clear too). The role card goes out as the server's instructions. The daemon
// not running is normal: tools answer that piggery is not running, and it keeps trying.

// mcpPrefix is how Claude names this server's tools (server key "piggery" in --mcp-config).
const mcpPrefix = "mcp__piggery__"

// builtinTool is one of the built-in model tools every adapter offers, as extensions/pi/tools.json
// defines it: a name without prefix, a text where {tool:X} is X's name for the
// reader, and a JSON Schema object for the arguments.
type builtinTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// builtinTools are the file's tools in its order (the order they are listed); mcpBaseTools their
// names, the tools every role may get.
var builtinTools, mcpBaseTools = func() ([]builtinTool, []string) {
	var f struct{ Tools []builtinTool }
	if err := json.Unmarshal(piext.Tools, &f); err != nil {
		panic("extensions/pi/tools.json: " + err.Error())
	}
	var names []string
	for _, t := range f.Tools {
		names = append(names, t.Name)
	}
	return f.Tools, names
}()

type mcpServer struct {
	dir               string
	id, token, run    string
	host              string     // a session the Human opened: its harness process (no id/token)
	harness           string     // claude or codex: the process that started this server
	shared            bool       // host is a shared app-server's: each tool call is served by the session its _meta.sessionId names (sessions)
	parent            *mcpServer // a session's server under a shared app-server: its output is the parent's
	sessions          map[string]*mcpServer
	ref, sock, sockTk string // CLAUDE_CODE_SESSION_ID, CLAUDE_CODE_MESSAGING_SOCKET/_TOKEN
	out               io.Writer
	outMu             sync.Mutex

	mu        sync.Mutex
	conn      *daemonConn
	ident     *core.IdentifyResult
	ready     chan struct{} // closed when the first identify is done and its connection is kept
	listed    bool          // Claude has read the tool list (list_changed is worth sending)
	nudges    int
	identErr  error
	readyOnce sync.Once
}

func (e *env) mcp(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%w: mcp takes no arguments", errUsage)
	}
	s := &mcpServer{
		dir: e.dir, id: os.Getenv("PIGGERY_ID"), token: os.Getenv("PIGGERY_TOKEN"), run: os.Getenv("PIGGERY_RUN_ID"),
		ref: os.Getenv("CLAUDE_CODE_SESSION_ID"), sock: os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"),
		sockTk: os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"), out: e.stdout, ready: make(chan struct{}),
	}
	if s.id == "" && s.token == "" {
		if os.Getenv("PIGGERY_DISABLED") == "1" {
			return s.serve(os.Stdin) // started from inside a piggery session's tool: no piggery here
		}
		s.host = sessionHost()
		if s.host == "" {
			return s.serve(os.Stdin)
		}
		s.harness, _, _ = strings.Cut(s.host, ":")
		if s.shared = sharedAppServer(s.host); s.shared {
			// Many threads, one process: no connection of its own. Each tool call names its thread
			// (forSession); until then the tools are a solo's.
			s.sessions = map[string]*mcpServer{}
			s.harness = s.profile().name
			return s.serve(os.Stdin)
		}
	} else if s.id == "" || s.token == "" || s.run == "" {
		return errors.New("piggery mcp: PIGGERY_ID, PIGGERY_TOKEN and PIGGERY_RUN_ID must be set (the harness runs it for a piggery worker)")
	} else if h := sessionHost(); h != "" {
		s.harness, _, _ = strings.Cut(h, ":")
	}
	s.harness = s.profile().name
	go s.keepConnected()
	return s.serve(os.Stdin)
}

// keepConnected holds one identified connection to the daemon for the life of the process,
// dialing again (no autostart: the daemon runs this worker) when it drops.
func (s *mcpServer) keepConnected() {
	delay := 200 * time.Millisecond
	for {
		c, err := s.connect()
		if err == nil {
			delay = 200 * time.Millisecond
			for p := range c.pushes {
				s.onPush(p)
			}
			// Dropped: a tool call now waits for the next connection (callTool) instead of
			// writing to this one.
			s.mu.Lock()
			if s.conn == c {
				s.conn = nil
			}
			s.mu.Unlock()
			c.close()
		} else if stale(err) && !(s.host != "" && s.ref == "" && ruleID(err) == "host.unknown") {
			s.readyOnce.Do(func() { s.identErr = err; close(s.ready) })
			return // another process owns this run, or the token is gone: stop driving it
		}
		time.Sleep(delay)
		delay = min(delay*2, 5*time.Second)
	}
}

func ruleID(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.RuleID
	}
	return ""
}

func stale(err error) bool {
	var ce *core.Error
	return errors.As(err, &ce) && (ce.Code == core.CodeUnauthorized || ce.RuleID == "run.stale")
}

func (s *mcpServer) connect() (*daemonConn, error) {
	cl, err := Dial(s.dir, false)
	if err != nil {
		return nil, err
	}
	// Without the session's id (Codex gives its MCP server none) the SessionStart hook joins it;
	// until then identify finds no session with this host (host.unknown) and we try again.
	if s.host != "" && s.ref != "" {
		wd, _ := os.Getwd()
		var jr core.JoinResult
		if _, err := cl.CallInto(proto.VerbJoinAuto, core.JoinAutoArgs{Cwd: wd, Harness: s.harness, Mode: "interactive",
			HarnessRef: s.ref, Source: "startup", Host: s.host}, &jr); err != nil {
			cl.Close()
			return nil, err
		}
	}
	if s.host != "" {
		cl.AsHost(s.host)
	} else {
		cl.AsParticipant(s.id, s.token)
	}
	c := newDaemonConn(cl)
	if err := s.identify(c); err != nil {
		c.close()
		return nil, err
	}
	s.mu.Lock()
	s.conn = c
	s.mu.Unlock()
	// Ready only now: what waits on it (tools/list, a tool call) takes the identity and the
	// connection together, and one without the other fails the call.
	s.readyOnce.Do(func() { close(s.ready) })
	return c, nil
}

func (s *mcpServer) identify(c *daemonConn) error {
	var r core.IdentifyResult
	a := core.IdentifyArgs{RunID: s.run, Harness: s.harness, HarnessRef: s.ref, ToolPrefix: mcpPrefix,
		ProtocolVersion: core.ProtocolVersion}
	if s.host != "" {
		// The run is the session's current one (auth by host). Piggery can wake it (the inbox
		// socket) and add mail to a running turn (hooks), not abort it or switch its model.
		a.Mode, a.Capabilities = "interactive", []string{core.CapWake, core.CapSteer}
	}
	err := c.call(proto.VerbIdentify, a, &r)
	if err != nil {
		return err
	}
	if note := protocolNote(r.ProtocolVersion, s.harness); note != "" && (s.ident == nil || s.ident.ProtocolVersion != r.ProtocolVersion) {
		fmt.Fprintln(os.Stderr, "piggery mcp: "+note)
	}
	s.mu.Lock()
	changed := s.ident == nil || !slices.Equal(s.ident.Tools, r.Tools)
	notify := changed && s.listed
	s.ident = &r
	s.mu.Unlock()
	if notify {
		s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
	}
	return nil
}

func (s *mcpServer) onPush(p proto.Push) {
	switch p.Event {
	case proto.EventWake:
		s.profile().wake(s, p.Ref)
	case proto.EventRole:
		s.mu.Lock()
		c := s.conn
		s.mu.Unlock()
		if c != nil {
			_ = s.identify(c)
		}
	}
}

// nudge writes one line into Claude's inbox socket so an idle session starts a turn (C7, C8);
// the mail itself comes with that turn (hook turn_start). #N differs every time: Claude drops
// identical repeats. The priority is Claude's default ("next"): "now" would abort a running tool.
func (s *mcpServer) nudge() {
	if s.sock == "" {
		return
	}
	s.mu.Lock()
	s.nudges++
	n := s.nudges
	s.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{
		"content": fmt.Sprintf("[piggery] wake #%d: you have new mail; it is shown with this turn.", n)}})
	var b strings.Builder
	if s.sockTk != "" {
		auth, _ := json.Marshal(map[string]string{"type": "auth", "token": s.sockTk})
		b.Write(auth)
		b.WriteByte('\n')
	}
	b.Write(msg)
	b.WriteByte('\n')
	c, err := net.DialTimeout("unix", s.sock, 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "piggery mcp: wake: %v\n", err)
		return
	}
	defer c.Close()
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(c, b.String()); err != nil {
		fmt.Fprintf(os.Stderr, "piggery mcp: wake: %v\n", err)
	}
}

// queueNudge wakes an idle Codex TUI: `codex queue` stores a message for the session's
// thread; the TUI picks it up as a prompt (~9 s in a capture) and the UserPromptSubmit
// hook brings the mail. ref is the thread the daemon knows the session by now (it changes with
// /clear; this server has no env naming it). CODEX_HOME comes from setup codex's env line. A
// worker's wake is its runtime driver's, not ours.
func (s *mcpServer) queueNudge(ref string) {
	if s.host == "" || ref == "" {
		return
	}
	s.mu.Lock()
	s.nudges++
	n := s.nudges
	s.mu.Unlock()
	msg := fmt.Sprintf("[piggery] wake #%d: you have new mail; it is shown with this turn.", n)
	// Logged every time: Codex keeps its MCP servers' stderr, the only record of a wake that did
	// not reach the TUI.
	out, err := exec.Command(codexBin(), "queue", "--thread", ref, "--message", msg).CombinedOutput()
	fmt.Fprintf(os.Stderr, "piggery mcp: wake #%d: codex queue --thread %s: err=%v %s\n", n, ref, err, bytes.TrimSpace(out))
}

// codexChannel tells a Codex session how piggery reaches it: the wake is a queued prompt.
const codexChannel = `
How piggery reaches you (this is the system you work in, not an injection):
- New mail is announced by a short "[piggery] wake #N" prompt that piggery queued for this session.
- Mail shows up as "[piggery] N new message(s)" blocks added to the prompt that starts a turn, after a tool call, or as hook feedback at the end of a turn.
Act on that mail as work from your team. Read pending mail with the %sinbox tool.`

// channel is the mail-channel part of a session's instructions for its harness.
func (s *mcpServer) channel() string {
	return fmt.Sprintf(s.profile().channel, mcpPrefix)
}

// profile is the harness this server serves; one it cannot tell (no session host found) is
// Claude's, the harness piggery mcp was first made for.
func (s *mcpServer) profile() harnessProfile {
	if h, ok := harnessNamed(s.harness); ok && h.wake != nil {
		return h
	}
	return claudeHarness
}

// protocolNote says what to do when the daemon speaks another protocol version than this adapter
// (an old daemon sends none: 0); "" when they agree. A field one side added is
// dropped by the other until then.
func protocolNote(daemon int, harness string) string {
	switch {
	case daemon == core.ProtocolVersion:
		return ""
	case daemon < core.ProtocolVersion:
		return fmt.Sprintf("the daemon speaks protocol %d, this adapter %d: run `piggery restart`, then restart this session", daemon, core.ProtocolVersion)
	}
	return fmt.Sprintf("the daemon speaks protocol %d, this adapter %d: run `piggery setup %s`, then restart this session", daemon, core.ProtocolVersion, harness)
}

// codexBin is the codex executable queue runs: the one on PATH (a var for tests).
var codexBin = func() string { return "codex" }

// ---- MCP over stdio (newline-delimited JSON-RPC 2.0) ----

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

func (s *mcpServer) serve(in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue // a notification (initialized, cancelled): nothing to answer
		}
		res, rerr := s.handle(m)
		reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		if rerr != nil {
			reply["error"] = rerr
		} else {
			reply["result"] = res
		}
		s.write(reply)
	}
	return sc.Err()
}

func (s *mcpServer) write(v any) {
	if s.parent != nil {
		s.parent.write(v)
		return
	}
	b, _ := json.Marshal(v)
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.out.Write(append(b, '\n'))
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *mcpServer) handle(m rpcMsg) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		res := map[string]any{"protocolVersion": p.ProtocolVersion,
			"capabilities": map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":   map[string]any{"name": "piggery", "version": Version}}
		if s.host != "" && !s.shared {
			// A session's role card and how mail reaches it (captured: enough for the model to
			// trust and act on mail). A worker has both in its system prompt already. A later
			// role change reaches the model with the next hook (the daemon adds the new card).
			card, note := "", ""
			if id, _ := s.identityWithin(2 * time.Second); id != nil {
				card, note = id.RoleCard, protocolNote(id.ProtocolVersion, s.harness)
			}
			res["instructions"] = card + s.channel()
			if note != "" {
				res["instructions"] = res["instructions"].(string) + "\n\npiggery: " + note
			}
		}
		return res, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolList()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				SessionID string `json:"sessionId"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcError{-32602, "bad params: " + err.Error()}
		}
		target := s
		if s.shared {
			target = s.forSession(p.Meta.SessionID)
			if target == nil { // fail closed: never another thread's identity
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": "piggery cannot tell which Codex thread this call is from (no _meta.sessionId): refused"}}, "isError": true}, nil
			}
		}
		text, err := target.callTool(p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": errText(err)}}, "isError": true}, nil
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
	}
	return nil, &rpcError{-32601, "method not found: " + m.Method}
}

// forSession is the server of the thread session under a shared app-server, made at its first call:
// its own host (threadHost) and daemon connection, so a wake for that thread goes out on that
// connection. nil when the call names no session.
func (s *mcpServer) forSession(session string) *mcpServer {
	if session == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.sessions[session]
	if !ok {
		t = &mcpServer{dir: s.dir, host: threadHost(s.host, session), harness: s.harness, parent: s, ready: make(chan struct{})}
		s.sessions[session] = t
		go t.keepConnected()
	}
	return t
}

func errText(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		if ce.RuleID != "" {
			return fmt.Sprintf("refused (%s): %s", ce.RuleID, ce.Message)
		}
		return ce.Message
	}
	return err.Error()
}

// identity waits (bounded) for the first identify: Claude lists the tools right after it starts us.
// A session the Human opened waits less: no daemon running is normal there.
func (s *mcpServer) identity() (*core.IdentifyResult, *daemonConn) {
	if s.host != "" {
		return s.identityWithin(2 * time.Second)
	}
	return s.identityWithin(10 * time.Second)
}

// mcpConnectWait is how long a tool call waits for a connection to the daemon (a restart).
const mcpConnectWait = 10 * time.Second

func (s *mcpServer) identityWithin(d time.Duration) (*core.IdentifyResult, *daemonConn) {
	select {
	case <-s.ready:
	case <-time.After(d):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listed = true; s.ident == nil {
		return nil, nil
	}
	if s.conn != nil && s.conn.isDead() {
		return s.ident, nil
	}
	return s.ident, s.conn
}

func (s *mcpServer) toolList() []any {
	var id *core.IdentifyResult
	if !s.shared { // a shared app-server has no identity of its own to wait for
		id, _ = s.identity()
	}
	tools := []any{}
	if id == nil && s.host != "" {
		// Not placed yet (no daemon): a solo's tools, like pi; calling one starts the daemon.
		id = &core.IdentifyResult{Tools: mcpBaseTools}
	}
	if id == nil {
		return tools
	}
	for _, t := range mcpBaseTools {
		if slices.Contains(id.Tools, t) {
			tools = append(tools, baseTool(t))
		}
	}
	return tools
}

// baseTool is the MCP definition of a built-in tool (the file's, with this server's tool names).
func baseTool(name string) map[string]any {
	i := slices.IndexFunc(builtinTools, func(t builtinTool) bool { return t.Name == name })
	t := builtinTools[i]
	return map[string]any{"name": t.Name, "description": core.WithToolNames(t.Description, mcpPrefix), "inputSchema": t.Parameters}
}

func (s *mcpServer) callTool(name string, raw json.RawMessage) (string, error) {
	id, c := s.identity()
	// No connection now: a session the Human opened asks for the daemon (the model called a piggery
	// tool, so its user wants piggery); a worker that was placed before waits for keepConnected to
	// dial again (the daemon restarted). A call already written to a connection that then dropped
	// is not here: it fails as it did, since the old daemon may have run it.
	if (id == nil || c == nil) && s.identErr == nil && (s.host != "" || id != nil) {
		if s.host != "" {
			if cl, err := Dial(s.dir, true); err == nil {
				cl.Close()
			}
		}
		for end := time.Now().Add(mcpConnectWait); time.Now().Before(end) && (id == nil || c == nil); {
			time.Sleep(50 * time.Millisecond)
			id, c = s.identityWithin(0)
		}
	}
	if id == nil || c == nil {
		if s.identErr != nil {
			return "", s.identErr
		}
		return "", errors.New("piggery is not reachable right now; try again")
	}
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	allowed := slices.Contains(id.Tools, name)
	switch {
	case !allowed:
		return "", fmt.Errorf("your role has no tool %q", name)
	case name == "send":
		var r core.SendResult
		if err := c.call(proto.VerbSend, raw, &r); err != nil {
			return "", err
		}
		return core.SentText(r), nil
	case name == "inbox":
		var a struct {
			View string `json:"view"`
		}
		_ = json.Unmarshal(raw, &a)
		var msgs []core.Delivered
		// No batch: while a turn is open the daemon records the delivery in its batch.
		if err := c.call(proto.VerbInbox, core.InboxArgs{View: a.View}, &msgs); err != nil {
			return "", err
		}
		switch {
		case len(msgs) == 0 && a.View != "":
			return "Nothing in this view.", nil
		case len(msgs) == 0:
			return "No new messages.", nil
		case a.View != "":
			return core.RenderMail(msgs, fmt.Sprintf("view %s, %d message(s), nothing marked read", a.View, len(msgs)), mcpPrefix, time.Now()), nil
		}
		return core.RenderMail(msgs, "", mcpPrefix, time.Now()), nil
	case name == "who":
		var ps []core.Presence
		if err := c.call(proto.VerbWho, nil, &ps); err != nil {
			return "", err
		}
		return core.RenderWho(ps, id.ParticipantID), nil
	case name == "agent":
		var a core.AgentArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		var r core.AgentResult
		if err := c.call(proto.VerbAgent, a, &r); err != nil {
			return "", err
		}
		if (a.Action == core.AgentFound || a.Action == core.AgentReopen || (a.Action == core.AgentClose && a.Team == "")) && s.host != "" {
			// This session is now another participant, or the same one in another role: identify
			// again (auth by host follows it) for its tools and card; Claude re-reads the tools.
			if err := s.identify(c); err != nil {
				return "", err
			}
			id, _ = s.identityWithin(0)
			tools := make([]string, len(id.Tools))
			for i, t := range id.Tools {
				tools[i] = mcpPrefix + t
			}
			yours := "your tools: " + strings.Join(tools, ", ")
			if a.Action == core.AgentFound {
				return fmt.Sprintf("founded team %s; you are %s (%s), its gate; %s", r.TeamName, id.Name, id.Role, yours), nil
			}
			return agentText(a, r) + "\n" + yours, nil
		}
		return agentText(a, r), nil
	}
	return "", fmt.Errorf("your role has no tool %q", name)
}

// agentText is the model-facing result of an agent action (names and #N only).
func agentText(a core.AgentArgs, r core.AgentResult) string {
	switch {
	case r.Text != "":
		return r.Text
	case a.Action == core.AgentTail:
		lines := make([]string, len(r.Records))
		for i, rec := range r.Records {
			lines[i] = string(rec)
		}
		if len(lines) == 0 {
			return "(no output)"
		}
		return strings.Join(lines, "\n")
	case r.Exit != nil:
		b, _ := json.Marshal(r.Exit)
		return "stopped (exit " + string(b) + ")"
	case a.Action == core.AgentSpawn && a.Template != "":
		return fmt.Sprintf("called up taskforce %s; its task is #%d; write to it as %s, its result comes to you as mail", r.TeamName, r.TaskSeq, r.TeamName)
	case a.Action == core.AgentClose && a.Team != "":
		return fmt.Sprintf("closed taskforce %s; stopped: %s", r.TeamName, strings.Join(r.Stopped, ", "))
	case a.Action == core.AgentClose:
		return fmt.Sprintf("closed team %s; stopped: %s", r.TeamName, strings.Join(r.Stopped, ", "))
	case a.Action == core.AgentSpawn:
		return fmt.Sprintf("spawned %s; its task is #%d (its reply comes to you as mail)", a.Name, r.TaskSeq)
	case a.Action == core.AgentResume && r.TaskSeq != 0:
		return fmt.Sprintf("resumed %s; its task is #%d (its reply comes to you as mail)", a.Target, r.TaskSeq)
	}
	return fmt.Sprintf("%s ok: %s", a.Action, a.Target)
}

// ---- a daemon connection that also carries pushes ----

// daemonConn is one identified connection: responses are matched to calls by id, pushes (no
// id) go to the pushes channel, which closes when the connection drops.
type daemonConn struct {
	cl      *Client
	mu      sync.Mutex
	pending map[string]chan proto.Response
	pushes  chan proto.Push
	dead    chan struct{}
}

func newDaemonConn(cl *Client) *daemonConn {
	c := &daemonConn{cl: cl, pending: map[string]chan proto.Response{}, pushes: make(chan proto.Push, 16), dead: make(chan struct{})}
	go c.read()
	return c
}

func (c *daemonConn) read() {
	defer func() {
		close(c.dead)
		close(c.pushes)
	}()
	for {
		b, err := c.cl.r.ReadBytes('\n')
		if err != nil {
			return
		}
		var f struct {
			ID    string `json:"id"`
			Event string `json:"event"`
		}
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		if f.ID == "" && f.Event != "" {
			var p proto.Push
			if json.Unmarshal(b, &p) == nil {
				c.pushes <- p
			}
			continue
		}
		var resp proto.Response
		if json.Unmarshal(b, &resp) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (c *daemonConn) close() { c.cl.Close() }

func (c *daemonConn) isDead() bool {
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// call sends one request and decodes its result into out (nil: ignore it).
func (c *daemonConn) call(verb string, args, out any) error {
	c.mu.Lock()
	c.cl.next++
	req := proto.Request{ID: strconv.Itoa(c.cl.next), Verb: verb, Auth: c.cl.auth}
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		req.Args = raw
	}
	ch := make(chan proto.Response, 1)
	c.pending[req.ID] = ch
	line, _ := json.Marshal(req)
	_, err := c.cl.conn.Write(append(line, '\n'))
	c.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-c.dead:
		return errors.New("piggery: the daemon connection dropped; try again")
	}
}
