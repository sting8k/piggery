package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Adapter events: for a harness whose adapter has no batch logic of its own (Claude hooks), the
// daemon counts batches and applies the ack rule here, in one place. A batch is one turn:
// turn_start opens batch n+1 of the run, keyed by the harness's turn id (prompt_id); mail given to
// the turn (at its start, after a tool call, or by blocking its end) is recorded in that batch;
// turn_end ok for the same key acks it. A failed or interrupted turn, a turn that never ends (Esc),
// or a key that does not match acks nothing: its mail stays pending and is given again.

// maxMailText bounds the mail text given at once: Claude moves an additionalContext over 10k
// characters to a file. Mail left over is given at the next tool call or turn end.
const maxMailText = 9000

// maxBlocks bounds consecutive blocked turn ends (Claude allows 8 Stop blocks in a row).
const maxBlocks = 8

// HarnessEvent records one adapter event of the caller's current run and returns what the
// adapter shows the model.
func (e *Engine) HarnessEvent(ctx context.Context, c Caller, a HarnessEventArgs) (HarnessEventResult, error) {
	var res HarnessEventResult
	wake, held := false, false
	var notice string // the notice the turn's end wrote to notify
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.caller(c, "harness.event")
		if err != nil {
			return err
		}
		held = p.state == "awaiting_permission"
		switch a.Event {
		case HarnessSessionStart:
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET harness_ref=COALESCE(harness_ref,NULLIF(?,'')),
				session_ref=COALESCE(NULLIF(?,''),session_ref) WHERE id=?`, a.HarnessRef, a.HarnessRef, p.id); err != nil {
				return internal(err)
			}
			if p.state == "gone" || p.state == "requested" || p.state == "starting" {
				if err := t.setState(&p, "idle", "session_start"); err != nil {
					return err
				}
			}
			if a.Source == "clear" || a.Source == "compact" {
				// The conversation was wiped or summarized; the card the MCP server gave at start
				// is gone with it (seen live: the model forgot piggery after /clear).
				return t.giveCard(p, &res, "[piggery] you are in piggery:")
			}
			return nil
		case HarnessTurnStart:
			if a.Wake {
				// A wake or a reconnect asks for mail, and the model has not run: with nothing to give
				// (read elsewhere, or none) there is no turn, so no batch, no working state, and no
				// last_turn_end. A turn the adapter already opened under this key goes on as one.
				if quiet, err := t.nothingToGive(p, a.PromptID); err != nil || quiet {
					return err
				}
			}
			n, err := t.openTurn(p, a.PromptID)
			if err != nil {
				return err
			}
			if err := t.setState(&p, "working", "turn_start"); err != nil {
				return err
			}
			if res.Text, err = t.giveMail(p, n); err != nil {
				return err
			}
			return t.newCard(p, &res)
		case HarnessToolBoundary:
			if held { // the tool ran: the prompt is answered
				if err := t.setState(&p, "working", "tool_boundary"); err != nil {
					return err
				}
			}
			b, ok, err := t.openTurnBatch(p)
			if err != nil || !ok || (a.PromptID != "" && b.promptID != a.PromptID) {
				return err
			}
			if res.Text, err = t.giveMail(p, b.n); err != nil {
				return err
			}
			return t.newCard(p, &res)
		case HarnessTurnEnd:
			res, wake, notice, err = t.endTurn(p, a)
			return err
		case HarnessPermission:
			held = false
			return t.setState(&p, "awaiting_permission", "permission_wait")
		case HarnessIdle:
			held = false // like an abort: what the closed turn had does not wake it
			if err := t.closeOpenTurns(p.run); err != nil {
				return err
			}
			return t.setState(&p, "idle", "idle")
		case HarnessModelChanged:
			_, err := t.ExecContext(t.ctx, `UPDATE participants SET session_model=?, session_thinking=? WHERE id=?`,
				nullStr(a.Model), nullStr(a.Thinking), p.id)
			return internal(err)
		case HarnessSessionEnd:
			if a.HarnessRef != "" {
				// The end of a session id the participant no longer runs (Codex ends the old id
				// ~30 s after /clear started the new one): not the participant's end.
				var cur sql.NullString
				if err := t.QueryRowContext(t.ctx, `SELECT session_ref FROM participants WHERE id=?`, p.id).Scan(&cur); err != nil {
					return internal(err)
				}
				if cur.Valid && cur.String != a.HarnessRef {
					return nil
				}
			}
			return t.setState(&p, "gone", "session_end")
		}
		return errf(CodeInvalid, "unknown harness event %q", a.Event)
	})
	if err != nil {
		return HarnessEventResult{}, err
	}
	if wake || held {
		e.notifyAfterCommit(c.ParticipantID)
	}
	if notice != "" {
		e.notifyHookAfterCommit(notice)
	}
	return res, nil
}

type turnBatch struct {
	n        int64
	promptID string
	blocks   int
}

// openTurnBatch is the run's turn in progress: its adapter batch not ended.
func (t *txn) openTurnBatch(p participant) (turnBatch, bool, error) {
	var b turnBatch
	err := t.QueryRowContext(t.ctx, `SELECT batch_seq, prompt_id, blocks FROM batches
		WHERE run_id=? AND prompt_id IS NOT NULL AND ended_at IS NULL AND completed_at IS NULL
		ORDER BY batch_seq DESC LIMIT 1`, p.run).Scan(&b.n, &b.promptID, &b.blocks)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	return b, err == nil, internal(err)
}

// openTurn opens the batch of the turn promptID: the one in progress when it has that key,
// else a new batch after the run's highest, ending (not acking) a turn left open.
func (t *txn) openTurn(p participant, promptID string) (int64, error) {
	if promptID == "" {
		return 0, errf(CodeInvalid, "turn_start needs prompt_id")
	}
	b, ok, err := t.openTurnBatch(p)
	if err != nil {
		return 0, err
	}
	if ok && b.promptID == promptID {
		return b.n, nil
	}
	if ok {
		if _, err := t.ExecContext(t.ctx, `UPDATE batches SET ended_at=? WHERE run_id=? AND batch_seq=?`,
			t.now, p.run, b.n); err != nil {
			return 0, internal(err)
		}
	}
	var n int64
	if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(MAX(batch_seq),0)+1 FROM batches WHERE run_id=?`,
		p.run).Scan(&n); err != nil {
		return 0, internal(err)
	}
	if _, err := t.ExecContext(t.ctx, `INSERT INTO batches(run_id, batch_seq, opened_at, prompt_id) VALUES (?,?,?,?)`,
		p.run, n, t.now, promptID); err != nil {
		return 0, internal(err)
	}
	return n, nil
}

