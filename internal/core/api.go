// Package core is the piggery engine: registry, mailbox, gate, board, events, timers.
// It knows nothing about sockets, CLI, or the concrete SQL driver (database/sql only).
//
// Invariants (guardrails):
//   - Every state change and its event are written in the same transaction.
//     A refused request still commits its `denied` event.
//   - Core never branches on message body or kind.
//   - A message is acked only by the end of its batch in the caller's current run: an ok
//     turn_end of the batch's turn (harness.event, every adapter) or Completion(run, batch)
//     (the CLI participant).
//     Never because the participant looks idle, batch_seq grew, or it was redelivered.
package core

import (
	"database/sql"
	"encoding/json"
	"sync"
	"time"
)

// Reserved addresses core interprets.
const (
	AddrNotify = "notify"
	AddrBoard  = "board"
	AddrEngine = "engine"
)

// OpAssign marks a mail as the recipient's current assignment (ps shows it).
const OpAssign = "assign"

// BoardLimit is the max number of live pins per team.
const BoardLimit = 20

// Engine is the piggery core. Safe for use by one daemon; callers serialize through the DB.
type Engine struct {
	db             *sql.DB
	now            func() time.Time
	notify         func(participantID string)
	runtimes       []RuntimeDriver                    // one per harness (WithRuntime)
	defaultHarness string                             // workers of no role or session harness (WithDefaultHarness)
	allowedRoots   []string                           // spawn cwd bounds besides the team root (WithAllowedRoots)
	sharedPrompts  func(template, role string) string // the Human's text for a role card (WithSharedPrompts)
	minProtocol    int                                // lowest adapter protocol version identify accepts
	deliverLocks   sync.Map                           // participant id -> *sync.Mutex: one delivery at a time, in batch order
	// notifySink is told about each new, not held message to notify (WithNotifySink).
	notifySink func(messageID string)
	// roleChanged is told when a live session's role changes under it (WithRoleChanged).
	roleChanged func(participantID string)
	// abortPush pushes abort to a live session (WithAbortPush).
	abortPush func(participantID string) int
	// soloLimits are the limits of a solo (WithSoloTemplate).
	soloLimits map[string]int
	// templates resolves a team.found template name (WithTemplates).
	templates func(name, cwd string) (string, error)
	// templateList lists the templates found for a cwd (WithTemplateList).
	templateList func(cwd string) ([]TemplateRef, error)
}

// Option configures an Engine.
type Option func(*Engine)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(e *Engine) { e.now = now } }

// WithNotify registers f, called after commit once for each new message addressed to a
// participant (Send, FireDue); never for board/notify or refused sends. f must not block.
func WithNotify(f func(participantID string)) Option { return func(e *Engine) { e.notify = f } }

// ProtocolVersion is the version of the socket lines adapters rely on, raised when their shape
// changes; identify exchanges it. An adapter below MinProtocolVersion is refused; one that differs
// is recorded (ps, doctor). An adapter from before the exchange sends none: 0.
const (
	ProtocolVersion    = 1
	MinProtocolVersion = 0
)

// WithMinProtocolVersion sets the lowest adapter protocol version identify accepts (default
// MinProtocolVersion).
func WithMinProtocolVersion(n int) Option { return func(e *Engine) { e.minProtocol = n } }

// New returns an Engine over a DB opened by store.Open/OpenMemory.
func New(db *sql.DB, opts ...Option) *Engine {
	e := &Engine{db: db, now: time.Now, minProtocol: MinProtocolVersion}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Caller is an authenticated participant for the current run.
type Caller struct {
	ParticipantID string `json:"participant_id"`
	RunID         string `json:"run_id"`
	TeamID        string `json:"team_id"`
	Role          string `json:"role"`
	Name          string `json:"name"`
	// ByHost: authenticated by its host process (AuthenticateHost), not a token.
	ByHost bool `json:"-"`
}

// ---- admin (the server checks the admin token before calling these) ----

type TeamUpArgs struct {
	Name     string `json:"name,omitempty"` // default: the manifest's template name
	Manifest string `json:"manifest"`       // YAML text, frozen into teams.manifest
	Cwd      string `json:"cwd"`            // normalized by core: realpath, no trailing '/'
}

type Team struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Template  string `json:"template"`
	RootCwd   string `json:"root_cwd"`
	CreatedAt int64  `json:"created_at"`
	// Warnings are things in the manifest that work but are probably not meant (see manifestWarnings).
	Warnings []string `json:"warnings,omitempty"`
}

