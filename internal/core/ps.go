package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	Template string `json:"template"` // the template it was founded from
	// Parent and ParentID: the participant (name, id) that called this team up as a taskforce
	// (spawn template=); "" for an ordinary team.
	Parent    string        `json:"parent,omitempty"`
	ParentID  string        `json:"parent_id,omitempty"`
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
	// Transcript is its session's own file as its adapter reported it (the CLI reads ctx, turns
	// and tail from it); nil for a headless worker (its driver log) or none reported.
	Transcript *Transcript `json:"transcript,omitempty"`
	// Assignment is the latest mail marked op assign to it (a spawn or resume task is one); nil = none.
	Assignment *Assignment `json:"assignment,omitempty"`
}

// Assignment is a member's current task: a stored mail, not a field anyone reports.
type Assignment struct {
	Seq   int64  `json:"seq"`
	Title string `json:"title"` // the first non-empty line of the body, plain, capped
	From  string `json:"from"`  // the sender's name; "admin" for the admin's resume
	At    int64  `json:"at"`
	// Latest is the newest mail in the reply chain rooted at the assignment (mails whose reply_to
	// leads back to it); nil = the assignment itself is the newest.
	Latest *ChainMail `json:"latest,omitempty"`
	// Newer is set when the chain ends with the member's own mail and the assigner has since sent
	// the member a mail outside the chain (a task whose op assign was forgotten, or a note): the
	// newest such mail.
	Newer *NewerMail `json:"newer,omitempty"`
}

type ChainMail struct {
	Seq      int64 `json:"seq"`
	At       int64 `json:"at"`
	ByMember bool  `json:"by_member"` // the member sent it (a handback or a question); else it is live again
}

type NewerMail struct {
	Seq   int64  `json:"seq"`
	Title string `json:"title"`
	At    int64  `json:"at"`
}

var mdPairs = regexp.MustCompile("\\*\\*([^*]+)\\*\\*|__([^_]+)__|`([^`]+)`")

