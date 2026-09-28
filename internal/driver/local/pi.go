package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// The pi codec: `pi --mode rpc` with the human's pi setup
// minus the blacklist; stdout records logged for tail except streaming deltas; blocking
// extension UI dialogs cancelled so pi never waits; admin commands as pi rpc on stdin.

// Harness is what the pi driver's workers run (core compares it with a caller's harness).
const Harness = "pi"

// Profile is ~/.piggery/harness/pi.json: the worker command without session id and model.
type Profile struct {
	Cmd   string   `json:"cmd"`
	Args  []string `json:"args"`
	Model string   `json:"model"`
	// Thinking is pi's --thinking for workers the chain gives none to, as pi names levels.
	Thinking string `json:"thinking"`
	// Blacklist names extensions/packages of the human's pi setup a worker must not load
	// (npm package, git repo, or local path basename); see agentdir.go.
	Blacklist []string `json:"blacklist"`
	// TestedVersions are the pi versions piggery was tested with (`pi --version`); doctor warns
	// about another.
	TestedVersions []string `json:"tested_versions"`
}

// New returns the pi driver over dir.
func New(dir string, opts Options) *Driver { return newWith(dir, opts, piCodec{dir: dir}) }

// ProfilePath is where the pi worker profile is read from.
func ProfilePath(dir string) string { return filepath.Join(dir, "harness", "pi.json") }

type piCodec struct{ dir string }

func (piCodec) harness() string { return Harness }

func (piCodec) toolPrefix() string { return "piggery_" }

func (c piCodec) defaults() (string, string) {
	p, _ := c.profile()
	return p.Model, p.Thinking
}

func (c piCodec) profile() (Profile, error) {
	var p Profile
	b, err := os.ReadFile(ProfilePath(c.dir))
	if errors.Is(err, os.ErrNotExist) {
		return p, fmt.Errorf("no worker profile at %s (see harness/pi.example.json)", ProfilePath(c.dir))
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("worker profile %s: %w", ProfilePath(c.dir), err)
	}
	if p.Cmd == "" {
		return p, fmt.Errorf("worker profile %s: cmd is required", ProfilePath(c.dir))
	}
	inheritEmpty(&p.Model, &p.Thinking)
	return p, nil
}

func (c piCodec) launch(s core.Spec) (launch, error) {
	prof, err := c.profile()
	if err != nil {
		return launch{}, err
	}
	// The worker loads the human's pi setup minus the blacklist, through its own agent dir.
	// --no-extensions (profiles written before that) would drop it all, so it is not passed;
	// the -e extensions (piggery) are loaded as given and skipped in the setup.
	var args, skip []string
	for i, a := range prof.Args {
		if a == "--no-extensions" {
			continue
		}
		if i > 0 && (prof.Args[i-1] == "-e" || prof.Args[i-1] == "--extension") {
			skip = append(skip, a)
		}
		args = append(args, a)
	}
	agentDir, err := c.newAgentDir(s, prof.Blacklist, skip)
	if err != nil {
		return launch{}, fmt.Errorf("worker agent dir: %w", err)
	}
	args = append(args, "--session-id", s.HarnessRef)
	if m := firstNonEmpty(s.Model, prof.Model); m != "" {
		args = append(args, "--model", m)
	}
	thinking := firstNonEmpty(s.Thinking, prof.Thinking) // passed as declared: pi's own levels
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	return launch{
		cmd:      prof.Cmd,
		args:     args,
		env:      []string{"PI_CODING_AGENT_DIR=" + agentDir},
		thinking: thinking,
		cleanup:  func() { os.RemoveAll(agentDir) },
	}, nil
}

