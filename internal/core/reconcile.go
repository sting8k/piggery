package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Reconcile runs once at daemon start, before serving. It makes the state tables true again after
// the daemon stopped: every participant with state != gone (except pull participants, which have no
// process or connection) is decided in its own transaction (state + `reconcile` event + notice),
// with driver calls outside it. It never acks mail and never respawns.
func (e *Engine) Reconcile(ctx context.Context) error {
	if err := e.backfillGates(ctx); err != nil {
		return err
	}
	type row struct {
		id, run, mode, state, harness string
	}
	// Pull participants (CLI, never identified by a harness) have no process or connection
	// to lose, so a restart does not change them.
	rows, err := e.db.QueryContext(ctx, `SELECT id, run_id, COALESCE(mode,''), state, COALESCE(harness,'') FROM participants
		WHERE state<>'gone' AND COALESCE(mode,'')<>'pull'`)
	if err != nil {
		return internal(err)
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.run, &r.mode, &r.state, &r.harness); err != nil {
			rows.Close()
			return internal(err)
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return internal(err)
	}
	for _, r := range todo {
		var d decision
		if r.mode == "headless" {
			d = e.inspectWorker(ctx, r.id, r.run, r.harness)
		} else {
			d = decision{state: "gone", reason: "daemon_restart"}
		}
		if err := e.applyReconcile(ctx, r.id, r.run, r.mode == "headless", d); err != nil {
			return err
		}
	}
	return nil
}

// backfillGates (and dropStaleRows) repair teams at start: it stores the gate of every team that has none yet (a DB from before schema v25, or a
// team that had no member that could be one): nextGate, as when a gate leaves. It runs before the
// states are decided below, and a team whose manifest cannot be read is skipped.
func (e *Engine) backfillGates(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, `SELECT id FROM teams WHERE gate_id IS NULL`)
	if err != nil {
		return internal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return internal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return internal(err)
	}
	for _, id := range ids {
		if err := e.inTx(ctx, func(t *txn) error {
			g, ok, err := t.nextGate(id)
			if err != nil || !ok {
				return nil
			}
			return t.setGate(id, g.id)
		}); err != nil {
			return err
		}
	}
	return e.dropStaleRows(ctx)
}

