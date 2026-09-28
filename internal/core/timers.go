package core

import (
	"context"
	"encoding/json"
)

// minTimerEveryMs is the floor for repeating timers.
const minTimerEveryMs = 60_000

type timerPayload struct {
	Body string `json:"body"`
}

// WatchAdd schedules a timer owned by the caller that messages a participant in view.
func (e *Engine) WatchAdd(ctx context.Context, c Caller, a TimerArgs) (Timer, error) {
	if a.Body == "" || a.InMs < 0 || a.EveryMs < 0 {
		return Timer{}, errf(CodeInvalid, "body is required; in_ms and every_ms must be >= 0")
	}
	// Timer mail comes from engine and is exempt from the mail limits, so a fast repeating
	// timer would be an unlimited storm: repeats are at most once a minute.
	if a.EveryMs > 0 && a.EveryMs < minTimerEveryMs {
		return Timer{}, errf(CodeInvalid, "every_ms must be 0 (fire once) or at least %d", minTimerEveryMs)
	}
	var tm Timer
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "watch.add", "send")
		if err != nil {
			return err
		}
		q, err := t.resolveInView(p, a.To, "watch.add")
		if err != nil {
			return err
		}
		// The fired message comes from engine, so routing is checked here, as Send would.
		m, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		if rule, ok := m.route(p.role, q.role); !ok {
			return deny(&p, "watch.add", "routing", rule, "routing does not allow "+p.role+" -> "+q.role, nil,
				map[string]any{"to": a.To})
		}
		payload, err := json.Marshal(timerPayload{Body: a.Body})
		if err != nil {
			return internal(err)
		}
		tm = Timer{ID: newID(t.now), Owner: p.id, Target: q.id, FireAt: t.now + a.InMs, EveryMs: a.EveryMs,
			Body: a.Body, Active: true}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO timers(id, team_id, owner, target, fire_at, every_ms, payload)
			VALUES (?,?,?,?,?,?,?)`, tm.ID, p.team, tm.Owner, tm.Target, tm.FireAt, nullInt(tm.EveryMs), string(payload)); err != nil {
			return internal(err)
		}
		return nil
	})
	if err != nil {
		return Timer{}, err
	}
	return tm, nil
}

// WatchList returns the caller's active timers, soonest first.
func (e *Engine) WatchList(ctx context.Context, c Caller) ([]Timer, error) {
	var out []Timer
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "watch.list", "send")
		if err != nil {
			return err
		}
		out, err = t.timers(`owner=? AND active=1`, p.id)
		return err
	})
	return out, err
}

// FireDue fires active timers with fire_at <= now: one message from "engine" to the target,
// event timer_fired, then deactivate (or reschedule when every_ms > 0). Called by serve's ticker.
func (e *Engine) FireDue(ctx context.Context) (int, error) {
	var targets []string
	err := e.inTx(ctx, func(t *txn) error {
		targets = nil
		due, err := t.timers(`active=1 AND fire_at<=?`, t.now)
		if err != nil {
			return err
		}
		for _, tm := range due {
			var team string
			if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(team_id,'') FROM timers WHERE id=?`, tm.ID).Scan(&team); err != nil {
				return internal(err)
			}
			id := newID(t.now)
			if _, err := t.insertMessage(id, "", team, AddrEngine, tm.Target, "", id, "", false, "", "", tm.Body); err != nil {
				return err
			}
			if tm.EveryMs > 0 {
				_, err = t.ExecContext(t.ctx, `UPDATE timers SET fire_at=? WHERE id=?`, t.now+tm.EveryMs, tm.ID)
			} else {
				_, err = t.ExecContext(t.ctx, `UPDATE timers SET active=0 WHERE id=?`, tm.ID)
			}
			if err != nil {
				return internal(err)
			}
			targets = append(targets, tm.Target)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, id := range targets {
		e.notifyAfterCommit(id)
	}
	return len(targets), nil
}

func (t *txn) timers(where string, args ...any) ([]Timer, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT id, owner, target, fire_at, COALESCE(every_ms,0), payload, active
		FROM timers WHERE `+where+` ORDER BY fire_at, id`, args...)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	out := []Timer{}
	for rows.Next() {
		var tm Timer
		var payload string
		if err := rows.Scan(&tm.ID, &tm.Owner, &tm.Target, &tm.FireAt, &tm.EveryMs, &payload, &tm.Active); err != nil {
			return nil, internal(err)
		}
		var tp timerPayload
		if err := json.Unmarshal([]byte(payload), &tp); err != nil {
			return nil, internal(err)
		}
		tm.Body = tp.Body
		out = append(out, tm)
	}
	return out, internal(rows.Err())
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
