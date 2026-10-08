package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sting8k/piggery/internal/core"
)

// The omp codec: `omp --mode rpc` (oh-my-pi, a pi fork) speaks pi's rpc, so it is the pi codec
// (abort, set_model, set_thinking_level and the stdout records are pi's) with what differs:
//   - no --session-id: omp mints the session id, which start reads from get_state and returns as
//     the worker's harness_ref; a resumed run passes it back as --resume. Sessions are kept in
//     piggery's own --session-dir, not the per-run agent dir that is removed when the run ends.
//   - the settings file is config.yml (YAML), filtered like pi's settings.json.
//   - omp reads a project's .mcp.json (and starts its servers, with the worker's PIGGERY_* env):
//     a --config overlay turns that off for the worker.
//   - a named profile in the environment (OMP_PROFILE, PI_PROFILE) would move omp off the
//     per-run agent dir: the worker gets both empty.
//   - it clamps a thinking level it does not have, as pi does, so start and set_thinking_level
//     check get_state; it also answers commands sent before its `ready` frame, after it.
//   - set_model resets the thinking level to max (seen live): setModel puts the level back.
//
// The extension is not the profile's: launch has EnsureOmpWorkerExt make piggery's own copy
// current and loads it with -e. The captures this rests on are testdata/fixtures/omp/ (omp 18.4.2).

// OmpHarness is what the omp driver's workers run.
const OmpHarness = "omp"

// OmpProfilePath is where the omp worker profile is read from: the pi profile's shape.
func OmpProfilePath(dir string) string { return filepath.Join(dir, "harness", "omp.json") }

// DefaultOmpProfile is the default omp.json: the rpc mode, nothing else (the worker's extension
// is added by the driver).
var DefaultOmpProfile = Profile{Cmd: "omp", Args: []string{"--mode", "rpc"}, Model: Inherit, Thinking: Inherit,
	Blacklist: []string{}, TestedVersions: []string{"18.4.2"}}

// ompWorkerConfig is the --config overlay of a worker (omp's settings, for this run only):
// no MCP server from the project's root .mcp.json/mcp.json.
const ompWorkerConfig = "mcp:\n  enableProjectConfig: false\n"

// NewOmp returns the omp driver over dir.
func NewOmp(dir string, opts Options) *Driver {
	return newWith(dir, opts, ompCodec{piCodec{dir: dir}})
}

type ompCodec struct {
	piCodec
}

func (ompCodec) harness() string { return OmpHarness }

func (c ompCodec) defaults() (string, string) {
	p, _ := c.profile()
	return p.Model, p.Thinking
}

func (c ompCodec) profile() (Profile, error) {
	return readProfile(OmpProfilePath(c.dir), fmt.Sprintf("no omp worker profile at %s (run piggery setup)", OmpProfilePath(c.dir)))
}

func (c ompCodec) launch(s core.Spec) (launch, error) {
	prof, err := c.profile()
	if err != nil {
		return launch{}, err
	}
	args, skip := workerArgs(prof.Args)
	entry, err := EnsureOmpWorkerExt(c.dir)
	if err != nil {
		return launch{}, fmt.Errorf("worker extension: %w", err)
	}
	args, skip = append(args, "-e", entry), append(skip, entry)
	root := OmpAgentDirRoot(c.dir)
	agentDir, err := newRunAgentDir(root, OmpHumanAgentDir(root), ompSettings, s, prof.Blacklist, skip)
	if err != nil {
		return launch{}, fmt.Errorf("worker agent dir: %w", err)
	}
	fail := func(err error) (launch, error) {
		removeRun(agentDir)
		return launch{}, err
	}
	overlay := filepath.Join(agentDir, "piggery-worker.yml")
	if err := os.WriteFile(overlay, []byte(ompWorkerConfig), 0o600); err != nil {
		return fail(err)
	}
	moveOmpSessions(c.dir, s.ParticipantID)
	sessions := OmpSessionsDir(c.dir, s.ParticipantID)
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		return fail(err)
	}
	args = append(args, "--config", overlay, "--session-dir", sessions)
	if s.Resume {
		if s.HarnessRef == "" {
			return fail(errors.New("resume: the worker has no omp session id"))
		}
		args = append(args, "--resume", s.HarnessRef)
	}
	if m := firstNonEmpty(s.Model, prof.Model); m != "" {
		args = append(args, "--model", m)
	}
	thinking := firstNonEmpty(s.Thinking, prof.Thinking) // passed as declared: omp's own levels
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	return launch{
		cmd:  prof.Cmd,
		args: args,
		// Empty, not unset (the runner sets keys): omp reads an empty OMP_PROFILE as the default profile.
		env:      []string{"PI_CODING_AGENT_DIR=" + agentDir, "OMP_PROFILE=", "PI_PROFILE="},
		thinking: thinking,
		cleanup:  func() { removeRun(agentDir) },
	}, nil
}