type JoinArgs struct {
	Team    string `json:"team"` // team id or unique open-team name
	Role    string `json:"role"` // must exist in the team manifest
	Name    string `json:"name"` // unique within the team
	Cwd     string `json:"cwd"`
	Kind    string `json:"kind,omitempty"`    // default "agent"
	Harness string `json:"harness,omitempty"` // default "cli"
	Mode    string `json:"mode,omitempty"`    // default "pull"
}

type JoinResult struct {
	ID     string `json:"id"`    // PIGGERY_ID
	Token  string `json:"token"` // PIGGERY_TOKEN; only its hash is stored
	RunID  string `json:"run_id"`
	TeamID string `json:"team_id"`
}

type LogArgs struct {
	After int64  `json:"after,omitempty"` // events with seq > After
	Team  string `json:"team,omitempty"`
	Limit int    `json:"limit,omitempty"` // default 200
}

type Event struct {
	Seq         int64           `json:"seq"`
	Ts          int64           `json:"ts"`
	Type        string          `json:"type"`
	Participant string          `json:"participant,omitempty"`
	TeamID      string          `json:"team_id,omitempty"`
	RunID       string          `json:"run_id,omitempty"`
	RefID       string          `json:"ref_id,omitempty"`
	Payload     json.RawMessage `json:"payload"` // never contains message body; denied carries rule_id, layer, reason
}

// ---- participant verbs ----

