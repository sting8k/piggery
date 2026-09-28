package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// Operator snapshot: what `ps` and `top` show. Read-only: state tables only, no event, no state
// change.

type StateArgs struct {
	Events int `json:"events,omitempty"` // how many of the latest events to include (default 10)
}

type State struct {
	Teams []TeamState `json:"teams"`
	// Closed are the closed teams gc has not removed, newest first.
	Closed  []ClosedTeam `json:"closed"`
	Solos   []SoloState  `json:"solos"`
	Held    int          `json:"held"`    // messages held (not delivered until released)
	Unacked int          `json:"unacked"` // messages to participants not yet acked, held excluded
	Events  []Event      `json:"events"`  // the latest events, oldest first
	// Names labels every id in Events (actor or ref): a participant by its name, closed teams too;
	// a message by its #seq. A team ref has none.
	Names map[string]string `json:"names"`
	// ProtocolVersion is the daemon's; a session whose adapter's differs is out of step.
	ProtocolVersion int `json:"protocol_version"`
}

// TeamState is one open team. Held and Unacked count mail to its members (by recipient).
type TeamState struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Root      string        `json:"root"`
	Gate      string        `json:"gate"` // gate's name, "" when the team has none
	Held      int           `json:"held"`
	Unacked   int           `json:"unacked"`
	Members   []MemberState `json:"members"`
	CreatedAt int64         `json:"created_at"` // when it was brought up
}

// ClosedTeam is a closed team as it was left: its members with their final state.
type ClosedTeam struct {
	TeamState
	ClosedAt int64  `json:"closed_at"`
	ClosedBy string `json:"closed_by,omitempty"` // who ran team down; "" the admin
}

// MemberState is a participant of an open team (members that left it are not listed), in the
// order they entered it.
type MemberState struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	State       string `json:"state"`
	Headless    bool   `json:"headless"`
	Gate        bool   `json:"gate"`
	StateSince  int64  `json:"state_since"`
	LastTurnEnd int64  `json:"last_turn_end,omitempty"`
	Unacked     int    `json:"unacked"`
	// Model is what its session last reported, else the model stored for the worker; "" unknown.
	Model string `json:"model,omitempty"`
	// Thinking is its thinking level the same way (session's report, else stored); "" unknown.
	Thinking string `json:"thinking,omitempty"`
	RunID    string `json:"run_id"` // current run: a headless worker's driver log is per run
	// ReportsTo is who gets its mail and handbacks now (moves with a reroute); "" none.
	ReportsTo string `json:"reports_to,omitempty"`
	// Capabilities is what its harness supports (Cap*); top shows ctx, turns and tail by them.
	// nil = not declared.
	Capabilities []string `json:"capabilities"`
	Harness      string   `json:"harness,omitempty"` // pi, claude, codex; "" unknown
	CreatedAt    int64    `json:"created_at"`        // when it joined or was spawned
	LastActivity int64    `json:"last_activity"`
	Cwd          string   `json:"cwd"`
	SpawnedBy    string   `json:"spawned_by,omitempty"` // participant id; "" joined on its own
	// ProtocolVersion is what its adapter sent at its latest identify; nil = none yet.
	ProtocolVersion *int `json:"protocol_version,omitempty"`
}

// SoloState is a live solo session.
type SoloState struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Cwd          string `json:"cwd"`
	State        string `json:"state"`
	StateSince   int64  `json:"state_since"`
	Unacked      int    `json:"unacked"`
	CreatedAt    int64  `json:"created_at"` // when it joined
	Harness      string `json:"harness,omitempty"`
	Model        string `json:"model,omitempty"` // as for a member: its session's report, else stored
	LastActivity int64  `json:"last_activity"`
	// ProtocolVersion is what its adapter sent at its latest identify; nil = none yet.
	ProtocolVersion *int `json:"protocol_version,omitempty"`
}

// pendingMail counts, per recipient, mail not yet acked: [unacked (deliverable), held].
const pendingMail = `SELECT to_id, SUM(held_reason IS NULL), SUM(held_reason IS NOT NULL) FROM messages
	WHERE acked_at IS NULL AND to_id NOT IN ('board', 'notify', 'engine') GROUP BY to_id`

