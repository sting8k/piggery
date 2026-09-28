package core

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
)

// Mail through the runtime driver (deliver(batch)): for a worker whose driver declares CapDeliver,
// core hands each batch to the driver instead of an adapter taking it at a turn start or tool
// boundary, and the driver reports how the batch ended. The ack rule is the same: only a completion
// of that batch while its run is current acks; a cancelled batch is not acked and its mail is given
// again. Holding mail while the worker waits on a permission prompt and never giving mail twice
// stay here.

func delivers(d RuntimeDriver) bool { return slices.Contains(d.Capabilities(), CapDeliver) }

// deliver opens the next batch of the worker's run with its mail that is not in an open batch
// already and hands it to the driver. Nothing when there is none, when the worker has no
// process yet or is gone (start delivers once it runs); notifyAfterCommit already held it back
// while it waits on a permission prompt. A driver error ends the batch unacked.
func (e *Engine) deliver(participantID string, d RuntimeDriver) {
	// One at a time per worker, the batch's tx and its Deliver together: otherwise two senders
	// (or a completion's follow-up) could commit batch n first but reach the harness with n+1
	// first, and the model would read the mail out of order.
	mu, _ := e.deliverLocks.LoadOrStore(participantID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	ctx := context.Background()
	var dl Delivery
	if err := e.inTx(ctx, func(t *txn) error {
		p, ok, err := t.participantByID(participantID)
		if err != nil || !ok || p.state == "requested" || p.state == "gone" || p.state == "awaiting_permission" {
			return err
		}
		var n int64
		if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(MAX(batch_seq),0)+1 FROM batches WHERE run_id=?`,
			p.run).Scan(&n); err != nil {
			return internal(err)
		}
		text, err := t.giveMailWhere(p, n, `to_id=? AND acked_at IS NULL AND held_reason IS NULL
			AND id NOT IN (SELECT d.message_id FROM deliveries d JOIN batches b ON b.run_id=d.run_id AND b.batch_seq=d.batch_seq
				WHERE d.run_id=? AND b.ended_at IS NULL AND b.completed_at IS NULL)`, p.id, p.run)
		if err != nil || text == "" {
			return err
		}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO batches(run_id, batch_seq, opened_at) VALUES (?,?,?)`,
			p.run, n, t.now); err != nil {
			return internal(err)
		}
		dl = Delivery{RunID: p.run, Batch: n, Text: text}
		return t.setState(&p, "working", "deliver")
	}); err != nil || dl.Batch == 0 {
		return
	}
	if err := d.Deliver(participantID, dl); err != nil {
		e.inTx(ctx, func(t *txn) error { return t.endDelivery(participantID, dl.RunID, dl.Batch, "deliver_error") })
	}
}