// nothingToGive: p has no mail waiting, no role card due, and no open turn under promptID.
func (t *txn) nothingToGive(p participant, promptID string) (bool, error) {
	if b, ok, err := t.openTurnBatch(p); err != nil || (ok && b.promptID == promptID) {
		return false, err
	}
	if k, err := t.pendingMail(p, 0); err != nil || k > 0 {
		return false, err
	}
	var seen, caps sql.NullString
	if err := t.QueryRowContext(t.ctx, `SELECT card_hash, capabilities FROM participants WHERE id=?`, p.id).Scan(&seen, &caps); err != nil {
		return false, internal(err)
	}
	return cardKey(p) == seen.String || !caps.Valid || !lacksCap(caps, CapSystemPrompt), nil
}

// newCard puts p's role card before res.Text when it changed since p's session last got one (found,
// admit, close, reopen mid-session) and p's harness declared capabilities without system_prompt:
// nothing else would show the model its new role. A harness that puts the card in its system
// prompt, or declared nothing, is left alone.
func (t *txn) newCard(p participant, res *HarnessEventResult) error {
	var seen sql.NullString
	if err := t.QueryRowContext(t.ctx, `SELECT card_hash FROM participants WHERE id=?`, p.id).Scan(&seen); err != nil {
		return internal(err)
	}
	if cardKey(p) == seen.String {
		return nil
	}
	return t.giveCard(p, res, "[piggery] your role changed:")
}

