package core

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Teams talk to each other only gate to gate (a team's gate: the member teams.gate_id names, live or
// not; a solo is its own gate). Replies too: no reply_to exception.

type teamRef struct{ id, name string }

// relatedTeams returns the open teams other than teamID, ordered by name. (Every team on the
// machine: there is no parent/child rule any more.)
func (t *txn) relatedTeams(teamID string) ([]teamRef, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT id, name FROM teams WHERE closed_at IS NULL AND id<>? ORDER BY name`, teamID)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	var out []teamRef
	for rows.Next() {
		var r teamRef
		if err := rows.Scan(&r.id, &r.name); err != nil {
			return nil, internal(err)
		}
		out = append(out, r)
	}
	return out, internal(rows.Err())
}

// teamGate returns the gate of teamID: the member teams.gate_id names. It is stored, not computed: the
// founder (found), the reopener (reopen), or the next member by join order when the gate leaves
// (leave.go, event gate_moved). A member that is only gone stays the gate: its mail waits for it.
// ok is false when the team has none (no member left, or none with a role that has send). This is the
// one place that reads it.
func (t *txn) teamGate(teamID string) (gate participant, ok bool, err error) {
	var id sql.NullString
	if err := t.QueryRowContext(t.ctx, `SELECT gate_id FROM teams WHERE id=?`, teamID).Scan(&id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return gate, false, internal(err)
	}
	if !id.Valid {
		return gate, false, nil
	}
	return t.participantByID(id.String)
}

// joinOrder is the order members entered the team: joined_at, which a solo moving in (found, admit,
// reopen) sets and a member made inside the team leaves NULL (its created_at is then the moment).
const joinOrder = `COALESCE(joined_at, created_at), created_at, rowid`

// workerRow is a worker the daemon spawned: not a person's session (participants.person). A person's
// session that a mail woke runs headless but is still a session, so it never counts as a worker.
const workerRow = `(person=0)`

// setGate stores id (a member of teamID; "" none) as the team's gate.
func (t *txn) setGate(teamID, id string) error {
	_, err := t.ExecContext(t.ctx, `UPDATE teams SET gate_id=NULLIF(?,'') WHERE id=?`, id, teamID)
	return internal(err)
}

// nextGate is who becomes the gate of teamID when it has none or its gate left: the member that
// has not left, with a role that has the send tool, sessions (a person's, woken or not) before
// workers, then the one that joined earliest. ok is false when there is none.
func (t *txn) nextGate(teamID string) (gate participant, ok bool, err error) {
	m, err := t.teamManifest(teamID)
	if err != nil {
		return gate, false, err
	}
	rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+` FROM participants
		WHERE team_id=? AND left_at IS NULL ORDER BY `+workerRow+`, `+joinOrder, teamID)
	if err != nil {
		return gate, false, internal(err)
	}
	defer rows.Close()
	for rows.Next() {
		q, err := scanParticipant(rows)
		if err != nil {
			return gate, false, internal(err)
		}
		if contains(m.Roles[q.role].Tools, "send") {
			return q, true, nil
		}
	}
	return gate, false, internal(rows.Err())
}

// gateIfNone makes member id the gate of teamID when the team has none: the first session whose role
// has send to enter (a hidden `team up` team has no founder; a team whose gate left with no one
// to follow). A worker never takes a team's gate this way.
func (t *txn) gateIfNone(teamID, id string) error {
	var cur sql.NullString
	if err := t.QueryRowContext(t.ctx, `SELECT gate_id FROM teams WHERE id=?`, teamID).Scan(&cur); err != nil {
		return internal(err)
	}
	if cur.Valid {
		return nil
	}
	var role sql.NullString
	var worker bool
	if err := t.QueryRowContext(t.ctx, `SELECT role, `+workerRow+` FROM participants WHERE id=? AND team_id=? AND left_at IS NULL`,
		id, teamID).Scan(&role, &worker); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return internal(err)
	}
	m, err := t.teamManifest(teamID)
	if err != nil {
		return err
	}
	if worker || !contains(m.Roles[role.String].Tools, "send") {
		return nil
	}
	return t.setGate(teamID, id)
}

// resolveSendTarget resolves a send's to: a member of p's own team (id or name) first; then an open
// team's name, which is its gate; then a participant of another open team or a live solo, by id or
// name. other is the target's team (id "" = a solo) when it is not p's. Several matches at the last
// step -> visibility.ambiguous.
func (t *txn) resolveSendTarget(p participant, to string) (q participant, other *teamRef, err error) {
	if p.team != "" {
		q, err = scanParticipant(t.QueryRowContext(t.ctx,
			`SELECT `+participantCols+` FROM participants WHERE team_id=? AND (id=? OR name=?)`, p.team, to, to))
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return q, nil, internal(err)
		}
	}
	var team teamRef
	err = t.QueryRowContext(t.ctx, `SELECT id, name FROM teams WHERE closed_at IS NULL AND name=? AND id<>?`,
		to, p.team).Scan(&team.id, &team.name)
	switch {
	case err == nil:
		gate, ok, err := t.teamGate(team.id)
		if err != nil {
			return q, nil, err
		}
		if !ok {
			return q, nil, noGate(p, to, team.name)
		}
		return gate, &team, nil
	case !errors.Is(err, sql.ErrNoRows):
		return q, nil, internal(err)
	}
	rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+`, COALESCE((SELECT name FROM teams WHERE id=team_id),'')
		FROM participants WHERE (id=? OR name=?) AND id<>? AND (
		  (team_id IS NULL AND state<>'gone') OR
		  (team_id<>? AND team_id IN (SELECT id FROM teams WHERE closed_at IS NULL)))`, to, to, p.id, p.team)
	if err != nil {
		return q, nil, internal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var name string
		c, err := scanParticipant(rows, &name)
		if err != nil {
			return q, nil, internal(err)
		}
		q, other = c, &teamRef{id: c.team, name: name}
		ids = append(ids, c.id)
	}
	if err := rows.Err(); err != nil {
		return q, nil, internal(err)
	}
	switch len(ids) {
	case 0:
		return q, nil, deny(&p, "send", "visibility", "visibility.unknown_target",
			fmt.Sprintf("no participant or team %q in view", to), nil, map[string]any{"to": to})
	case 1:
		return q, other, nil
	}
	return q, nil, deny(&p, "send", "visibility", "visibility.ambiguous",
		fmt.Sprintf("%q names several participants; send by id: %s", to, strings.Join(ids, ", ")),
		map[string]any{"ids": ids}, map[string]any{"to": to, "ids": ids})
}

func noGate(p participant, to, team string) error {
	return deny(&p, "send", "visibility", "team_gate.none",
		fmt.Sprintf("team %s has no gate (no live member with send)", team),
		map[string]any{"team": team}, map[string]any{"to": to, "team": team})
}

// gateCrossTeam allows mail from p to q of another team only gate to gate: p is its own
// team's gate and q is the other team's (a solo, other.id "", is its own gate).
func (t *txn) gateCrossTeam(p, q participant, other teamRef, a SendArgs) error {
	own := p // a solo is its own gate
	if p.team != "" {
		g, _, err := t.teamGate(p.team)
		if err != nil {
			return err
		}
		own = g
	}
	if own.id != p.id {
		return deny(&p, "send", "visibility", "team_gate.sender_not_gate",
			fmt.Sprintf("only your team's gate %s sends to another team; ask %s to send it", own.name, own.name),
			map[string]any{"gate": own.name, "gate_id": own.id}, map[string]any{"to": a.To, "gate": own.name})
	}
	if other.id == "" {
		return nil
	}
	gate, ok, err := t.teamGate(other.id)
	if err != nil {
		return err
	}
	if !ok {
		return noGate(p, a.To, other.name)
	}
	if q.id != gate.id {
		return deny(&p, "send", "visibility", "team_gate.not_gate",
			fmt.Sprintf("%s is in team %s; mail to another team goes to its gate %s", q.name, other.name, gate.name),
			map[string]any{"team": other.name, "gate": gate.name, "gate_id": gate.id},
			map[string]any{"to": a.To, "team": other.name, "gate": gate.name})
	}
	return nil
}