// State returns the operator snapshot.
func (e *Engine) State(ctx context.Context, a StateArgs) (State, error) {
	if a.Events <= 0 {
		a.Events = 10
	}
	out := State{Teams: []TeamState{}, Closed: []ClosedTeam{}, Solos: []SoloState{}, Events: []Event{}, Names: map[string]string{},
		ProtocolVersion: ProtocolVersion}
	err := e.readOnly(ctx, func(t *txn) error {
		pending := map[string][2]int{}
		rows, err := t.QueryContext(t.ctx, pendingMail)
		if err != nil {
			return internal(err)
		}
		for rows.Next() {
			var to string
			var n [2]int
			if err := rows.Scan(&to, &n[0], &n[1]); err != nil {
				rows.Close()
				return internal(err)
			}
			pending[to] = n
			out.Unacked += n[0]
			out.Held += n[1]
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internal(err)
		}

		teams, err := t.QueryContext(t.ctx, `SELECT id, name, root_cwd, created_at FROM teams WHERE closed_at IS NULL ORDER BY name`)
		if err != nil {
			return internal(err)
		}
		for teams.Next() {
			var ts TeamState
			if err := teams.Scan(&ts.ID, &ts.Name, &ts.Root, &ts.CreatedAt); err != nil {
				teams.Close()
				return internal(err)
			}
			out.Teams = append(out.Teams, ts)
		}
		teams.Close()
		if err := teams.Err(); err != nil {
			return internal(err)
		}
		for i := range out.Teams {
			if err := t.teamMembers(&out.Teams[i], pending); err != nil {
				return err
			}
		}
		// Closed teams gc has not removed yet, newest first, with who closed them.
		closed, err := t.QueryContext(t.ctx, `SELECT id, name, root_cwd, closed_at, created_at FROM teams
			WHERE closed_at IS NOT NULL ORDER BY closed_at DESC`)
		if err != nil {
			return internal(err)
		}
		for closed.Next() {
			var c ClosedTeam
			if err := closed.Scan(&c.ID, &c.Name, &c.Root, &c.ClosedAt, &c.CreatedAt); err != nil {
				closed.Close()
				return internal(err)
			}
			out.Closed = append(out.Closed, c)
		}
		closed.Close()
		if err := closed.Err(); err != nil {
			return internal(err)
		}
		for i := range out.Closed {
			c := &out.Closed[i]
			if err := t.teamMembers(&c.TeamState, pending); err != nil {
				return err
			}
			var by sql.NullString
			err := t.QueryRowContext(t.ctx, `SELECT p.name FROM events e JOIN participants p ON p.id=e.participant
				WHERE e.type='team_down' AND e.team_id=? ORDER BY e.seq DESC LIMIT 1`, c.ID).Scan(&by)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return internal(err)
			}
			c.ClosedBy = by.String
		}

		solos, err := t.QueryContext(t.ctx, `SELECT `+participantCols+`, cwd, created_at, COALESCE(session_model, model, ''), protocol_version FROM participants
			WHERE team_id IS NULL AND state<>'gone' ORDER BY created_at, rowid`)
		if err != nil {
			return internal(err)
		}
		for solos.Next() {
			var cwd, model string
			var created int64
			var proto sql.NullInt64
			q, err := scanParticipant(solos, &cwd, &created, &model, &proto)
			if err != nil {
				solos.Close()
				return internal(err)
			}
			out.Solos = append(out.Solos, SoloState{ID: q.id, Name: q.name, Cwd: cwd, State: q.state,
				StateSince: q.stateSince, Unacked: pending[q.id][0], CreatedAt: created, Harness: q.harness, Model: model,
				LastActivity: q.lastActivity, ProtocolVersion: intOrNil(proto)})
		}
		solos.Close()
		if err := solos.Err(); err != nil {
			return internal(err)
		}

		evs, err := t.QueryContext(t.ctx, `SELECT seq, ts, type, COALESCE(participant,''), COALESCE(team_id,''),
			COALESCE(run_id,''), COALESCE(ref_id,''), payload FROM events ORDER BY seq DESC LIMIT ?`, a.Events)
		if err != nil {
			return internal(err)
		}
		defer evs.Close()
		for evs.Next() {
			var ev Event
			var payload string
			if err := evs.Scan(&ev.Seq, &ev.Ts, &ev.Type, &ev.Participant, &ev.TeamID, &ev.RunID, &ev.RefID, &payload); err != nil {
				return internal(err)
			}
			ev.Payload = []byte(payload)
			out.Events = append(out.Events, ev)
		}
		if err := evs.Err(); err != nil {
			return internal(err)
		}
		evs.Close()
		slices.Reverse(out.Events)
		for _, ev := range out.Events {
			for _, id := range []string{ev.Participant, ev.RefID} {
				if _, done := out.Names[id]; done || id == "" {
					continue
				}
				var name string
				err := t.QueryRowContext(t.ctx, `SELECT name FROM participants WHERE id=?
					UNION ALL SELECT '#' || seq FROM messages WHERE id=?`, id, id).Scan(&name)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return internal(err)
				}
				if err == nil { // a ref may also be a team: no label
					out.Names[id] = name
				}
			}
		}
		return nil
	})
	return out, err
}