// DeliveryEnded records how the driver's batch ended: completed acks exactly its mail,
// cancelled ends it unacked. A report for a run that is no longer current, or for a batch
// already ended, changes nothing: a piggery abort ends the run's open batches before it asks the
// driver (endDeliveries), so the cancellations it causes land here as already ended and do not
// wake the worker. A batch the harness cancelled on its own leaves mail no one would wake the
// worker for: it is delivered again, once until a batch of the run completes (batches.end_reason
// is the record), and audited (event delivery_cancelled). Mail still waiting after a completion is delivered next.
func (e *Engine) DeliveryEnded(ctx context.Context, participantID, runID string, batch int64, outcome string) error {
	if outcome != DeliveryCompleted && outcome != DeliveryCancelled {
		return errf(CodeInvalid, "unknown delivery outcome %q", outcome)
	}
	wake := false
	err := e.inTx(ctx, func(t *txn) error {
		p, ok, err := t.participantByID(participantID)
		if err != nil || !ok || p.run != runID {
			return err
		}
		open, err := t.batchOpen(runID, batch)
		if err != nil || !open {
			return err
		}
		if outcome == DeliveryCancelled {
			var before int // harness cancels since the run's last completed batch
			if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM batches WHERE run_id=? AND end_reason=?
				AND batch_seq > COALESCE((SELECT MAX(batch_seq) FROM batches WHERE run_id=? AND completed_at IS NOT NULL),0)`,
				runID, DeliveryCancelled, runID).Scan(&before); err != nil {
				return internal(err)
			}
			wake = before == 0
		}
		if err := t.endDelivery(participantID, runID, batch, outcome); err != nil {
			return err
		}
		if outcome == DeliveryCancelled {
			return t.event(evt{typ: "delivery_cancelled", participant: p.id, team: p.team, run: p.run,
				payload: map[string]any{"batch": batch, "redelivered": wake}})
		}
		left, err := t.pendingMail(p, 0)
		wake = left > 0
		return err
	})
	if err == nil && wake {
		// Not on the caller's goroutine: the driver reports from its stdout reader, and the
		// next delivery calls back into the driver.
		go e.notifyAfterCommit(participantID)
	}
	return err
}

// batchOpen reports whether batch n of run exists and has neither ended nor completed.
func (t *txn) batchOpen(run string, n int64) (bool, error) {
	var ended, completed sql.NullInt64
	err := t.QueryRowContext(t.ctx, `SELECT ended_at, completed_at FROM batches WHERE run_id=? AND batch_seq=?`,
		run, n).Scan(&ended, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && !ended.Valid && !completed.Valid, internal(err)
}

// endDeliveries ends every open batch of p's run without an ack (a piggery abort, before it asks
// the driver) and makes the worker idle: the cancellations the abort causes then change nothing.
func (t *txn) endDeliveries(participantID string) error {
	p, ok, err := t.participantByID(participantID)
	if err != nil || !ok {
		return err
	}
	if _, err := t.ExecContext(t.ctx, `UPDATE batches SET ended_at=?, end_reason='abort' WHERE run_id=? AND ended_at IS NULL
		AND completed_at IS NULL`, t.now, p.run); err != nil {
		return internal(err)
	}
	return t.settleIdle(p, "abort")
}

// UnbatchedTurn records a turn the harness runs that no delivered batch started (Claude wakes
// itself when a background task ends): the worker is working while it runs and idle once it
// and every open batch have ended. It acks nothing and opens no batch; mail that comes meanwhile
// is delivered as to a busy worker. A report for a run that is no longer current changes nothing.
func (e *Engine) UnbatchedTurn(ctx context.Context, participantID, runID, event string) error {
	if event != TurnStarted && event != TurnEnded {
		return errf(CodeInvalid, "unknown turn event %q", event)
	}
	return e.inTx(ctx, func(t *txn) error {
		p, ok, err := t.participantByID(participantID)
		if err != nil || !ok || p.run != runID || p.state == "gone" {
			return err
		}
		if event == TurnStarted {
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET unbatched_run=?, last_activity=? WHERE id=?`,
				runID, t.now, p.id); err != nil {
				return internal(err)
			}
			return t.setState(&p, "working", "turn_started")
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE participants SET unbatched_run=NULL WHERE id=?`, p.id); err != nil {
			return internal(err)
		}
		return t.settleIdle(p, "turn_ended")
	})
}

// settleIdle makes a working delivering worker idle once its run has no open batch and no turn
// without a batch running.
func (t *txn) settleIdle(p participant, cause string) error {
	if p.state != "working" {
		return nil
	}
	var busy int
	if err := t.QueryRowContext(t.ctx, `SELECT
		(SELECT COUNT(*) FROM batches WHERE run_id=? AND ended_at IS NULL AND completed_at IS NULL)
		+ (SELECT COUNT(*) FROM participants WHERE id=? AND unbatched_run=?)`, p.run, p.id, p.run).Scan(&busy); err != nil {
		return internal(err)
	}
	if busy > 0 {
		return nil
	}
	if _, err := t.ExecContext(t.ctx, `UPDATE participants SET last_turn_end=?, last_activity=? WHERE id=?`,
		t.now, t.now, p.id); err != nil {
		return internal(err)
	}
	return t.setState(&p, "idle", cause)
}

// endDelivery ends batch n of runID if it is the participant's current run and the batch is
// open: completed acks it, any other reason ends it without an ack and is kept as its end_reason. The worker is idle once no
// batch of its run is open.
func (t *txn) endDelivery(participantID, runID string, n int64, reason string) error {
	p, ok, err := t.participantByID(participantID)
	if err != nil || !ok || p.run != runID {
		return err
	}
	if open, err := t.batchOpen(runID, n); err != nil || !open {
		return err
	}
	if reason == DeliveryCompleted {
		if _, err := t.complete(p, n); err != nil {
			return err
		}
	} else if _, err := t.ExecContext(t.ctx, `UPDATE batches SET ended_at=?, end_reason=? WHERE run_id=? AND batch_seq=?`,
		t.now, reason, runID, n); err != nil {
		return internal(err)
	}
	return t.settleIdle(p, "delivery_end")
}