// assignmentTitle is body's first non-empty line without Markdown markers, at most 100 runes.
func assignmentTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(mdPairs.ReplaceAllString(strings.TrimSpace(line), "$1$2$3"), "#>-*+ \t"))
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 100 {
			line = string(r[:100]) + "…"
		}
		return line
	}
	return ""
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
	// LastTurnEnd is when its latest turn ended (unix ms), as for a member; 0 = none yet.
	LastTurnEnd int64 `json:"last_turn_end,omitempty"`
	// ProtocolVersion is what its adapter sent at its latest identify; nil = none yet.
	ProtocolVersion *int `json:"protocol_version,omitempty"`
	// Transcript is as for a member.
	Transcript *Transcript `json:"transcript,omitempty"`
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

		teams, err := t.QueryContext(t.ctx, `SELECT id, name, template_name, root_cwd, created_at, COALESCE(parent_id,''),
			COALESCE((SELECT name FROM participants WHERE id=teams.parent_id),'') FROM teams WHERE closed_at IS NULL ORDER BY name`)
		if err != nil {
			return internal(err)
		}
		for teams.Next() {
			var ts TeamState
			if err := teams.Scan(&ts.ID, &ts.Name, &ts.Template, &ts.Root, &ts.CreatedAt, &ts.ParentID, &ts.Parent); err != nil {
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
		closed, err := t.QueryContext(t.ctx, `SELECT id, name, template_name, root_cwd, closed_at, created_at, COALESCE(parent_id,''),
			COALESCE((SELECT name FROM participants WHERE id=teams.parent_id),'') FROM teams
			WHERE closed_at IS NOT NULL ORDER BY closed_at DESC`)
		if err != nil {
			return internal(err)
		}
		for closed.Next() {
			var c ClosedTeam
			if err := closed.Scan(&c.ID, &c.Name, &c.Template, &c.Root, &c.ClosedAt, &c.CreatedAt, &c.ParentID, &c.Parent); err != nil {
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

		solos, err := t.QueryContext(t.ctx, `SELECT `+participantCols+`, cwd, created_at, COALESCE(session_model, model, ''), protocol_version,
			transcript, transcript_format, last_turn_end FROM participants
			WHERE team_id IS NULL AND state<>'gone' ORDER BY created_at, rowid`)
		if err != nil {
			return internal(err)
		}
		for solos.Next() {
			var cwd, model string
			var created int64
			var proto, turn sql.NullInt64
			var tpath, tformat sql.NullString
			q, err := scanParticipant(solos, &cwd, &created, &model, &proto, &tpath, &tformat, &turn)
			if err != nil {
				solos.Close()
				return internal(err)
			}
			out.Solos = append(out.Solos, SoloState{ID: q.id, Name: q.name, Cwd: cwd, State: q.state,
				StateSince: q.stateSince, Unacked: pending[q.id][0], CreatedAt: created, Harness: q.harness, Model: model,
				LastActivity: q.lastActivity, LastTurnEnd: turn.Int64, ProtocolVersion: intOrNil(proto), Transcript: transcriptOrNil(tpath, tformat)})
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
	assigned, err := t.assignments(ts.ID)
	if err != nil {
		return err
	}
	rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+`, COALESCE(mode,''), last_turn_end, left_at,
		COALESCE(session_model, model, ''), COALESCE(session_thinking, thinking, ''), COALESCE(capabilities, 'null'),
		created_at, COALESCE(spawned_by, ''), cwd, protocol_version, transcript, transcript_format
		FROM participants WHERE team_id=? ORDER BY `+joinOrder, ts.ID)
	if err != nil {
		return internal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var mode, model, thinking, caps, spawnedBy, cwd string
		var turn, left, proto sql.NullInt64
		var created int64
		var tpath, tformat sql.NullString
		q, err := scanParticipant(rows, &mode, &turn, &left, &model, &thinking, &caps, &created, &spawnedBy, &cwd, &proto,
			&tpath, &tformat)
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
			ProtocolVersion: intOrNil(proto), Transcript: transcriptOrNil(tpath, tformat), Assignment: assigned[q.id]})
		json.Unmarshal([]byte(caps), &ts.Members[len(ts.Members)-1].Capabilities)
	}
	if err := rows.Err(); err != nil {
		return internal(err)
	}
	return nil
}

// assignments returns, per member of the team, its latest delivered mail marked op assign, with
// the state of that assignment's reply chain (see Assignment).
func (t *txn) assignments(team string) (map[string]*Assignment, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT m.to_id, m.id, m.seq, m.body, m.from_id, COALESCE(f.name, 'admin'), m.created_at
		FROM messages m LEFT JOIN participants f ON f.id=m.from_id
		WHERE m.op=? AND m.held_reason IS NULL AND m.to_id IN (SELECT id FROM participants WHERE team_id=?)
		AND m.seq=(SELECT MAX(x.seq) FROM messages x WHERE x.op=m.op AND x.to_id=m.to_id AND x.held_reason IS NULL)`,
		OpAssign, team)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	out := map[string]*Assignment{}
	from := map[string]string{} // member -> assigner's id
	ids := map[string]string{}  // member -> assignment message id
	for rows.Next() {
		var to, id, body, fromID string
		var a Assignment
		if err := rows.Scan(&to, &id, &a.Seq, &body, &fromID, &a.From, &a.At); err != nil {
			return nil, internal(err)
		}
		a.Title = assignmentTitle(body)
		out[to], from[to], ids[to] = &a, fromID, id
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	rows.Close()
	for to, a := range out {
		var c ChainMail
		var sender string
		err := t.QueryRowContext(t.ctx, `WITH RECURSIVE chain(id) AS (
				SELECT ? UNION SELECT m.id FROM messages m JOIN chain ON m.reply_to=chain.id)
			SELECT seq, created_at, from_id FROM messages WHERE id IN chain AND id<>? ORDER BY seq DESC LIMIT 1`,
			ids[to], ids[to]).Scan(&c.Seq, &c.At, &sender)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, internal(err)
		}
		c.ByMember = sender == to
		a.Latest = &c
		if !c.ByMember {
			continue
		}
		var n NewerMail
		var body string
		err = t.QueryRowContext(t.ctx, `SELECT seq, body, created_at FROM messages
			WHERE from_id=? AND to_id=? AND seq>? AND cc_of IS NULL AND held_reason IS NULL ORDER BY seq DESC LIMIT 1`,
			from[to], to, c.Seq).Scan(&n.Seq, &body, &n.At)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, internal(err)
		}
		n.Title = assignmentTitle(body)
		a.Newer = &n
	}
	return out, nil
}

func transcriptOrNil(path, format sql.NullString) *Transcript {
	if !path.Valid {
		return nil
	}
	return &Transcript{Path: path.String, Format: format.String}
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
	RunID         string `json:"run_id"` // a worker's latest run ("" for a session)
	// Transcript is a session's own file (no process of piggery's), as its adapter reported it.
	Transcript *Transcript `json:"transcript,omitempty"`
}

// WorkerLog resolves a worker to its latest run, or a session to its transcript. Read-only.
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
		if !errors.Is(err, sql.ErrNoRows) {
			return internal(err)
		}
		var path, format sql.NullString
		if err := t.QueryRowContext(t.ctx, `SELECT transcript, transcript_format FROM participants WHERE id=?`,
			p.id).Scan(&path, &format); err != nil {
			return internal(err)
		}
		if out.Transcript = transcriptOrNil(path, format); out.Transcript == nil {
			return errf(CodeNotFound, "%s has no log: not a worker, and its harness reported no transcript", a.Worker)
		}
		return nil
	})
	return out, err
}
