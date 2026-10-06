package core

import (
	"database/sql"
	"errors"
	"fmt"
)

// Leaving a team: a member that founds a new team leaves its old one. Its workers
// (reports_to/spawned_by) and its unacked mail move to the team's gate (a gate that is only gone
// keeps them in its inbox; notify is told). When the gate itself leaves, the next member by join order
// is the gate (gate_moved). With no gate at all (no member left that can be one) they wait, mail held
// as heldNoGate, and notify is told; the first session with send to enter takes the gate and the
// held mail (backAtGate). Mail is never acked here: a moved message is new mail for the gate, acked
// only by its own completion.

// heldNoGate is held_reason of mail to a member that left while its team had no gate.
const heldNoGate = "team.no_gate"

// leave takes p (a member) out of its team: its row goes gone with left_at set and a fresh
// run (so no completion of the old run can ack mail that moves away), one left event, then
// the reroute with one rerouted event. It returns the gate to wake ("" = none) and a notify
// notice id ("" = none).
func (t *txn) leave(p participant, toTeam string) (wake, notice string, err error) {
	if _, err := t.ExecContext(t.ctx, `UPDATE participants SET left_at=?, run_id=? WHERE id=?`,
		t.now, newID(t.now), p.id); err != nil {
		return "", "", internal(err)
	}
	if err := t.setState(&p, "gone", "left"); err != nil {
		return "", "", err
	}
	if err := t.event(evt{typ: "left", participant: p.id, team: p.team, run: p.run, ref: p.id,
		payload: map[string]any{"to_team": toTeam}}); err != nil {
		return "", "", err
	}
	// The gate itself leaving is the one thing that moves it: to the next member by join order.
	if cur, has, err := t.teamGate(p.team); err != nil {
		return "", "", err
	} else if has && cur.id == p.id {
		next, _, err := t.nextGate(p.team) // none: the zero value, stored as no gate
		if err != nil {
			return "", "", err
		}
		if err := t.setGate(p.team, next.id); err != nil {
			return "", "", err
		}
		if err := t.event(evt{typ: "gate_moved", participant: p.id, team: p.team, ref: next.id,
			payload: map[string]any{"from": p.id, "to": next.id}}); err != nil {
			return "", "", err
		}
	}
	gate, msgs, workers, ok, err := t.rerouteToGate(p.team)
	if err != nil {
		return "", "", err
	}
	payload := map[string]any{"from": p.id, "messages": msgs, "workers": workers}
	if ok {
		payload["to"] = gate.id
		wake = gate.id
	}
	if ok && gate.state == "gone" { // it waits in the gate's inbox for the gate to come back
		var team string
		if err := t.QueryRowContext(t.ctx, `SELECT name FROM teams WHERE id=?`, p.team).Scan(&team); err != nil {
			return "", "", internal(err)
		}
		notice = newID(t.now)
		body := fmt.Sprintf("%s left team %s; its gate %s is not live: %d unacked messages and %d workers of %s wait for %s.",
			p.name, team, gate.name, msgs, workers, p.name, gate.name)
		if _, err := t.insertMessage(notice, "", p.team, AddrEngine, AddrNotify, noticeGateLost, "", "", "", body); err != nil {
			return "", "", err
		}
	}
	if !ok {
		r, err := t.ExecContext(t.ctx, `UPDATE messages SET held_reason=? WHERE to_id=? AND acked_at IS NULL
			AND held_reason IS NULL`, heldNoGate, p.id)
		if err != nil {
			return "", "", internal(err)
		}
		held, _ := r.RowsAffected()
		if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM participants WHERE team_id=? AND
			(reports_to=? OR spawned_by=?)`, p.team, p.id, p.id).Scan(&workers); err != nil {
			return "", "", internal(err)
		}
		payload["held"], payload["messages"], payload["workers"] = true, held, workers
		var team string
		if err := t.QueryRowContext(t.ctx, `SELECT name FROM teams WHERE id=?`, p.team).Scan(&team); err != nil {
			return "", "", internal(err)
		}
		notice = newID(t.now)
		body := fmt.Sprintf("%s left team %s, which has no live member now: %d unacked messages and %d workers of "+
			"%s wait for the team's next gate.", p.name, team, held, workers, p.name)
		if _, err := t.insertMessage(notice, "", p.team, AddrEngine, AddrNotify, noticeGateLost, "", "", "", body); err != nil {
			return "", "", err
		}
	}
	return wake, notice, t.event(evt{typ: "rerouted", participant: p.id, team: p.team, ref: p.id, payload: payload})
}

// rerouteToGate moves, to team's gate, the unacked mail of members that left and their workers'
// reports_to/spawned_by. ok is false when the team has no gate (nothing moves).
func (t *txn) rerouteToGate(team string) (gate participant, msgs, workers int, ok bool, err error) {
	gate, ok, err = t.teamGate(team)
	if err != nil || !ok {
		return gate, 0, 0, ok, err
	}
	const leavers = `(SELECT id FROM participants WHERE team_id=? AND left_at IS NOT NULL)`
	r, err := t.ExecContext(t.ctx, `UPDATE messages SET rerouted_from=COALESCE(rerouted_from, to_id), to_id=?,
		held_reason=CASE WHEN held_reason=? THEN NULL ELSE held_reason END
		WHERE acked_at IS NULL AND to_id IN `+leavers, gate.id, heldNoGate, team)
	if err != nil {
		return gate, 0, 0, ok, internal(err)
	}
	n, _ := r.RowsAffected()
	msgs = int(n)
	// Never the gate itself: it would report to itself.
	r, err = t.ExecContext(t.ctx, `UPDATE participants SET reports_to=? WHERE team_id=? AND id<>? AND reports_to IN `+leavers,
		gate.id, team, gate.id, team)
	if err != nil {
		return gate, 0, 0, ok, internal(err)
	}
	n, _ = r.RowsAffected()
	workers = int(n)
	if _, err := t.ExecContext(t.ctx, `UPDATE participants SET spawned_by=? WHERE team_id=? AND id<>? AND spawned_by IN `+leavers,
		gate.id, team, gate.id, team); err != nil {
		return gate, 0, 0, ok, internal(err)
	}
	// The gate itself reports to no one: its links to a leaver are cleared, never pointed at
	// itself (a worker that becomes the gate must not be told to send to itself).
	if _, err := t.ExecContext(t.ctx, `UPDATE participants SET
		reports_to=CASE WHEN reports_to IN `+leavers+` THEN NULL ELSE reports_to END,
		spawned_by=CASE WHEN spawned_by IN `+leavers+` THEN NULL ELSE spawned_by END WHERE id=?`,
		team, team, gate.id); err != nil {
		return gate, 0, 0, ok, internal(err)
	}
	return gate, msgs, workers, ok, nil
}

// backAtGate runs when p comes back to life (from gone) in a team: if the team was waiting
// for a gate, what its leavers left behind moves to the gate now, with one rerouted event.
// It returns the gate to wake ("" = nothing moved).
func (t *txn) backAtGate(p participant) (string, error) {
	if err := t.gateIfNone(p.team, p.id); err != nil { // a team with no gate: p may be it
		return "", err
	}
	var waiting int
	err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM messages WHERE held_reason=? AND to_id IN
		(SELECT id FROM participants WHERE team_id=? AND left_at IS NOT NULL)`, heldNoGate, p.team).Scan(&waiting)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", internal(err)
	}
	if waiting == 0 {
		if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM participants w WHERE w.team_id=? AND
			w.reports_to IN (SELECT id FROM participants WHERE team_id=? AND left_at IS NOT NULL)`,
			p.team, p.team).Scan(&waiting); err != nil {
			return "", internal(err)
		}
	}
	if waiting == 0 {
		return "", nil
	}
	gate, msgs, workers, ok, err := t.rerouteToGate(p.team)
	if err != nil || !ok {
		return "", err
	}
	return gate.id, t.event(evt{typ: "rerouted", participant: gate.id, team: p.team, ref: gate.id,
		payload: map[string]any{"to": gate.id, "messages": msgs, "workers": workers, "gate_back": true}})
}

// gateLeaver checks mail from p to q, a member of p's team. If q left the team, mail goes to
// the gate instead: denied naming it while there is one, else held until there is (noGate).
func (t *txn) gateLeaver(p, q participant, a SendArgs) (noGate bool, err error) {
	var left sql.NullInt64
	if err := t.QueryRowContext(t.ctx, `SELECT left_at FROM participants WHERE id=?`, q.id).Scan(&left); err != nil {
		return false, internal(err)
	}
	if !left.Valid {
		return false, nil
	}
	gate, ok, err := t.teamGate(q.team)
	if err != nil || !ok {
		return !ok, err
	}
	if gate.id == p.id {
		return false, deny(&p, "send", "visibility", "team.left",
			fmt.Sprintf("%s left the team and you are its gate now: there is no one above you to send this to", q.name),
			map[string]any{"gate": p.name, "gate_id": p.id}, map[string]any{"to": a.To})
	}
	return false, deny(&p, "send", "visibility", "team.left",
		fmt.Sprintf("%s left the team; send to its gate %s", q.name, gate.name),
		map[string]any{"gate": gate.name, "gate_id": gate.id}, map[string]any{"to": a.To, "gate": gate.name})
}

// holdNoGate holds a just-written message to a member that left a team with no gate; it moves
// to the gate when the team has one again (backAtGate).
func (t *txn) holdNoGate(p participant, msgID, to string) error {
	if _, err := t.ExecContext(t.ctx, `UPDATE messages SET held_reason=? WHERE id=?`, heldNoGate, msgID); err != nil {
		return internal(err)
	}
	return t.event(evt{typ: "held", participant: p.id, team: p.team, run: p.run, ref: msgID,
		payload: map[string]any{"rule_id": heldNoGate, "reason": heldNoGate, "to": to}})
}