// giveCard puts p's role card, after head, before res.Text and records it as seen, for a
// harness that declared capabilities without system_prompt (else nothing).
func (t *txn) giveCard(p participant, res *HarnessEventResult, head string) error {
	var caps sql.NullString
	if err := t.QueryRowContext(t.ctx, `SELECT capabilities FROM participants WHERE id=?`, p.id).Scan(&caps); err != nil {
		return internal(err)
	}
	if !caps.Valid || !lacksCap(caps, CapSystemPrompt) {
		return nil
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
	res.Text = strings.TrimSpace(head + "\n" + card + "\n\n" + res.Text)
	return nil
}

// cardKey is what makes p's role card another: its team, role and name. The teammates the card
// lists come and go without being a new role.
func cardKey(p participant) string { return hashToken(p.team + "\x00" + p.role + "\x00" + p.name) }

// RuntimeTurnFailed ends the worker's open turn as failed (no ack, no self-wake) when its runtime
// driver sees the turn fail and the harness sends no end of its own (Codex: a bad model name ends
// in turn/completed failed with no Stop hook). A run that is no longer current is ignored, and so
// is an open turn with another key (the next turn's hook may come first).
func (e *Engine) RuntimeTurnFailed(ctx context.Context, participantID, runID, key string) error {
	var notice string
	err := e.inTx(ctx, func(t *txn) error {
		p, ok, err := t.participantByID(participantID)
		if err != nil || !ok || p.run != runID {
			return err
		}
		if b, open, err := t.openTurnBatch(p); err != nil || (open && b.promptID != key) {
			return err
		}
		_, _, notice, err = t.endTurn(p, HarnessEventArgs{Event: HarnessTurnEnd, PromptID: key, Outcome: HarnessOutcomeFailed})
		return err
	})
	if err == nil && notice != "" {
		e.notifyHookAfterCommit(notice)
	}
	return err
}

// SessionRef is the session id participantID runs now ("" unknown), for a wake that names it.
func (e *Engine) SessionRef(ctx context.Context, participantID string) string {
	var ref sql.NullString
	e.db.QueryRowContext(ctx, `SELECT COALESCE(session_ref, harness_ref) FROM participants WHERE id=?`, participantID).Scan(&ref)
	return ref.String
}

// closeOpenTurns ends the run's open adapter turns without an ack and without a wake: their mail
// comes again at the next turn, and new mail wakes.
func (t *txn) closeOpenTurns(run string) error {
	_, err := t.ExecContext(t.ctx, `UPDATE batches SET ended_at=? WHERE run_id=? AND prompt_id IS NOT NULL
		AND ended_at IS NULL AND completed_at IS NULL`, t.now, run)
	return internal(err)
}

// endTurn applies turn_end. ok on the turn in progress: mail it has not seen blocks the end (up to
// maxBlocks in a row), else the batch completes and acks. Any other end (failed, interrupted,
// another turn's key) ends the open turn without an ack. wake: the turn ended ok and mail is still
// pending; a failed or aborted turn does not wake itself (abort). notice: the id of the notice the
// end wrote to notify, if any (gateNotice); the caller runs the hook after commit.
func (t *txn) endTurn(p participant, a HarnessEventArgs) (res HarnessEventResult, wake bool, notice string, err error) {
	b, ok, err := t.openTurnBatch(p)
	if err != nil {
		return res, false, "", err
	}
	ours := ok && a.PromptID == b.promptID && a.Outcome == HarnessOutcomeOK
	if ours && b.blocks < maxBlocks {
		text, err := t.giveMail(p, b.n)
		if err != nil || text != "" {
			if err == nil {
				_, err = t.ExecContext(t.ctx, `UPDATE batches SET blocks=blocks+1 WHERE run_id=? AND batch_seq=?`, p.run, b.n)
				err = internal(err)
			}
			return HarnessEventResult{Text: text, Block: true}, false, "", err
		}
	}
	if ours {
		if _, err := t.complete(p, b.n); err != nil {
			return res, false, "", err
		}
	}
	if ok {
		if _, err := t.ExecContext(t.ctx, `UPDATE batches SET ended_at=? WHERE run_id=? AND batch_seq=?`,
			t.now, p.run, b.n); err != nil {
			return res, false, "", internal(err)
		}
	}
	if _, err := t.ExecContext(t.ctx, `UPDATE participants SET last_turn_end=?, last_activity=? WHERE id=?`,
		t.now, t.now, p.id); err != nil {
		return res, false, "", internal(err)
	}
	if err := t.setState(&p, "idle", "turn_end"); err != nil {
		return res, false, "", err
	}
	left := 0
	if a.Outcome == HarnessOutcomeOK {
		if left, err = t.pendingMail(p, 0); err != nil {
			return res, false, "", err
		}
	}
	if ok && a.PromptID == b.promptID { // the turn that ended is the open one
		if notice, err = t.gateNotice(p, b.n, a.Outcome, left); err != nil {
			return res, false, "", err
		}
	}
	return res, left > 0, notice, nil
}

// pendingMail counts p's mail waiting (unacked, not held) and not yet given in batch n (0: any).
func (t *txn) pendingMail(p participant, n int64) (int, error) {
	var k int
	err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NULL AND held_reason IS NULL
		AND id NOT IN (SELECT message_id FROM deliveries WHERE run_id=? AND batch_seq=?)`, p.id, p.run, n).Scan(&k)
	return k, internal(err)
}

// giveMail records p's mail not yet given in batch n as delivered in it and renders it, as
// much as fits maxMailText (at least one message); "" when there is none.
func (t *txn) giveMail(p participant, n int64) (string, error) {
	return t.giveMailWhere(p, n, `to_id=? AND acked_at IS NULL AND held_reason IS NULL
		AND id NOT IN (SELECT message_id FROM deliveries WHERE run_id=? AND batch_seq=?)`, p.id, p.run, n)
}

// giveMailWhere is giveMail over p's messages that match where (readMessages' filter).
func (t *txn) giveMailWhere(p participant, n int64, where string, args ...any) (string, error) {
	msgs, err := t.readMessages(p, where, args...)
	if err != nil || len(msgs) == 0 {
		return "", err
	}
	now := time.UnixMilli(t.now)
	var given []Delivered
	text := ""
	for _, m := range msgs {
		again, err := t.redelivered(m.ID, 0)
		if err != nil {
			return "", err
		}
		next := RenderMail(append(given, Delivered{Redelivered: again, Message: m}), "", p.toolPrefix, now)
		if len(given) > 0 && len(next) > maxMailText {
			break
		}
		r, err := t.ExecContext(t.ctx, `INSERT INTO deliveries(message_id, run_id, batch_seq, delivered_at) VALUES (?,?,?,?)`,
			m.ID, p.run, n, t.now)
		if err != nil {
			return "", internal(err)
		}
		id, err := r.LastInsertId()
		if err != nil {
			return "", internal(err)
		}
		given = append(given, Delivered{DeliveryID: id, Redelivered: again, Message: m})
		text = next
	}
	return text, nil
}
