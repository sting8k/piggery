package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A closed gate (docs/guide.md "Closing your gate"): a team (teams.gate_closed) or a solo (participants.gate_closed)
// that other teams and solos neither see nor mail, and that sees and mails none of them. The caller
// of a taskforce and the taskforce reach each other whatever either gate says. Inside a team nothing
// changes.

// heldGateClosed is held_reason of mail from outside to a unit whose gate closed.
const heldGateClosed = "gate_closed"

// WithDefaultGate makes the gate of a new solo closed (config.yaml gate: closed).
func WithDefaultGate(closed bool) Option {
	return func(e *Engine) { e.soloGateClosed = closed }
}

// unitClosed is whether the gate of p's unit is closed: its team's, or its own when it is a solo.
func (t *txn) unitClosed(team, id string) (bool, error) {
	q, arg := `SELECT gate_closed FROM participants WHERE id=?`, id
	if team != "" {
		q, arg = `SELECT gate_closed FROM teams WHERE id=?`, team
	}
	var closed bool
	if err := t.QueryRowContext(t.ctx, q, arg).Scan(&closed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, internal(err)
	}
	return closed, nil
}

// inheritGate gives teamID the gate state of from's unit (found, a taskforce, reopen).
func (t *txn) inheritGate(teamID string, from participant) error {
	closed, err := t.unitClosed(from.team, from.id)
	if err != nil {
		return err
	}
	_, err = t.ExecContext(t.ctx, `UPDATE teams SET gate_closed=? WHERE id=?`, closed, teamID)
	return internal(err)
}