type SendArgs struct {
	To          string `json:"to"` // participant name/id in view, "notify", or "board"
	Kind        string `json:"kind,omitempty"`
	Body        string `json:"body"`
	ReplyTo     string `json:"reply_to,omitempty"`
	Op          string `json:"op,omitempty"`     // "" | assign (to a member that reports to the sender) | replace | remove (board)
	Target      string `json:"target,omitempty"` // board only: live pin id for replace/remove
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

type SendResult struct {
	ID        string `json:"id"`
	Seq       int64  `json:"seq"`                 // what models and humans see (#N)
	Duplicate bool   `json:"duplicate,omitempty"` // same (from, client_msg_id) seen: old id returned, nothing written
	Held      bool   `json:"held,omitempty"`      // stored but held by a mail limit; not delivered until released
	RuleID    string `json:"rule_id,omitempty"`   // the limit that held it
}

// Message is a stored message as seen by a reader. FromLabel is the engine-stamped header,
// e.g. `alice (peer)` or `boss (supervisor, you report to them)`.
type Message struct {
	ID         string `json:"id"`
	Seq        int64  `json:"seq"`
	From       string `json:"from"`
	FromName   string `json:"from_name"` // the sender's name (an address; ids are not shown)
	FromLabel  string `json:"from_label"`
	To         string `json:"to"`
	Kind       string `json:"kind,omitempty"`
	ReplyTo    string `json:"reply_to,omitempty"`
	ReplyToSeq int64  `json:"reply_to_seq,omitempty"`
	Op         string `json:"op,omitempty"`
	Target     string `json:"target,omitempty"`
	Body       string `json:"body"`
	CreatedAt  int64  `json:"created_at"`
	CcOf       string `json:"cc_of,omitempty"` // a routing cc copy of this message id
	CcOfSeq    int64  `json:"cc_of_seq,omitempty"`
	CcTo       string `json:"cc_to,omitempty"` // label of the original recipient, from the reader's view
}

type InboxArgs struct {
	// Batch nil = pull: deliveries recorded with batch_seq NULL, never ackable.
	// Batch N: records deliveries (run, N); N closed -> CodeBatchClosed;
	// opening a new N lower than the run's highest batch -> CodeBatchOrder.
	Batch *int64 `json:"batch,omitempty"`
	// View is a read-only read (views.go): "board". A view records no delivery, takes no batch,
	// and never acks. Limit caps it (default 50).
	View  string `json:"view,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type Delivered struct {
	DeliveryID int64 `json:"delivery_id"`
	// Redelivered: an earlier delivery of this message exists (it was given before and not acked, so
	// it is given again). Display only: the rendering marks it, delivery and ack do not look at it.
	Redelivered bool `json:"redelivered,omitempty"`
	Message
}

type CompletionArgs struct {
	Batch int64 `json:"batch"`
}

type CompletionResult struct {
	Acked []string `json:"acked"` // message ids acked by this completion
}

// Presence is one line of who, by Kind:
//   - member: a participant of the caller's team (id, name, role, state…, team, gate)
//   - team: another open team (id and name of the team, cwd = its root, gate_name)
//   - solo: a live solo session (id, name, cwd, state…, gate true, admittable when its cwd is
//     the caller's team root); a solo caller's own line comes first.
type Presence struct {
	Kind         string `json:"kind"` // member, team, solo
	ID           string `json:"id"`
	Name         string `json:"name"`
	Role         string `json:"role,omitempty"`
	State        string `json:"state,omitempty"`
	StateSince   int64  `json:"state_since,omitempty"`
	LastActivity int64  `json:"last_activity,omitempty"`
	Team         string `json:"team,omitempty"` // member: the team's name
	Cwd          string `json:"cwd,omitempty"`  // team: its root; solo: its cwd
	Gate         bool   `json:"gate"`           // member: the team's gate; solo: always
	GateName     string `json:"gate_name,omitempty"`
	Admittable   bool   `json:"admittable,omitempty"`
}

// Who line kinds.
const (
	WhoMember = "member"
	WhoTeam   = "team"
	WhoSolo   = "solo"
)

type TimerArgs struct {
	To      string `json:"to"`    // participant name/id in view
	InMs    int64  `json:"in_ms"` // first fire = now + InMs
	EveryMs int64  `json:"every_ms,omitempty"`
	Body    string `json:"body"`
}

type Timer struct {
	ID      string `json:"id"`
	Owner   string `json:"owner"`
	Target  string `json:"target"`
	FireAt  int64  `json:"fire_at"`
	EveryMs int64  `json:"every_ms,omitempty"`
	Body    string `json:"body"`
	Active  bool   `json:"active"`
}

// ---- harness driver verbs (step 2) ----

// IdentifyArgs binds a harness process to the caller. NewRun=true on the first identify of
// a process (new run_id, state idle; the old run's open deliveries never ack; RunID ignored).
// False on reconnect: RunID is required and must be the current run (else run.stale), so a
// superseded process cannot take over the new run; run kept, gone -> idle.
type IdentifyArgs struct {
	// ProtocolVersion is the adapter's (ProtocolVersion when it was built); absent = 0.
	ProtocolVersion int    `json:"protocol_version"`
	NewRun          bool   `json:"new_run"`
	RunID           string `json:"run_id,omitempty"`
	Harness         string `json:"harness,omitempty"`
	Mode            string `json:"mode,omitempty"`
	HarnessRef      string `json:"harness_ref,omitempty"`
	// ToolPrefix is how this driver names piggery's tools for the model (pi: "piggery_"); core
	// writes every tool name it puts in model-facing text with it. "" = the short names.
	ToolPrefix string `json:"tool_prefix,omitempty"`
	// Model is the session's current model as the harness names it ("" unknown); a worker it spawns
	// may inherit it. Presence "model" reports a change.
	Model string `json:"model,omitempty"`
	// Thinking is the session's current thinking level as the harness names it ("" unknown);
	// inherited like Model. Never checked by core.
	Thinking string `json:"thinking,omitempty"`
	// Capabilities is what the session's harness supports (Cap*), declared per run; for a
	// headless worker its runtime driver declares instead and this is ignored. Absent (nil) =
	// not declared: nothing is refused. A list, even empty, refuses what it leaves out.
	Capabilities []string `json:"capabilities"`
	// Again: this connection already identified as this run (the adapter reads its new role
	// after found, admit, reopen), not a reconnect: its open turn goes on. Set by the server.
	Again bool `json:"-"`
	// Ended is the turn ends the adapter could not send (the daemon was gone), oldest first;
	// applied before the run's open turns are closed.
	Ended []EndedTurn `json:"ended,omitempty"`
}

// EndedTurn is one queued turn_end: the turn's key and its outcome (HarnessOutcome*).
type EndedTurn struct {
	Key     string `json:"key"`
	Outcome string `json:"outcome"`
}

type IdentifyResult struct {
	ParticipantID string   `json:"participant_id"`
	RunID         string   `json:"run_id"`
	Name          string   `json:"name"`
	TeamID        string   `json:"team_id"`
	Role          string   `json:"role"`
	Tools         []string `json:"tools"`
	RoleCard      string   `json:"role_card"` // rendered by core; never contains tokens
	// ProtocolVersion is the daemon's; an adapter that differs says so in its session. A daemon
	// from before the exchange sends none: the adapter reads 0.
	ProtocolVersion int `json:"protocol_version"`
}

// Presence events from the harness driver.
const (
	PresenceAgentStart    = "agent_start"     // -> working
	PresenceTurnEnd       = "turn_end"        // sets last_turn_end
	PresenceUIPromptStart = "ui_prompt_start" // -> awaiting_permission
	PresenceUIPromptEnd   = "ui_prompt_end"   // -> working
	PresenceAgentSettled  = "agent_settled"   // -> idle; sent on every settle, after the turn's end if any
	PresenceShutdown      = "shutdown"        // -> gone
	PresenceModel         = "model"           // the session's model or thinking level changed (PresenceArgs); no state change
)

type PresenceArgs struct {
	Event    string `json:"event"`
	Model    string `json:"model,omitempty"`    // event model: the current model ("" unknown)
	Thinking string `json:"thinking,omitempty"` // event model: the current thinking level ("" unknown)
}

// Capabilities: what a participant's harness supports. The CLI, tools and top read them, never the
// harness name; a command the harness cannot do is refused with CodeUnsupported.
const (
	CapAbort        = "abort"         // cancel the current turn
	CapSetModel     = "set_model"     // change model or thinking level while it runs
	CapListModels   = "list_models"   // list the models SetModel accepts, while it runs
	CapWake         = "wake"          // an idle participant can be woken by mail
	CapSteer        = "steer"         // mail can join a running turn
	CapSystemPrompt = "system_prompt" // the role card goes in its system prompt
	CapUsage        = "usage"         // its driver log has usage and turns (top's ctx/turns, tail)
	// CapDeliver: the runtime driver delivers mail itself (RuntimeDriver.Deliver) and reports
	// each batch completed or cancelled (Engine.DeliveryEnded); no adapter gives the worker mail.
	CapDeliver = "deliver"
)

// ---- adapter events ----

// Standard events an adapter reports for a harness that has no batch logic of its own (Claude Code
// hooks). The daemon counts batches and applies the ack rule for them in one place: turn_start
// opens batch n+1 of the run, keyed by PromptID; turn_end ok on the same key acks it; failed or
// interrupted closes nothing and acks nothing.
const (
	HarnessSessionStart  = "session_start"   // HarnessRef; -> idle
	HarnessTurnStart     = "turn_start"      // PromptID; opens a batch; -> working; returns mail
	HarnessToolBoundary  = "tool_boundary"   // returns mail to add to the running turn
	HarnessTurnEnd       = "turn_end"        // Outcome, PromptID, StopHookActive
	HarnessPermission    = "permission_wait" // -> awaiting_permission: mail and wakes held until it ends
	HarnessIdle          = "idle"            // the harness sits idle (Claude idle_prompt): an open turn (Esc) closes unacked; -> idle
	HarnessModelChanged  = "model_changed"   // Model, Thinking
	HarnessSessionEnd    = "session_end"     // -> gone, unless HarnessRef is not the participant's current session id
	HarnessOutcomeOK     = "ok"
	HarnessOutcomeFailed = "failed"
	HarnessOutcomeIntr   = "interrupted"
)

type HarnessEventArgs struct {
	Event          string `json:"event"`
	Outcome        string `json:"outcome,omitempty"`     // turn_end
	HarnessRef     string `json:"harness_ref,omitempty"` // session_start, session_end: the session id
	Source         string `json:"source,omitempty"`      // session_start: startup|resume|clear|compact
	Model          string `json:"model,omitempty"`       // model_changed
	Thinking       string `json:"thinking,omitempty"`    // model_changed
	PromptID       string `json:"prompt_id,omitempty"`   // turn_start, tool_boundary, turn_end: the harness's key of the turn
	Wake           bool   `json:"wake,omitempty"`        // turn_start: a mail check (a wake, a reconnect), not a turn: with nothing to give, nothing is opened
	StopHookActive bool   `json:"stop_hook_active,omitempty"`
}

// HarnessEventResult is what the adapter prints back to the harness: Text is mail framed for
// the model ("" none; bounded in size, the rest comes next time); Block (turn_end only) means
// the turn must go on to read Text instead of ending.
type HarnessEventResult struct {
	Text  string `json:"text,omitempty"`
	Block bool   `json:"block,omitempty"`
}

// ---- error contract (serialized as-is on the wire) ----

const (
	CodeUnauthorized = "unauthorized"
	CodeDenied       = "denied" // gate refusal; RuleID + Layer set, event written
	CodeNotFound     = "not_found"
	CodeInvalid      = "invalid"
	CodeBatchClosed  = "batch_closed"
	CodeBatchOrder   = "batch_order"
	CodeUnsupported  = "unsupported"  // e.g. `agent` without a runtime driver
	CodeStartFailed  = "start_failed" // the runtime could not start a worker; Details = AgentResult
	CodeInternal     = "internal"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	RuleID  string `json:"rule_id,omitempty"`
	Layer   string `json:"layer,omitempty"`   // token | visibility | routing | permission | limit | target
	Details any    `json:"details,omitempty"` // e.g. live pins when the board is full
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ---- method set (bodies owned by the core implementer) ----

// TeamUp, Join, Log, Authenticate: team.go.

// Send, Inbox, Completion, Who: mail.go.

// Board: board.go. WatchAdd, WatchList, FireDue: timers.go.

// Agent (spawn, stop, resume, tail): agent.go.
