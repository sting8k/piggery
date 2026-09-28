package core

import (
	"context"
	"fmt"
)

// closeTeam is `agent action=close`: the team's current gate, a session a human opened, closes its
// own team when the Human asks. The effect is admin team down, through TeamDown itself: headless
// workers stopped, every session of the team (the gate included) out of it (its harness joins again
// as a solo), nothing acked. Anyone else is denied with the gate's name.
func (e *Engine) closeTeam(ctx context.Context, c Caller) (AgentResult, error) {
	var p participant
	var team string
	err := e.inTx(ctx, func(t *txn) error {
		var err error
		if p, err = t.callerGranted(c, "agent.close", "agent"); err != nil {
			return err
		}
		if p.team == "" {
			return errf(CodeInvalid, "you are solo: there is no team to close")
		}
		var mode string
		if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(mode,'') FROM participants WHERE id=?`, p.id).Scan(&mode); err != nil {
			return internal(err)
		}
		gate, ok, err := t.teamGate(p.team)
		if err != nil {
			return err
		}
		if mode == modeHeadless { // even as the gate (no session left): only a session closes
			return deny(&p, "agent.close", "permission", "team_gate.close_headless",
				"a headless worker does not close its team", nil, nil)
		}
		if !ok || gate.id != p.id {
			name := "(none)"
			if ok {
				name = gate.name
			}
			return deny(&p, "agent.close", "permission", "team_gate.close_not_gate",
				fmt.Sprintf("only your team's gate %s closes the team; ask %s", name, name),
				map[string]any{"gate": name, "gate_id": gate.id}, map[string]any{"gate": name})
		}
		return internal(t.QueryRowContext(t.ctx, `SELECT name FROM teams WHERE id=?`, p.team).Scan(&team))
	})
	if err != nil {
		return AgentResult{}, err
	}
	r, err := e.TeamDown(ctx, TeamDownArgs{Team: p.team, closedBy: p.id})
	if err != nil {
		return AgentResult{}, err
	}
	return AgentResult{TeamID: r.TeamID, TeamName: team, Stopped: r.Stopped, Failed: r.Failed}, nil
}
