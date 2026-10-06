package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// RuntimeDriver starts and stops headless worker processes. Core decides (gate, rows, mail);
// the driver owns processes. No harness names reach core: the driver maps a Spec to a command.
type RuntimeDriver interface {
	// Start launches the worker for s.RunID and returns once the process exists.
	Start(ctx context.Context, s Spec) (Proc, error)
	// Stop ends the participant's current process (close stdin, then SIGTERM, then SIGKILL on
	// its process group) and reports how it exited.
	Stop(ctx context.Context, participantID string) (Exit, error)
	// Tail returns the last `lines` normalized stdout records of the participant's current run.
	Tail(participantID string, lines int) ([]json.RawMessage, error)
	// Abort asks the live worker's harness to cancel its current turn; it stays alive.
	Abort(participantID string) error
	// SetModel switches the live worker's model and reports whether the harness accepted it.
	SetModel(ctx context.Context, participantID, model string) error
	// Models lists the models the live worker's harness offers, each as SetModel takes it, in the
	// order the harness gives them; only for a driver that declares CapListModels.
	Models(ctx context.Context, participantID string) ([]string, error)
	// SetThinking sets the live worker's thinking level and reports whether the harness runs it.
	SetThinking(ctx context.Context, participantID, level string) error
	// Kill ends the live worker's process group at once (SIGKILL) and reports the exit.
	Kill(ctx context.Context, participantID string) (Exit, error)
	// The three wrap ErrNotRunning when the driver has no live process for the participant.

	// Defaults names the harness its workers run (compared for equality only, never branched
	// on) and the model and thinking level its profile sets ("" none): links of the worker chains.
	Defaults() (harness, model, thinking string)
	// Capabilities is what the driver's workers support (Cap*).
	Capabilities() []string
	// Wake starts a turn of the idle worker with a nudge that carries no mail (the mail comes
	// through its adapter), for a harness whose adapter cannot wake it (Codex). ErrNoWake: the
	// worker's adapter wakes it (the notify hook's push).
	Wake(participantID string) error
	// ToolPrefix is how the driver's harness names piggery's tools for the model (pi:
	// "piggery_"); core writes the role card it passes in Spec with it.
	ToolPrefix() string
	// Deliver gives the live worker a batch of mail (deliver(batch)), idle (a new turn) or busy (it
	// joins the turn); only for a driver that declares CapDeliver. The driver reports the batch's
	// end through Engine.DeliveryEnded. An error: the batch ends unacked.
	Deliver(participantID string, d Delivery) error

	// Inspect checks a recorded process against the OS (recovery): dead, ours (pid alive, start
	// time and cmdline match), or reused (pid alive but another process). An error means the check
	// itself could not run.
	Inspect(ctx context.Context, p Proc) (ProcState, error)
	// KillVerified re-inspects p and signals its process group (TERM, then KILL) only while
	// it is still ours; it never signals a dead or reused pid.
	KillVerified(ctx context.Context, p Proc) (Exit, error)
	// StopAll stops every live worker (graceful daemon shutdown), recording exits via the
	// driver's exit hook.
	StopAll(ctx context.Context)
}

// ProcState is the result of RuntimeDriver.Inspect.
type ProcState string

const (
	ProcDead   ProcState = "dead"
	ProcOurs   ProcState = "ours"
	ProcReused ProcState = "reused"
)

// Spec is what a driver needs to start one run of a worker.
type Spec struct {
	ParticipantID string `json:"participant_id"`
	RunID         string `json:"run_id"`
	Token         string `json:"-"`           // PIGGERY_TOKEN for the worker; never the admin token
	Cwd           string `json:"cwd"`         // normalized
	HarnessRef    string `json:"harness_ref"` // harness session id to create or resume
	Model         string `json:"model,omitempty"`
	Thinking      string `json:"thinking,omitempty"` // as declared; the driver fails a start the harness does not run
	Harness       string `json:"harness"`            // the driver that runs it
	Resume        bool   `json:"resume,omitempty"`   // HarnessRef names an existing session to continue
	// AllowTools are native tools of the harness the role keeps though its profile disallows
	// them (roles.<role>.spawn.allow_tools).
	AllowTools []string `json:"allow_tools,omitempty"`
	// RoleCard is the worker's role card, for a harness that takes it at start (Claude's
	// --append-system-prompt); others get it at identify.
	RoleCard string `json:"-"`
}

