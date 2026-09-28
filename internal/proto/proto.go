// Package proto is the socket wire format shared by server and CLI: JSON lines over a unix socket,
// one Request -> one Response, matched by ID. Args/Result are the core.*Args / core result types
// marshaled as-is.
package proto

import (
	"encoding/json"

	"github.com/sting8k/piggery/internal/core"
)

// Verbs. Admin verbs require AdminToken; the rest require Auth (participant token).
// A request never falls back from a bad participant token to admin.
const (
	VerbTeamUp       = "team.up"       // admin, args core.TeamUpArgs -> core.Team
	VerbJoin         = "join"          // admin, args core.JoinArgs -> core.JoinResult
	VerbLog          = "log"           // admin, args core.LogArgs -> []core.Event
	VerbRelease      = "release"       // admin, args core.ReleaseArgs -> null; delivers a held message
	VerbSend         = "send"          // args core.SendArgs -> core.SendResult
	VerbInbox        = "inbox"         // args core.InboxArgs -> []core.Delivered
	VerbCompletion   = "completion"    // args core.CompletionArgs -> core.CompletionResult
	VerbWho          = "who"           // no args -> []core.Presence
	VerbBoard        = "board"         // no args -> []core.Message (live pins)
	VerbWatchAdd     = "watch.add"     // args core.TimerArgs -> core.Timer
	VerbWatchList    = "watch.list"    // no args -> []core.Timer
	VerbAgent        = "agent"         // args core.AgentArgs -> core.AgentResult
	VerbJoinAuto     = "join.auto"     // no auth (socket 0600 is the boundary), args core.JoinAutoArgs -> core.JoinResult
	VerbIdentify     = "identify"      // args core.IdentifyArgs -> core.IdentifyResult; binds the connection
	VerbPresence     = "presence"      // args core.PresenceArgs -> {}
	VerbHarnessEvent = "harness.event" // args core.HarnessEventArgs -> core.HarnessEventResult (adapter events)
	VerbTool         = "tool"          // args core.ToolArgs -> core.SendResult (a declarative role tool)
	VerbWhy          = "why"           // admin, args core.WhyArgs -> core.WhyResult (read-only)
	VerbDoctor       = "doctor"        // admin, no args -> core.DoctorResult (read-only)
	VerbLabels       = "labels"        // admin, args core.LabelsArgs -> map id -> name or #seq (read-only)
	VerbTeamDown     = "team.down"     // admin, args core.TeamDownArgs -> core.TeamDownResult
	VerbPs           = "ps"            // admin, args core.StateArgs -> PsResult (read-only)
	VerbTail         = "tail"          // admin, args core.WorkerLogArgs -> TailResult (read-only)
	VerbShutdown     = "shutdown"      // admin, no args -> {}; the daemon shuts down after answering
	VerbAbort        = "abort"         // admin, args core.AdminTarget -> core.AbortResult
	VerbKill         = "kill"          // admin, args core.AdminTarget -> core.AgentResult (exit)
	VerbResume       = "resume"        // admin, args core.AdminTarget -> core.AgentResult (new run)
	VerbModel        = "model"         // admin, args core.ModelArgs -> core.ModelResult
	VerbGC           = "gc"            // admin, args core.GCArgs -> core.GCResult (archives under <dir>/archive)
)

// PsResult is the operator snapshot with the daemon's own pid and start time.
type PsResult struct {
	PID       int   `json:"pid"`
	StartedAt int64 `json:"started_at"` // unix ms
	core.State
}

// TailResult is a worker's latest run and the path of its driver log (local to the daemon).
type TailResult struct {
	core.WorkerLog
	Path string `json:"path"`
}

// EventWake is pushed to every connection identified as a message's recipient. It carries
// no mail: the driver pulls with a batch, so the ack contract stays in inbox/completion.
const EventWake = "wake"

// EventRole is pushed to a session whose role changed under it (admit): the harness identifies
// again with the same run to get its tools, role card and tool specs.
const EventRole = "role"

// EventRetire is pushed to every identified connection of a team that was just closed (team
// down): the harness stops driving its participant now instead of at its next verb.
const EventRetire = "retire"

// EventAbort is pushed to a session an admin aborts (piggery -a abort): its harness cancels
// the current turn (as Esc); the aborted batch is not acked.
const EventAbort = "abort"

// Push is a server-initiated frame (no ID), interleaved with responses on an identified connection.
type Push struct {
	Event  string `json:"event"`
	Reason string `json:"reason,omitempty"` // retire: the rule, e.g. team.closed
	// Ref (wake): the session id the participant runs now, for an adapter that must name it to
	// wake its harness (Codex: codex queue --thread).
	Ref string `json:"ref,omitempty"`
}

// Auth is a participant's id and token, or (a session a person opened, e.g. Claude's hooks and
// MCP server) its host process alone (core.AuthenticateHost).
type Auth struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Host  string `json:"host,omitempty"`
}

type Request struct {
	ID         string          `json:"id"`
	Verb       string          `json:"verb"`
	Auth       *Auth           `json:"auth,omitempty"`
	AdminToken string          `json:"admin_token,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
}

type Response struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *core.Error     `json:"error,omitempty"`
}