// teamMembers fills ts's gate and members (those that have not left, in the order they entered)
// and its mail counts: every row of the team counts (a leaver's held mail too).
func (t *txn) teamMembers(ts *TeamState, pending map[string][2]int) error {
	gate, _, err := t.teamGate(ts.ID)
	if err != nil {
		return err
	}
	ts.Gate = gate.name
	ts.Members = []MemberState{}
	rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+`, COALESCE(mode,''), last_turn_end, left_at,
		COALESCE(session_model, model, ''), COALESCE(session_thinking, thinking, ''), COALESCE(capabilities, 'null'),
		created_at, COALESCE(spawned_by, ''), cwd, protocol_version
		FROM participants WHERE team_id=? ORDER BY created_at, rowid`, ts.ID)
	if err != nil {
		return internal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var mode, model, thinking, caps, spawnedBy, cwd string
		var turn, left, proto sql.NullInt64
		var created int64
		q, err := scanParticipant(rows, &mode, &turn, &left, &model, &thinking, &caps, &created, &spawnedBy, &cwd, &proto)
		if err != nil {
			return internal(err)
		}
		n := pending[q.id]
		ts.Unacked += n[0]
		ts.Held += n[1]
		if left.Valid {
			continue
		}
		ts.Members = append(ts.Members, MemberState{ID: q.id, Name: q.name, Role: q.role, State: q.state,
			Headless: mode == modeHeadless, Gate: q.id == gate.id, StateSince: q.stateSince,
			LastTurnEnd: turn.Int64, Unacked: n[0], Model: model, Thinking: thinking, RunID: q.run, ReportsTo: q.reportsTo,
			CreatedAt: created, SpawnedBy: spawnedBy, Harness: q.harness, LastActivity: q.lastActivity, Cwd: cwd,
			ProtocolVersion: intOrNil(proto)})
		json.Unmarshal([]byte(caps), &ts.Members[len(ts.Members)-1].Capabilities)
	}
	if err := rows.Err(); err != nil {
		return internal(err)
	}
	return nil
}

func intOrNil(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

type WorkerLogArgs struct {
	Worker string `json:"worker"`         // name or participant id
	Team   string `json:"team,omitempty"` // when the name is in more than one team
}

// WorkerLog names the latest run the daemon started for a worker; its driver log is the tail.
type WorkerLog struct {
	ParticipantID string `json:"participant_id"`
	RunID         string `json:"run_id"`
}

// WorkerLog resolves a worker to its latest run. Read-only.
func (e *Engine) WorkerLog(ctx context.Context, a WorkerLogArgs) (WorkerLog, error) {
	var out WorkerLog
	err := e.readOnly(ctx, func(t *txn) error {
		p, err := t.participantForAdmin(a.Worker, a.Team)
		if err != nil {
			return err
		}
		out.ParticipantID = p.id
		err = t.QueryRowContext(t.ctx, `SELECT run_id FROM processes WHERE participant_id=?
			ORDER BY started_at DESC LIMIT 1`, p.id).Scan(&out.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			return errf(CodeNotFound, "%s is not a worker (no process started for it)", a.Worker)
		}
		return internal(err)
	})
	return out, err
}
