package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// reopen is `agent action=reopen team=<name>`: a solo session standing at the root of a closed team
// gc has not removed opens it again with its frozen manifest and becomes its gate in the session
// role. The old gate's own session (same harness_ref) gets its old participant back (new run and
// token); any other session moves in with its solo row and the old gate is marked left, so its
// workers and unacked mail move to the new gate through rerouteToGate, as when a member leaves.
// Workers stay stopped. One team_up event with reopened: true. Text is a summary for the new gate.
func (e *Engine) reopen(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	if a.Team == "" {
		return AgentResult{}, errf(CodeInvalid, "reopen needs team (the closed team's name)")
	}
	token, err := newToken()
	if err != nil {
		return AgentResult{}, internal(err)
	}
	var res AgentResult
	var wake string
	err = e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "agent.reopen", "agent")
		if err != nil {
			return err
		}
		var cwd, ref, mode string
		if err := t.QueryRowContext(t.ctx, `SELECT cwd, COALESCE(harness_ref,''), COALESCE(mode,'') FROM participants
			WHERE id=?`, p.id).Scan(&cwd, &ref, &mode); err != nil {
			return internal(err)
		}
		if p.team != "" || mode == modeHeadless {
			return deny(&p, "agent.reopen", "permission", "reopen.not_solo",
				"only a solo session reopens a team; you are in a team", nil, map[string]any{"team": a.Team})
		}
		var teamID, root string
		var closed sql.NullInt64
		err = t.QueryRowContext(t.ctx, `SELECT id, root_cwd, closed_at FROM teams WHERE name=?
			ORDER BY closed_at IS NOT NULL, root_cwd<>?, closed_at DESC LIMIT 1`, a.Team, cwd).Scan(&teamID, &root, &closed)
		if errors.Is(err, sql.ErrNoRows) {
			return errf(CodeNotFound, "no team %q (a closed team gc removed cannot be reopened)", a.Team)
		}
		if err != nil {
			return internal(err)
		}
		if !closed.Valid {
			return errf(CodeInvalid, "team %s is open: send to its gate or ask a member to admit you", a.Team)
		}
		if root != cwd {
			return deny(&p, "agent.reopen", "target", "reopen.not_at_root",
				fmt.Sprintf("team %s is rooted at %s; you are in %s", a.Team, root, cwd), nil,
				map[string]any{"team": a.Team, "cwd": cwd})
		}
		if parent, err := t.taskforceParent(teamID); err != nil {
			return err
		} else if parent != "" {
			return errf(CodeInvalid, "team %s was a taskforce: it is not reopened; call its template up again", a.Team)
		}
		m, err := t.teamManifest(teamID)
		if err != nil {
			return err
		}
		role := m.sessionRole()
		if role == "" {
			return errf(CodeInvalid, "team %s has several roles and no auto_join_role", a.Team)
		}
		old, hasOld, err := t.oldGate(teamID)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE teams SET closed_at=NULL WHERE id=?`, teamID); err != nil {
			return internal(err)
		}
		payload := map[string]any{"name": a.Team, "reopened": true, "by": p.id}
		gate := p
		if hasOld && ref != "" && old.ref == ref {
			// The old gate's own session: its old participant comes back; the solo row it used
			// meanwhile leaves (never resumed again: join.auto skips left rows) and its unacked
			// mail moves to the old participant.
			gate = old.participant
			res = AgentResult{ParticipantID: gate.id, RunID: newID(t.now), Token: token}
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET run_id=?, token_hash=?, last_turn_end=NULL,
				role=? WHERE id=?`, res.RunID, hashToken(token), role, gate.id); err != nil {
				return internal(err)
			}
			gate.run, gate.role = res.RunID, role
			if err := t.setState(&gate, "idle", "reopen"); err != nil {
				return err
			}
			if err := t.moveSession(p.id, gate.id); err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET left_at=?, run_id=? WHERE id=?`,
				t.now, newID(t.now), p.id); err != nil {
				return internal(err)
			}
			if err := t.setState(&p, "gone", "reopen"); err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, `UPDATE messages SET rerouted_from=COALESCE(rerouted_from, to_id), to_id=?
				WHERE acked_at IS NULL AND to_id=?`, gate.id, p.id); err != nil {
				return internal(err)
			}
		} else {
			// Another session: its solo row moves in (same id, run and token) and replaces the old
			// gate, which stays gone and is marked left for the reroute.
			if hasOld {
				if _, err := t.ExecContext(t.ctx, `UPDATE participants SET left_at=?, run_id=? WHERE id=?`,
					t.now, newID(t.now), old.id); err != nil {
					return internal(err)
				}
				payload["replaced"] = old.id
			}
			name, err := t.freeName(teamID, p.name)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET team_id=?, role=?, name=?, joined_at=? WHERE id=?`,
				teamID, role, name, t.now, p.id); err != nil {
				return internal(err)
			}
			gate.team, gate.role, gate.name = teamID, role, name
			res = AgentResult{ParticipantID: p.id, RunID: p.run}
		}
		res.TeamID, res.TeamName = teamID, a.Team
		// The reopener is the gate (the old gate's own session: it already is).
		if contains(m.Roles[role].Tools, "send") {
			if err := t.setGate(teamID, gate.id); err != nil {
				return err
			}
		}
		// A member row whose session went on as a newer row (a solo that joined again after the team
		// closed) is not a member of the reopened team: it leaves, its mail goes to the gate below.
		dropped, err := t.dropSuperseded(teamID)
		if err != nil {
			return err
		}
		if dropped > 0 {
			payload["superseded"] = dropped
		}
		now, msgs, workers, ok, err := t.rerouteToGate(teamID)
		if err != nil {
			return err
		}
		if !ok || now.id != gate.id {
			return internal(fmt.Errorf("reopen: gate is %q, want %s", now.id, gate.id))
		}
		payload["gate"], payload["messages"], payload["workers"] = gate.id, msgs, workers
		if err := t.event(evt{typ: "team_up", participant: gate.id, team: teamID, ref: teamID, payload: payload}); err != nil {
			return err
		}
		if res.Text, err = t.reopenSummary(a.Team, gate, m); err != nil {
			return err
		}
		if msgs > 0 {
			wake = gate.id
		}
		return nil
	})
	if err != nil {
		return AgentResult{}, err
	}
	if wake != "" {
		e.notifyAfterCommit(wake)
	}
	return res, nil
}