// started reads get_state (omp answers it once it has sent `ready`): its session id is the
// worker's harness_ref, and the thinking level must be the declared one, which omp clamps
// silently. A run that ends before answering (an unknown model or session id makes omp exit
// before `ready`) fails with what omp wrote on stderr.
func (ompCodec) started(ctx context.Context, w *worker, l launch) error {
	st, err := w.state(ctx, startTimeout)
	if err != nil {
		if tail := w.stderrTail(); tail != "" && errors.Is(err, core.ErrNotRunning) {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	if st.SessionID == "" {
		return errors.New("omp get_state: no sessionId")
	}
	w.harnessRef = st.SessionID
	if l.thinking != "" {
		return w.thinkingIs(st.ThinkingLevel, l.thinking)
	}
	return nil
}

// setModel is pi's, then the thinking level the worker ran before is set again: omp's set_model
// moves it to max (rpc-set-model-resets-thinking), and what ps shows is the stored level.
func (c ompCodec) setModel(ctx context.Context, w *worker, model string) error {
	before, err := w.thinking(ctx, commandTimeout)
	if err != nil {
		return err
	}
	if err := c.piCodec.setModel(ctx, w, model); err != nil {
		return err
	}
	if ran, err := w.thinking(ctx, commandTimeout); err != nil || ran == before {
		return err
	}
	return c.piCodec.setThinking(ctx, w, before)
}

// stderrTail is the end of what the worker's process wrote on stderr, on one line.
func (w *worker) stderrTail() string {
	b, err := os.ReadFile(strings.TrimSuffix(w.logPth, ".jsonl") + ".stderr")
	if err != nil {
		return ""
	}
	s := strings.Join(strings.Fields(string(b)), " ")
	if r := []rune(s); len(r) > 300 {
		s = "…" + string(r[len(r)-300:])
	}
	return s
}

// Records tail does not need: pi's streaming ones, and omp's advisor cost ticks and its list of
// slash commands (30 KB, sent again at each change).
var ompSkip = map[string]bool{"message_update": true, "tool_execution_update": true,
	"available_commands_update": true, "advisor_cost_changed": true}

// record is pi's, except that two records are not kept: an agent_end with isTerminal false (omp
// runs on: a steer, a background job's result or a continuation follows), so the log has one
// agent_end per run, where the turn is over; and the answer to get_state, which carries the
// system prompt and every tool's schema (12 KB) and which start and each thinking check ask for.
// The request waiting for it still gets it.
func (ompCodec) record(_ *worker, line []byte) record {
	r := piRecord(line, ompSkip)
	if !r.keep {
		return r
	}
	switch {
	case bytes.Contains(line, []byte(`"agent_end"`)):
		var e struct {
			Type       string `json:"type"`
			IsTerminal *bool  `json:"isTerminal"`
		}
		if json.Unmarshal(line, &e) == nil && e.Type == "agent_end" && e.IsTerminal != nil && !*e.IsTerminal {
			r.keep = false
		}
	case r.answers != "" && bytes.Contains(line, []byte(`"command":"get_state"`)):
		var e struct {
			Command string `json:"command"`
			Success bool   `json:"success"`
		}
		if json.Unmarshal(line, &e) == nil && e.Command == "get_state" && e.Success {
			r.keep = false
		}
	}
	return r
}
