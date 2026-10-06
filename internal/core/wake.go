package core

import (
	"cmp"
	"context"
	"database/sql"
)

// Mail wakes a gone member: new mail for a gone member of an open team, other than
// the team's gate (teamGate: stored, live or not), resumes its session as a headless worker, as
// `agent resume` does, and the mail comes with the run. The gate is never woken: a gone gate's mail
// is a queue, given when the person comes back. A solo is its own gate, a member that left or a
// member of a closed team is never woken.
//
// The wake goes through resumeWorker, so limits.max_respawn_per_hour (parked + respawn_limit + one
// notice), limits.concurrency, the cwd bounds and the directory check all apply; a refusal is no
// error for the sender, the mail stays queued. wake_seq bounds it: a wake needs unacked mail newer
// than the last one that woke the member, so a member that dies again with the same mail unacked is
// not woken by it again, whatever calls notifyAfterCommit; only new mail wakes it again.

// wakeTarget is resumeWorker's find for a wake of participant id by mail: the member to resume
// (the "caller" is only its team) or why it is not woken.
func (e *Engine) wakeTarget(t *txn, id string) (participant, participant, error) {
	w, ok, err := t.participantByID(id)
	if err != nil || !ok {
		return participant{}, w, errf(CodeNotFound, "no participant %s", id)
	}
	var left, closed sql.NullInt64
	var ref, sessionRef string
	var fresh bool
	if w.team != "" {
		if err := t.QueryRowContext(t.ctx, `SELECT left_at, COALESCE(harness_ref,''), COALESCE(session_ref,''),
			(SELECT closed_at FROM teams WHERE id=participants.team_id),
			EXISTS (SELECT 1 FROM messages WHERE to_id=participants.id AND acked_at IS NULL AND held_reason IS NULL
				AND seq > participants.wake_seq) FROM participants WHERE id=?`, id).Scan(&left, &ref, &sessionRef, &closed, &fresh); err != nil {
			return participant{}, w, internal(err)
		}
	}
	switch {
	case w.team == "" || w.state != "gone" || w.kind != "agent" || left.Valid || closed.Valid:
		return participant{}, w, errf(CodeInvalid, "%s is not a gone member of an open team", w.name)
	case cmp.Or(ref, sessionRef) == "" || e.runtimeFor(w.harness) == nil:
		return participant{}, w, errf(CodeUnsupported, "%s has no session a worker can resume", w.name)
	case !fresh:
		return participant{}, w, errf(CodeInvalid, "%s has no new mail to wake for", w.name)
	}
	gate, ok, err := t.teamGate(w.team)
	if err != nil {
		return participant{}, w, err
	}
	if ok && gate.id == w.id {
		return participant{}, w, errf(CodeInvalid, "%s is the team's gate: its mail waits for it", w.name)
	}
	return participant{team: w.team}, w, nil
}

// wakeable reports whether mail for participant id would wake it now.
func (e *Engine) wakeable(id string) bool {
	return e.inTx(context.Background(), func(t *txn) error {
		_, _, err := e.wakeTarget(t, id)
		return err
	}) == nil
}

// autoWake resumes participant id because mail came for it (the caller checked wakeable). An error
// is a refusal: the mail stays queued, nobody is told beyond what resume records.
func (e *Engine) autoWake(id string) error {
	_, err := e.resumeWorker(context.Background(), func(t *txn) (participant, participant, error) {
		return e.wakeTarget(t, id)
	}, "", true)
	return err
}

// becomeWorker is what a wake by mail changes in w's row inside resume's transaction: a member that
// was a person's session (mode not headless) goes on as a headless worker (participants.person stays
// 1; the person's join sets the mode it reports again, autojoin.go takeBack), and wake_seq moves to the
// newest unacked mail it is woken for. It returns the harness session to resume: a person's
// session continues in the one it had last.
func (t *txn) becomeWorker(id, mode, ref, sessionRef string) (string, error) {
	if mode != modeHeadless {
		if _, err := t.ExecContext(t.ctx, `UPDATE participants SET mode=? WHERE id=?`, modeHeadless, id); err != nil {
			return "", internal(err)
		}
		ref = cmp.Or(sessionRef, ref)
	}
	_, err := t.ExecContext(t.ctx, `UPDATE participants SET wake_seq=MAX(wake_seq, COALESCE((SELECT MAX(seq) FROM messages
		WHERE to_id=participants.id AND acked_at IS NULL AND held_reason IS NULL), 0)) WHERE id=?`, id)
	return ref, internal(err)
}
