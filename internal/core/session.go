package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Identify binds a harness process to the caller and returns its role card.
func (e *Engine) Identify(ctx context.Context, c Caller, a IdentifyArgs) (IdentifyResult, error) {
	if a.Mode == modeHeadless {
		a.Mode = "" // only spawn makes a participant headless
	}
	if !a.NewRun && !(c.ByHost && a.RunID == "") { // by host: its current run
		if a.RunID == "" {
			return IdentifyResult{}, errf(CodeInvalid, "run_id is required when new_run is false")
		}
		c.RunID = a.RunID // the caller gate then refuses a run that is not current (run.stale)
	}
	if a.ProtocolVersion < e.minProtocol {
		return IdentifyResult{}, &Error{Code: CodeUnsupported, RuleID: "protocol_version", Message: fmt.Sprintf(
			"this adapter speaks piggery protocol %d; the daemon needs %d or later (it speaks %d): run `piggery setup <harness>` and restart the session",
			a.ProtocolVersion, e.minProtocol, ProtocolVersion)}
	}
	var res IdentifyResult
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.caller(c, "identify")
		if err != nil {
			return err
		}
		if a.NewRun {
			p.run = newID(t.now)
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET run_id=?, last_turn_end=NULL WHERE id=?`,
				p.run, p.id); err != nil {
				return internal(err)
			}
		}
		// harness_ref is the session's durable identity: set once, never overwritten. A spawned
		// worker stays headless whatever mode its harness reports (reconcile, concurrency and
		// join.auto rely on it).
		if _, err := t.ExecContext(t.ctx, `UPDATE participants SET harness=COALESCE(?,harness),
			mode=CASE WHEN mode='headless' THEN mode ELSE COALESCE(?,mode) END,
			harness_ref=COALESCE(harness_ref,?), tool_prefix=?, session_model=?, session_thinking=?, last_activity=?,
			capabilities=CASE WHEN mode='headless' THEN capabilities ELSE ? END, protocol_version=? WHERE id=?`,
			nullStr(a.Harness), nullStr(a.Mode), nullStr(a.HarnessRef), nullStr(a.ToolPrefix), nullStr(a.Model),
			nullStr(a.Thinking), t.now, capsJSON(a.Capabilities), a.ProtocolVersion, p.id); err != nil {
			return internal(err)
		}
		p.toolPrefix = a.ToolPrefix
		// Reconnect of the same run: first the turn ends the adapter queued while the daemon was
		// gone, an ok end of an open turn of this run acking its batch; then a turn still open may
		// have ended unseen: close it without an ack (its mail comes again, at-least-once) so it
		// neither holds back wakes nor takes a late end as an ack.
		if !a.Again { // identify again on its connection (a role change) leaves its turn open
			for _, end := range a.Ended {
				var n int64
				err := t.QueryRowContext(t.ctx, `SELECT batch_seq FROM batches WHERE run_id=? AND prompt_id=?
					AND ended_at IS NULL AND completed_at IS NULL`, p.run, end.Key).Scan(&n)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return internal(err)
				}
				if end.Outcome == HarnessOutcomeOK {
					if _, err := t.complete(p, n); err != nil {
						return err
					}
				}
				if _, err := t.ExecContext(t.ctx, `UPDATE batches SET ended_at=? WHERE run_id=? AND batch_seq=?`,
					t.now, p.run, n); err != nil {
					return internal(err)
				}
			}
			if err := t.closeOpenTurns(p.run); err != nil {
				return err
			}
		}
		// A new run, a reconnect after gone, or a spawned worker's first identify: ready.
		if a.NewRun || p.state == "gone" || p.state == "requested" || p.state == "starting" {
			if err := t.setState(&p, "idle", "identify"); err != nil {
				return err
			}
		}
		m, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		card, err := t.roleCard(p, m)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE participants SET card_hash=? WHERE id=?`, cardKey(p), p.id); err != nil {
			return internal(err)
		}
		tools := m.Roles[p.role].Tools
		if tools == nil {
			tools = []string{}
		}
		res = IdentifyResult{ParticipantID: p.id, RunID: p.run, Name: p.name, TeamID: p.team, Role: p.role,
			Tools: tools, RoleCard: card, ProtocolVersion: ProtocolVersion}
		return nil
	})
	if err != nil {
		return IdentifyResult{}, err
	}
	return res, nil
}