// Proc is the started process, recorded durably for recovery.
type Proc struct {
	PID       int      `json:"pid"`
	PGID      int      `json:"pgid"`
	StartTime int64    `json:"start_time"` // OS process start time, unix ms
	Cmdline   []string `json:"cmdline"`
	// HarnessRef is the session id the harness chose, for a harness that names its own
	// sessions (Codex's thread id): core keeps it as the worker's harness_ref, to resume. ""
	// = the Spec's.
	HarnessRef string `json:"harness_ref,omitempty"`
}

// Exit is how a process ended.
type Exit struct {
	Code   int    `json:"code"`             // -1 when killed by a signal
	Signal string `json:"signal,omitempty"` // e.g. "SIGTERM", "SIGKILL"
	At     int64  `json:"at"`               // unix ms
}

// ErrNotRunning: the driver has no live process for the participant.
var ErrNotRunning = errors.New("not running")

// Delivery is one batch of mail for a worker whose driver delivers it: batch n of run RunID,
// its mail rendered as the model reads it.
type Delivery struct {
	RunID string
	Batch int64
	Text  string
}

// How a delivered batch ended (Engine.DeliveryEnded).
const (
	DeliveryCompleted = "completed" // the harness finished the message: its mail is acked
	DeliveryCancelled = "cancelled" // the harness dropped it (abort): not acked, the mail is given again
)

// A turn of a delivering worker that no delivered batch started (Engine.UnbatchedTurn).
const (
	TurnStarted = "turn_started"
	TurnEnded   = "turn_ended"
)

// DeliveryEnd is a driver's report that batch Batch of a run ended with Outcome.
type DeliveryEnd struct {
	Batch   int64
	Outcome string
}

// ErrNoWake is RuntimeDriver.Wake's answer when the worker's adapter wakes it.
var ErrNoWake = errors.New("the adapter wakes this worker")

// WithRuntime adds the driver of one harness (its Defaults names it) for the agent actions.
// Without any every action is unsupported.
func WithRuntime(d RuntimeDriver) Option {
	return func(e *Engine) { e.runtimes = append(e.runtimes, d) }
}

// WithDefaultHarness names the harness of workers whose role and main session name none
// (config.yaml `harness`); default: the first driver's.
func WithDefaultHarness(h string) Option { return func(e *Engine) { e.defaultHarness = h } }

// WithAllowedRoots are directories outside a team's root where a worker may be spawned (config.yaml
// `spawn.allowed_roots`).
func WithAllowedRoots(dirs []string) Option { return func(e *Engine) { e.allowedRoots = dirs } }

// WithSharedPrompts sets where a role card gets the Human's shared text (config.yaml `prompts`): f
// returns the text for a role of a template ("" for none); what f cannot read it skips itself.
// A solo asks with template "" and role "solo". Core only passes the names; the text is opaque.
func WithSharedPrompts(f func(template, role string) string) Option {
	return func(e *Engine) { e.sharedPrompts = f }
}

// driverNamed is the driver of harness h, nil when none runs it.
func (e *Engine) driverNamed(h string) RuntimeDriver {
	for _, d := range e.runtimes {
		if name, _, _ := d.Defaults(); name == h {
			return d
		}
	}
	return nil
}

// runtimeFor is the driver of harness h ("" = the default harness: a worker recorded before
// workers had one). With a single driver every worker runs on it. nil when none fits.
// driverCaps is the capabilities column for a headless worker of harness h.
func (e *Engine) driverCaps(h string) any {
	d := e.runtimeFor(h)
	if d == nil {
		return nil
	}
	return capsJSON(d.Capabilities())
}