// dropStaleRows ends, in every open team, the member rows whose session went on as a newer row
// (dropSuperseded): a DB from before the reopen did it leaves them. Their mail goes to the gate.
func (e *Engine) dropStaleRows(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, `SELECT id FROM teams WHERE closed_at IS NULL`)
	if err != nil {
		return internal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return internal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return internal(err)
	}
	for _, id := range ids {
		if err := e.inTx(ctx, func(t *txn) error {
			n, err := t.dropSuperseded(id)
			if err != nil || n == 0 {
				return err
			}
			_, _, _, _, err = t.rerouteToGate(id)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// decision is what reconcile does with one participant.
type decision struct {
	state  string // gone | parked
	reason string
	proc   *Proc
	exit   *Exit // recorded on the run's process row when set
}

// inspectWorker decides a headless worker's fate. Driver calls only; no DB writes.
func (e *Engine) inspectWorker(ctx context.Context, id, run, harness string) decision {
	drv := e.runtimeFor(harness)
	proc, exited, err := e.processOf(ctx, id, run)
	lost := &Exit{Code: -1, Signal: "lost"}
	switch {
	case err != nil:
		return decision{state: "parked", reason: "process_row: " + err.Error()}
	case proc == nil:
		return decision{state: "gone", reason: "no_process", exit: lost}
	case exited:
		return decision{state: "gone", reason: "already_exited", proc: proc}
	case drv == nil:
		return decision{state: "parked", reason: "no_runtime_driver", proc: proc}
	}
	st, err := drv.Inspect(ctx, *proc)
	if err != nil {
		return decision{state: "parked", reason: "inspect_failed: " + err.Error(), proc: proc}
	}
	switch st {
	case ProcDead:
		return decision{state: "gone", reason: "dead", proc: proc, exit: lost}
	case ProcReused:
		return decision{state: "gone", reason: "pid_reused", proc: proc, exit: lost} // no signal
	case ProcOurs:
		x, err := drv.KillVerified(ctx, *proc)
		if err != nil {
			return decision{state: "parked", reason: "kill_failed: " + err.Error(), proc: proc}
		}
		return decision{state: "gone", reason: "killed", proc: proc, exit: &x}
	}
	return decision{state: "parked", reason: fmt.Sprintf("inspect returned %q", st), proc: proc}
}

// WorkerProcess is the process a worker's identify and harness.event must come from: bound is false
// for a participant that is not a spawned worker, or whose current run has no process recorded yet
// (the driver is still starting it: nothing nested in it can run). A recorded process that exited
// has pid 0, so nothing matches it.
func (e *Engine) WorkerProcess(ctx context.Context, participantID string) (pid int, start int64, bound bool, err error) {
	var mode, run sql.NullString
	err = e.db.QueryRowContext(ctx, `SELECT mode, run_id FROM participants WHERE id=?`, participantID).Scan(&mode, &run)
	if errors.Is(err, sql.ErrNoRows) || mode.String != modeHeadless {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	p, exited, err := e.processOf(ctx, participantID, run.String)
	if err != nil || p == nil {
		return 0, 0, false, err
	}
	if exited {
		return 0, 0, true, nil
	}
	return p.PID, p.StartTime, true, nil
}

// processOf returns the recorded process of (participant, run), nil when there is none.
func (e *Engine) processOf(ctx context.Context, id, run string) (*Proc, bool, error) {
	var p Proc
	var cmd string
	var exitedAt sql.NullInt64
	err := e.db.QueryRowContext(ctx, `SELECT pid, pgid, start_time, cmdline, exited_at FROM processes
		WHERE participant_id=? AND run_id=?`, id, run).Scan(&p.PID, &p.PGID, &p.StartTime, &cmd, &exitedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal([]byte(cmd), &p.Cmdline); err != nil {
		return nil, false, err
	}
	return &p, exitedAt.Valid, nil
}

// applyReconcile writes one participant's decision: exit, state, `reconcile` event, and for a
// worker that became gone or parked a notice from engine to its reports_to. A worker already
// parked that stays parked gets no second notice.
func (e *Engine) applyReconcile(ctx context.Context, id, run string, worker bool, d decision) error {
	var notified string
	err := e.inTx(ctx, func(t *txn) error {
		p, ok, err := t.participantByID(id)
		if err != nil || !ok || p.run != run || p.state == "gone" {
			return err // changed meanwhile: not ours to decide
		}
		wasParked := p.state == "parked"
		if d.exit != nil {
			x := *d.exit
			if _, err := t.markExited(id, run, x); err != nil {
				return err
			}
		}
		payload := map[string]any{"decision": d.state, "reason": d.reason, "from_state": p.state}
		if d.proc != nil {
			payload["pid"] = d.proc.PID
		}
		if d.exit != nil {
			payload["exit_code"], payload["signal"] = d.exit.Code, d.exit.Signal
		}
		if err := t.event(evt{typ: "reconcile", participant: id, team: p.team, run: run, payload: payload}); err != nil {
			return err
		}
		if err := t.setState(&p, d.state, "reconcile"); err != nil {
			return err
		}
		if !worker || p.reportsTo == "" || (wasParked && d.state == "parked") {
			return nil
		}
		var prefix string // the notice is for reports_to: its tool names
		if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(tool_prefix,'') FROM participants WHERE id=?`,
			p.reportsTo).Scan(&prefix); err != nil {
			return internal(err)
		}
		body := fmt.Sprintf("worker %s was lost in a daemon restart (%s); its unacked mail is kept; resume it with %sagent action=resume target=%s",
			p.name, d.reason, prefix, p.name)
		if d.state == "parked" {
			body = fmt.Sprintf("worker %s could not be verified after a daemon restart (%s) and is parked; its unacked mail is kept; "+
				"check its process, then resume it with %sagent action=resume target=%s", p.name, d.reason, prefix, p.name)
		}
		// A notice is plain mail from engine: nothing in core reacts to mail, so it cannot
		// trigger another notice.
		msg := newID(t.now)
		if _, err := t.insertMessage(msg, "", p.team, AddrEngine, p.reportsTo, "", "", "", "", body); err != nil {
			return err
		}
		notified = p.reportsTo
		return nil
	})
	if err == nil && notified != "" {
		e.notifyAfterCommit(notified)
	}
	return err
}