// Presence records a harness lifecycle event for the caller's current run.
func (e *Engine) Presence(ctx context.Context, c Caller, a PresenceArgs) error {
	to := map[string]string{
		PresenceAgentStart:    "working",
		PresenceUIPromptStart: "awaiting_permission",
		PresenceUIPromptEnd:   "working",
		PresenceAgentSettled:  "idle",
		PresenceShutdown:      "gone",
	}
	target, isState := to[a.Event]
	if !isState && a.Event != PresenceTurnEnd && a.Event != PresenceModel {
		return errf(CodeInvalid, "unknown presence event %q", a.Event)
	}
	held := false
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.caller(c, "presence")
		if err != nil {
			return err
		}
		// A ui prompt that ends releases the mail and wakes held while it was open.
		held = p.state == "awaiting_permission" && isState && target != "awaiting_permission"
		switch a.Event {
		case PresenceTurnEnd:
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET last_turn_end=?, last_activity=? WHERE id=?`,
				t.now, t.now, p.id); err != nil {
				return internal(err)
			}
			return nil
		case PresenceModel:
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET session_model=?, session_thinking=? WHERE id=?`,
				nullStr(a.Model), nullStr(a.Thinking), p.id); err != nil {
				return internal(err)
			}
			return nil
		}
		return t.setState(&p, target, a.Event)
	})
	if err == nil && held {
		e.notifyAfterCommit(c.ParticipantID)
	}
	return err
}

// setState moves p to state `to` (state_since = now); no-op when already there. cause says why at
// the call site; state changes write no event.
func (t *txn) setState(p *participant, to, cause string) error {
	if p.state == to {
		return nil
	}
	if _, err := t.ExecContext(t.ctx, `UPDATE participants SET state=?, state_since=?, last_activity=? WHERE id=?`,
		to, t.now, t.now, p.id); err != nil {
		return internal(err)
	}
	back := p.state == "gone" && p.team != ""
	p.state, p.stateSince = to, t.now
	if back {
		// A member coming back may give its team the gate it was waiting for (leave.go). The
		// gate is then this participant or an earlier one already live; it reads its inbox.
		_, err := t.backAtGate(*p)
		return err
	}
	return nil
}

// sharedPrompt is the Human's shared text for a role of a team's template, named as the manifest's
// `template:` (a solo: "", "solo"), read now, so an edit shows in the next card. "" when none applies.
func (t *txn) sharedPrompt(template, role string) string {
	if t.shared == nil {
		return ""
	}
	return t.shared(template, role)
}

// roleCard is the plain-text card put in every run's system prompt. It never contains tokens.
func (t *txn) roleCard(p participant, m manifest) (string, error) {
	if p.team == "" {
		return soloCard(p, t.taskforceLinesOf(p)) + t.sharedPrompt("", "solo"), nil
	}
	var team string
	if err := t.QueryRowContext(t.ctx, `SELECT name FROM teams WHERE id=?`, p.team).Scan(&team); err != nil {
		return "", internal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, role %q in piggery team %q (template %s).\n", p.name, p.role, team, m.Template)
	if ins := strings.TrimSpace(m.Roles[p.role].Instructions); ins != "" {
		b.WriteString("\n" + WithToolNames(ins, p.toolPrefix) + "\n")
	}
	b.WriteString(t.sharedPrompt(m.Template, p.role))
	rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+` FROM participants WHERE team_id=? AND id<>? ORDER BY `+joinOrder,
		p.team, p.id)
	if err != nil {
		return "", internal(err)
	}
	defer rows.Close()
	b.WriteString("\nOther participants in your team:\n")
	n := 0
	for rows.Next() {
		q, err := scanParticipant(rows)
		if err != nil {
			return "", internal(err)
		}
		fmt.Fprintf(&b, "- %s\n", label(p, q))
		n++
	}
	if err := rows.Err(); err != nil {
		return "", internal(err)
	}
	if n == 0 {
		b.WriteString("- (none yet)\n")
	}
	b.WriteString("\nMail from others arrives as a user message with a header naming the sender and the message's #N.")
	b.WriteString(toolTips(p.toolPrefix, m.Roles[p.role].Tools))
	extra, err := t.taskforceCard(p, m)
	if err != nil {
		return "", err
	}
	b.WriteString(extra)
	b.WriteString("\n")
	return b.String(), nil
}

// toolTips tells the model how to answer and look around using only tools its role has: a
// tip naming a tool the role lacks makes the model call a tool that does not exist.
func toolTips(prefix string, tools []string) string {
	has := map[string]bool{}
	for _, name := range tools {
		has[name] = true
	}
	var b strings.Builder
	if has["send"] {
		b.WriteString(" Reply with the " + prefix + "send tool: to=<sender name>, reply_to=<its #N>.")
	}
	if has["inbox"] {
		b.WriteString(" Use " + prefix + "inbox to read pending mail.")
	}
	if has["who"] {
		b.WriteString(" Use " + prefix + "who to see who is around.")
	}
	return b.String()
}