// newAgentDir builds the agent dir of run s. Dirs of the participant's earlier runs are removed
// first: core starts a run only once the previous one has ended (spawn, or resume of a gone
// or verified-dead worker), and a daemon restart can leave them behind.
func (c piCodec) newAgentDir(s core.Spec, blacklist, skip []string) (string, error) {
	parent := filepath.Join(AgentDirRoot(c.dir), s.ParticipantID)
	old, err := os.ReadDir(parent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, e := range old {
		if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(parent, s.RunID)
	home, _ := os.UserHomeDir()
	// -e paths resolve against the worker's cwd, as pi resolves them.
	bl := newBlacklist(blacklist, skip, home, s.Cwd)
	if err := buildAgentDir(HumanAgentDir(AgentDirRoot(c.dir)), dir, home, bl); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// started: pi warns about a level it does not know and runs another one, so a worker that
// does not run the declared level does not start.
func (piCodec) started(ctx context.Context, w *worker, l launch) error {
	if l.thinking == "" {
		return nil
	}
	return w.checkThinking(ctx, startTimeout, l.thinking)
}

// Streaming records tail does not need.
var piSkip = map[string]bool{"message_update": true, "tool_execution_update": true}

// Extension UI methods that block pi until answered (rpc-extension-ui.md "Dialog methods").
var piDialog = map[string]bool{"select": true, "confirm": true, "input": true, "editor": true}

func (piCodec) record(_ *worker, line []byte) record {
	var rec struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if json.Unmarshal(line, &rec) != nil || piSkip[rec.Type] {
		return record{} // not a protocol record, or a streaming delta
	}
	r := record{keep: true}
	if rec.Type == "response" && rec.ID != "" {
		r.answers = rec.ID
	}
	if rec.Type == "extension_ui_request" && piDialog[rec.Method] {
		r.reply, _ = json.Marshal(map[string]any{"type": "extension_ui_response", "id": rec.ID, "cancelled": true})
	}
	return r
}

// Admin quick actions on a live pi worker: pi rpc commands on stdin.

// commandTimeout bounds how long a command waits for pi's response; startTimeout bounds the
// get_state that checks a new worker's thinking level (pi answers once it has started).
var (
	commandTimeout = 10 * time.Second
	startTimeout   = 30 * time.Second
)

var nextID atomic.Int64

// rpcResponse is pi's answer to a command sent with an id (pi docs/rpc.md "Responses").
type rpcResponse struct {
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// abort asks pi to cancel the current turn (rpc abort). It does not wait: pi answers only once
// the session is idle, and the worker stays alive either way.
func (piCodec) abort(w *worker) error { return w.send(map[string]string{"type": "abort"}) }

// setModel switches the model (rpc set_model, effective from the next turn) and waits for
// pi's response: a refusal is an error.
func (piCodec) setModel(ctx context.Context, w *worker, model string) error {
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" {
		return fmt.Errorf("model %q: want provider/model", model)
	}
	_, err := w.call(ctx, commandTimeout, map[string]string{"type": "set_model", "provider": provider, "modelId": id})
	return err
}

// setThinking sets the thinking level (rpc set_thinking_level), then checks that pi runs it:
// pi accepts levels it does not keep, so the check is get_state. On a mismatch
// the level it ran before is set again, so a refused change leaves the worker as it was (seen
// live: pi took "bogus" and moved the worker from max to minimal).
func (piCodec) setThinking(ctx context.Context, w *worker, level string) error {
	before, err := w.thinking(ctx, commandTimeout)
	if err != nil {
		return err
	}
	if _, err := w.call(ctx, commandTimeout, map[string]string{"type": "set_thinking_level", "level": level}); err != nil {
		return err
	}
	ran, err := w.thinking(ctx, commandTimeout)
	if err != nil || ran == level {
		return err
	}
	if _, err := w.call(ctx, commandTimeout, map[string]string{"type": "set_thinking_level", "level": before}); err != nil {
		return fmt.Errorf("thinking level: pi ran %q, not %q; setting %q back failed: %w", ran, level, before, err)
	}
	if back, err := w.thinking(ctx, commandTimeout); err != nil || back != before {
		return fmt.Errorf("thinking level: pi ran %q, not %q; it now runs %q, not %q as before", ran, level, back, before)
	}
	return fmt.Errorf("thinking level: pi ran %q, not %q (%q kept)", ran, level, before)
}

// checkThinking fails unless pi runs the thinking level want.
func (w *worker) checkThinking(ctx context.Context, timeout time.Duration, want string) error {
	ran, err := w.thinking(ctx, timeout)
	if err == nil && ran != want {
		err = fmt.Errorf("thinking level: pi ran %q, not %q", ran, want)
	}
	return err
}

// thinking is the thinking level pi runs now (rpc get_state).
func (w *worker) thinking(ctx context.Context, timeout time.Duration) (string, error) {
	r, err := w.call(ctx, timeout, map[string]string{"type": "get_state"})
	if err != nil {
		return "", err
	}
	var st struct {
		ThinkingLevel string `json:"thinkingLevel"`
	}
	json.Unmarshal(r.Data, &st)
	return st.ThinkingLevel, nil
}

// call sends one pi rpc command with an id and waits for pi's response to it; a refusal is an
// error naming the command.
func (w *worker) call(ctx context.Context, timeout time.Duration, cmd map[string]string) (rpcResponse, error) {
	reqID := "piggery-" + strconv.FormatInt(nextID.Add(1), 10)
	cmd["id"] = reqID
	typ := cmd["type"]
	line, err := w.request(ctx, timeout, reqID, typ, cmd)
	if err != nil {
		return rpcResponse{}, err
	}
	var r rpcResponse
	json.Unmarshal(line, &r)
	if !r.Success {
		return r, fmt.Errorf("pi refused %s: %s", typ, r.Error)
	}
	return r, nil
}
