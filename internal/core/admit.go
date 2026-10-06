package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// WithRoleChanged registers f, called after commit when a live session's role changes under it
// (admit), so the server can tell its harness to identify again. f must not block.
func WithRoleChanged(f func(participantID string)) Option {
	return func(e *Engine) { e.roleChanged = f }
}

// admit is `agent action=admit target=<solo> role=<r>`: a member whose role can spawn r takes a
// solo standing at the team root into the team as r. The solo keeps its row, run and token; its
// reports_to and spawned_by become the admitter. One admitted event. A headless worker never
// admits.
func (e *Engine) admit(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	if a.Target == "" || a.Role == "" {
		return AgentResult{}, errf(CodeInvalid, "admit needs target and role")
	}
	var res AgentResult
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "agent.admit", "agent")
		if err != nil {
			return err
		}
		var mode string
		if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(mode,'') FROM participants WHERE id=?`, p.id).Scan(&mode); err != nil {
			return internal(err)
		}
		if mode == modeHeadless { // as found: only sessions take others in
			return deny(&p, "agent.admit", "permission", "admit.headless", "a headless worker does not admit", nil,
				map[string]any{"target": a.Target})
		}
		m, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		if _, ok := m.Roles[a.Role]; !ok || p.team == "" {
			return errf(CodeInvalid, "role %q not in team manifest", a.Role)
		}
		if !contains(m.Roles[p.role].CanSpawn, a.Role) {
			return deny(&p, "agent.admit", "permission", "can_spawn", "role "+p.role+" cannot admit into "+a.Role, nil,
				map[string]any{"role": a.Role})
		}
		var cwd string
		s, err := scanParticipant(t.QueryRowContext(t.ctx, `SELECT `+participantCols+`, cwd FROM participants
			WHERE team_id IS NULL AND state<>'gone' AND (id=? OR name=?)`, a.Target, a.Target), &cwd)
		if errors.Is(err, sql.ErrNoRows) {
			return deny(&p, "agent.admit", "target", "admit.not_solo",
				fmt.Sprintf("%s is not a live solo session", a.Target), nil, map[string]any{"target": a.Target})
		}
		if err != nil {
			return internal(err)
		}
		var root string
		if err := t.QueryRowContext(t.ctx, `SELECT root_cwd FROM teams WHERE id=?`, p.team).Scan(&root); err != nil {
			return internal(err)
		}
		if cwd != root {
			return deny(&p, "agent.admit", "target", "admit.not_at_root",
				fmt.Sprintf("%s is in %s, not at the team root %s", s.name, cwd, root), nil,
				map[string]any{"target": a.Target, "cwd": cwd})
		}
		name, err := t.freeName(p.team, s.name)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE participants SET team_id=?, role=?, name=?, reports_to=?,
			spawned_by=?, joined_at=? WHERE id=?`, p.team, a.Role, name, p.id, p.id, t.now, s.id); err != nil {
			return internal(err)
		}
		res = AgentResult{ParticipantID: s.id, RunID: s.run, TeamID: p.team}
		if err := t.event(evt{typ: "admitted", participant: s.id, team: p.team, run: s.run, ref: s.id,
			payload: map[string]any{"by": p.id, "role": a.Role, "name": name}}); err != nil {
			return err
		}
		// A role with send may give a team without a gate its gate back (leave.go).
		s.team = p.team
		_, err = t.backAtGate(s)
		return err
	})
	if err != nil {
		return AgentResult{}, err
	}
	if e.roleChanged != nil {
		e.roleChanged(res.ParticipantID)
	}
	return res, nil
}