type oldGateRow struct {
	participant
	ref string
}

// oldGate is who was the gate of a closed team: the member teams.gate_id names (team down keeps it),
// when it is a session that did not leave (a worker is never replaced by a session).
func (t *txn) oldGate(teamID string) (g oldGateRow, ok bool, err error) {
	g.participant, err = scanParticipant(t.QueryRowContext(t.ctx, `SELECT `+participantCols+`, COALESCE(harness_ref,'') FROM participants
		WHERE id=(SELECT gate_id FROM teams WHERE id=?) AND left_at IS NULL AND NOT `+workerRow, teamID), &g.ref)
	if errors.Is(err, sql.ErrNoRows) {
		return oldGateRow{}, false, nil
	}
	return g, err == nil, internal(err)
}

// reopenSummary is what the new gate reads after reopen: the team and template, each other
// member with its last state, the board pins, the gate's unacked mail, and how to go on.
func (t *txn) reopenSummary(team string, gate participant, m manifest) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Reopened team %s (template %s); you are %s (%s), its gate.\n", team, m.Template, gate.name, gate.role)
	rows, err := t.QueryContext(t.ctx, `SELECT name, COALESCE(role,''), COALESCE(mode,'')='headless', state, last_activity,
		(SELECT COUNT(*) FROM messages WHERE to_id=participants.id AND acked_at IS NULL)
		FROM participants WHERE team_id=? AND id<>? AND left_at IS NULL ORDER BY `+joinOrder, gate.team, gate.id)
	if err != nil {
		return "", internal(err)
	}
	defer rows.Close()
	var workers []string
	b.WriteString("Members:")
	n := 0
	for rows.Next() {
		var name, role, state string
		var headless bool
		var last int64
		var unacked int
		if err := rows.Scan(&name, &role, &headless, &state, &last, &unacked); err != nil {
			return "", internal(err)
		}
		kind := "session"
		if headless {
			kind = "worker"
			workers = append(workers, name)
		}
		fmt.Fprintf(&b, "\n- %s (%s, %s): %s, last active %s", name, role, kind, state,
			time.UnixMilli(last).Format("2006-01-02 15:04"))
		if unacked > 0 {
			fmt.Fprintf(&b, ", %d unacked message(s)", unacked)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return "", internal(err)
	}
	if n == 0 {
		b.WriteString(" none besides you")
	}
	pins, err := t.livePins(gate)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\nBoard: %d pin(s)", len(pins))
	for _, p := range pins {
		fmt.Fprintf(&b, "\n- #%d %s", p.Seq, firstLine(p.Body))
	}
	var unacked int
	if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NULL`,
		gate.id).Scan(&unacked); err != nil {
		return "", internal(err)
	}
	fmt.Fprintf(&b, "\nUnacked mail to you: %d (read it with %sinbox).", unacked, gate.toolPrefix)
	if len(workers) > 0 {
		fmt.Fprintf(&b, "\nWorkers are stopped: %sagent action=tail target=<name> shows where one was; "+
			"action=resume brings it back with its context.", gate.toolPrefix)
	}
	return b.String(), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100]) + "…"
	}
	return s
}
