// Package proto is the socket wire format shared by server and CLI: JSON lines over a unix socket,
// one Request -> one Response, matched by ID. Args/Result are the core.*Args / core result types
// marshaled as-is.
package proto

import (
	"encoding/json"
	"fmt"
	"strings"

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
	VerbHistory      = "history"       // args core.HistoryArgs -> string (read-only): the inbox tool's view of the caller's own mail
	VerbCompletion   = "completion"    // args core.CompletionArgs -> core.CompletionResult
	VerbWho          = "who"           // no args -> []core.Presence
	VerbWatchAdd     = "watch.add"     // args core.TimerArgs -> core.Timer
	VerbWatchList    = "watch.list"    // no args -> []core.Timer
	VerbAgent        = "agent"         // args core.AgentArgs -> core.AgentResult
	VerbJoinAuto     = "join.auto"     // no auth (socket 0600 is the boundary), args core.JoinAutoArgs -> core.JoinResult
	VerbIdentify     = "identify"      // args core.IdentifyArgs -> core.IdentifyResult; binds the connection
	VerbPresence     = "presence"      // args core.PresenceArgs -> {}
	VerbHarnessEvent = "harness.event" // args core.HarnessEventArgs -> core.HarnessEventResult (adapter events)
	VerbWhy          = "why"           // admin, args core.WhyArgs -> core.WhyResult (read-only)
	VerbDoctor       = "doctor"        // admin, no args -> core.DoctorResult (read-only)
	VerbLabels       = "labels"        // admin, args core.LabelsArgs -> map id -> name or #seq (read-only)
	VerbTeamDown     = "team.down"     // admin, args core.TeamDownArgs -> core.TeamDownResult
	VerbPs           = "ps"            // admin, args core.StateArgs -> PsResult (read-only)
	VerbTail         = "tail"          // admin, args core.WorkerLogArgs -> TailResult (read-only): a worker's log or a session's transcript
	VerbShutdown     = "shutdown"      // admin, no args -> {}; the daemon shuts down after answering
	VerbAbort        = "abort"         // admin, args core.AdminTarget -> core.AbortResult
	VerbKill         = "kill"          // admin, args core.AdminTarget -> core.AgentResult (exit)
	VerbResume       = "resume"        // admin, args core.AdminTarget -> core.AgentResult (new run)
	VerbModel        = "model"         // admin, args core.ModelArgs -> core.ModelResult
	VerbModels       = "models"        // admin, args core.AdminTarget -> core.ModelsResult (read-only): what `model` accepts for the worker
	VerbGC           = "gc"            // admin, args core.GCArgs -> core.GCResult (archives under <dir>/archive)
)

// PsResult is the operator snapshot with the daemon's own pid and start time.
type PsResult struct {
	PID       int    `json:"pid"`
	StartedAt int64  `json:"started_at"`        // unix ms
	Version   string `json:"version,omitempty"` // the daemon's build version (report only; "" when the binary does not set it)
	// Outdated are the installs only `piggery setup --outdated` brings up (claude, codex),
	// read from files on each call; absent when there are none.
	Outdated []Outdated `json:"outdated,omitempty"`
	// Update is the newest release tag when it is newer than the daemon's build (the daily check,
	// cache/update.json); empty when none, when update.check is off, or on a dev build.
	Update string `json:"update,omitempty"`
	// Notices are the latest few messages to notify, newest first (what the notify hooks received).
	Notices []core.NotifyMail `json:"notices,omitempty"`
	core.State
}

// Outdated is one installed integration older than this binary's (or, for Codex, missing a hook).
type Outdated struct {
	Name  string `json:"name"`
	Have  int    `json:"have"`            // the integer installed (0: from before the integers)
	Want  int    `json:"want"`            // the integer this binary writes
	Drift string `json:"drift,omitempty"` // same integer, other entries: what differs
}

// UpdateNotice is `v0.7.0 available: piggery update`, or "" when latest is "" (no newer release).
// It is what top, ps --view and setup say of the daily update check.
func UpdateNotice(latest string) string {
	if latest == "" {
		return ""
	}
	return latest + " available: piggery update"
}

// Detail is `v1 < v2`, or `v2: <drift>` when the integer is the binary's but the entries differ.
func (o Outdated) Detail() string {
	if o.Have < o.Want {
		return fmt.Sprintf("v%d < v%d", o.Have, o.Want)
	}
	return fmt.Sprintf("v%d: %s", o.Have, o.Drift)
}

func (o Outdated) String() string { return o.Name + " (" + o.Detail() + ")" }

// Notice is `outdated: claude (v1 < v2), codex (v0 < v1): piggery setup --outdated`, or "" for none.
// It is the header line of ps and top and the status line of a UI that shows it.
func Notice(list []Outdated) string {
	if len(list) == 0 {
		return ""
	}
	what := make([]string, len(list))
	for i, o := range list {
		what[i] = o.String()
	}
	return "outdated: " + strings.Join(what, ", ") + ": piggery setup --outdated"
}

// TailResult is a worker's latest run and the path of its driver log (local to the daemon), or a
// session's transcript (Path "").
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