// capsJSON is caps as stored (a JSON array; NULL for none declared).
func capsJSON(caps []string) any {
	if caps == nil {
		return nil
	}
	b, _ := json.Marshal(caps)
	return string(b)
}

// lacksCap reports whether stored capabilities were declared and leave out c. NULL (an adapter
// or client older than capabilities declared nothing) refuses nothing, as before them.
func lacksCap(stored sql.NullString, c string) bool {
	if !stored.Valid {
		return false
	}
	var caps []string
	json.Unmarshal([]byte(stored.String), &caps)
	return !contains(caps, c)
}

func (e *Engine) runtimeFor(h string) RuntimeDriver {
	if len(e.runtimes) == 1 {
		return e.runtimes[0]
	}
	if h == "" {
		h = e.defaultHarnessName()
	}
	return e.driverNamed(h)
}

func (e *Engine) defaultHarnessName() string {
	if e.defaultHarness == "" && len(e.runtimes) > 0 {
		h, _, _ := e.runtimes[0].Defaults()
		return h
	}
	return e.defaultHarness
}

// Agent actions.
const (
	AgentSpawn     = "spawn"
	AgentStop      = "stop"
	AgentResume    = "resume"
	AgentTail      = "tail"
	AgentFound     = "found"
	AgentAdmit     = "admit"
	AgentTemplates = "templates"
	AgentClose     = "close"
	AgentReopen    = "reopen"
)

// AgentArgs is the `agent` tool. Fields by action:
// spawn {role, name, task}; stop/resume {target}; tail {target, lines}; found {template?}; admit {target, role};
// templates {}; close {}.
type AgentArgs struct {
	Action string `json:"action"`
	Role   string `json:"role,omitempty"`
	Name   string `json:"name,omitempty"`
	Task   string `json:"task,omitempty"` // spawn, resume: becomes a message from the caller to the worker
	// Cwd is spawn's directory for the worker, relative to the spawner's cwd or absolute; ""
	// inherits the spawner's. Another than the spawner's needs can_set_cwd and the bounds. With Template it
	// is the taskforce's directory: the bounds only (a solo or a gate has no can_set_cwd).
	Cwd string `json:"cwd,omitempty"`
	// No model: agents do not choose a worker's model; a "model" in the args is ignored like any
	// unknown field.
	Target string `json:"target,omitempty"` // worker name or id in view
	Lines  int    `json:"lines,omitempty"`
	// Template is found's team template (default p2p), or spawn's: the taskforce template to call up (instead of Role).
	Template string `json:"template,omitempty"`
	// Team is reopen's closed team (its name), or close's: a taskforce this participant called up.
	Team string `json:"team,omitempty"`
}

type AgentResult struct {
	ParticipantID string            `json:"participant_id,omitempty"` // spawn, resume, found, admit
	Token         string            `json:"token,omitempty"`          // found: new token ("" = keep yours)
	TeamID        string            `json:"team_id,omitempty"`        // found: the new team
	RunID         string            `json:"run_id,omitempty"`         // spawn, resume, found
	TaskID        string            `json:"task_id,omitempty"`        // spawn: the task message
	TaskSeq       int64             `json:"task_seq,omitempty"`       // spawn, resume with a task: its #N, what the model sees
	Exit          *Exit             `json:"exit,omitempty"`           // stop
	Records       []json.RawMessage `json:"records,omitempty"`        // tail
	Text          string            `json:"text,omitempty"`           // templates: the list for the model, printed as is
	Templates     []TemplateInfo    `json:"templates,omitempty"`      // templates: the same, structured
	TeamName      string            `json:"team_name,omitempty"`      // found: the new team; close: the team closed
	Stopped       []string          `json:"stopped,omitempty"`        // close: workers stopped
	Failed        []string          `json:"failed,omitempty"`         // close: workers whose stop failed
	Warnings      []string          `json:"warnings,omitempty"`       // found: as Team.Warnings
}