// calls is whether p and the other unit (a team, or a solo when otherTeam is "") are a caller and
// its taskforce, either way: the exception to a closed gate.
func (t *txn) calls(p participant, otherTeam, otherID string) (bool, error) {
	if otherTeam != "" {
		parent, err := t.taskforceParent(otherTeam)
		if err != nil || parent == p.id {
			return parent == p.id, err
		}
	}
	parent, err := t.taskforceParent(p.team)
	if err != nil || parent == "" {
		return false, err
	}
	if parent == otherID {
		return true, nil
	}
	if otherTeam == "" {
		return false, nil
	}
	var in bool
	err = t.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM participants WHERE id=? AND team_id=?)`, parent, otherTeam).Scan(&in)
	return in, internal(err)
}

// gateStands is how p's gate stands to another team (otherTeam) or solo (otherID): seen is whether
// the other is in p's view at all (a closed gate is not, unless the two are a caller and its
// taskforce); selfClosed is whether p's own closed gate keeps p from it.
func (t *txn) gateStands(p participant, otherTeam, otherID string) (seen, selfClosed bool, err error) {
	if related, err := t.calls(p, otherTeam, otherID); err != nil || related {
		return true, false, err
	}
	other, err := t.unitClosed(otherTeam, otherID)
	if err != nil || other {
		return false, false, err
	}
	self, err := t.unitClosed(p.team, p.id)
	return true, self, err
}

func gateClosedSelf(p participant, to string) error {
	return deny(&p, "send", "visibility", "gate.closed_self",
		fmt.Sprintf("your gate is closed: you do not reach %s; ask the Human to open it (agent action=gate_open)", to), nil,
		map[string]any{"to": to})
}

// gateAction is `agent action=gate_close|gate_open`: a solo, or its team's gate (a session, not a
// headless worker), changes its unit's gate. Same state again is a no-op. Closing holds the unit's
// unacked mail from outside; opening releases it and wakes the recipients.
func (e *Engine) gateAction(ctx context.Context, c Caller, closed bool) (AgentResult, error) {
	verb := "agent.gate_open"
	if closed {
		verb = "agent.gate_close"
	}
	var res AgentResult
	var wake []string
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, verb, "agent")
		if err != nil {
			return err
		}
		if p.team != "" {
			var mode string
			if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(mode,'') FROM participants WHERE id=?`, p.id).Scan(&mode); err != nil {
				return internal(err)
			}
			gate, ok, err := t.teamGate(p.team)
			if err != nil {
				return err
			}
			if mode == modeHeadless || !ok || gate.id != p.id {
				name := "(none)"
				if ok {
					name = gate.name
				}
				return deny(&p, verb, "permission", "gate.not_gate",
					fmt.Sprintf("only a solo or your team's gate %s changes the gate; ask %s", name, name),
					map[string]any{"gate": name, "gate_id": gate.id}, map[string]any{"gate": name})
			}
		}
		was, err := t.unitClosed(p.team, p.id)
		if err != nil {
			return err
		}
		word := map[bool]string{true: "closed", false: "open"}[closed]
		if was == closed {
			res.Text = "your gate is already " + word
			return nil
		}
		table, key := "participants", p.id
		if p.team != "" {
			table, key = "teams", p.team
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE `+table+` SET gate_closed=? WHERE id=?`, closed, key); err != nil {
			return internal(err)
		}
		n := 0
		typ, field := "gate_opened", "released"
		if closed {
			typ, field = "gate_closed", "held"
			n, err = t.holdOutsideMail(p.team, p.id)
		} else {
			wake, n, err = t.releaseOutsideMail(p.team, p.id)
		}
		if err != nil {
			return err
		}
		res.Text = fmt.Sprintf("your gate is %s (%d messages %s)", word, n, field)
		return t.event(evt{typ: typ, participant: p.id, team: p.team, run: p.run, ref: key, payload: map[string]any{field: n}})
	})
	if err != nil {
		return AgentResult{}, err
	}
	for _, id := range wake {
		e.notifyAfterCommit(id)
	}
	return res, nil
}

// toUnit matches the participant r (a messages.to_id) of the unit (team, or the solo id).
const toUnit = `(? <> '' AND r.team_id=?) OR (? = '' AND r.id=?)`

// holdOutsideMail holds the unit's unacked mail whose sender is another team or solo, not its
// caller or taskforce, and returns how many. Mail from the engine, the Human or a member is not.
func (t *txn) holdOutsideMail(team, id string) (int, error) {
	r, err := t.ExecContext(t.ctx, `UPDATE messages SET held_reason=? WHERE id IN (
		SELECT m.id FROM messages m JOIN participants r ON r.id=m.to_id JOIN participants s ON s.id=m.from_id
		WHERE m.acked_at IS NULL AND m.held_reason IS NULL AND (`+toUnit+`)
		AND COALESCE(s.team_id,s.id) <> COALESCE(r.team_id,r.id)
		AND COALESCE((SELECT parent_id FROM teams WHERE id=s.team_id),'') <> r.id
		AND COALESCE((SELECT parent_id FROM teams WHERE id=r.team_id),'') <> s.id)`,
		heldGateClosed, team, team, team, id)
	if err != nil {
		return 0, internal(err)
	}
	n, err := r.RowsAffected()
	return int(n), internal(err)
}

// releaseOutsideMail releases what holdOutsideMail held for the unit: the recipients to wake and how many messages.
func (t *txn) releaseOutsideMail(team, id string) ([]string, int, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT DISTINCT m.to_id FROM messages m JOIN participants r ON r.id=m.to_id
		WHERE m.held_reason=? AND m.acked_at IS NULL AND (`+toUnit+`)`, heldGateClosed, team, team, team, id)
	if err != nil {
		return nil, 0, internal(err)
	}
	var to []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, 0, internal(err)
		}
		to = append(to, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, internal(err)
	}
	r, err := t.ExecContext(t.ctx, `UPDATE messages SET held_reason=NULL WHERE id IN (
		SELECT m.id FROM messages m JOIN participants r ON r.id=m.to_id
		WHERE m.held_reason=? AND m.acked_at IS NULL AND (`+toUnit+`))`, heldGateClosed, team, team, team, id)
	if err != nil {
		return nil, 0, internal(err)
	}
	n, err := r.RowsAffected()
	return to, int(n), internal(err)
}
