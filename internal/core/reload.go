package core

import (
	"context"
	"database/sql"
	"time"
)

// ReloadArgs is the admin's reload of one worker.
type ReloadArgs struct {
	AdminTarget
	Now bool `json:"now,omitempty"` // stop the turn in progress instead of waiting for its end
}

// Reload restarts a headless worker in its session (the admin's; agent action reload is the
// spawner's), so that it starts again with the Human's config as it is now: harness/<harness>.json
// and the prompts are read at every start, and the model and thinking level come again from the
// chain a spawn of its role follows (workerSettings), except a field set by hand (`piggery model`),
// which is kept. A config that now gives its role another harness is refused: a session cannot go
// on in another harness. A worker in a turn reloads when the turn ends (Pending), unless now; a
// stopped worker is resumed with the new settings. Events: stopped (cause reload), resumed.
func (e *Engine) Reload(ctx context.Context, a ReloadArgs) (AgentResult, error) {
	return e.reload(ctx, func(t *txn) (participant, participant, error) {
		w, err := t.participantForAdmin(a.Target, a.Team)
		if err != nil {
			return participant{}, w, err
		}
		if err := t.requireHeadless(w, "a session is restarted by its Human"); err != nil {
			return participant{}, w, err
		}
		var closed sql.NullInt64
		if err := t.QueryRowContext(t.ctx, `SELECT closed_at FROM teams WHERE id=?`, w.team).Scan(&closed); err != nil {
			return participant{}, w, internal(err)
		}
		if closed.Valid {
			return participant{}, w, errf(CodeInvalid, "%s's team is closed", w.name)
		}
		return participant{team: w.team}, w, nil // the admin: no participant, the worker's team
	}, a.Now)
}

// inTurn is a worker state with a turn running or about to: a reload waits for its end.
func inTurn(state string) bool {
	switch state {
	case "requested", "starting", "working", "awaiting_permission":
		return true
	}
	return false
}

// reloadPoll is how often a waiting reload looks whether the turn ended (a guess: one row read).
const reloadPoll = 500 * time.Millisecond

func (e *Engine) reload(ctx context.Context, find func(t *txn) (participant, participant, error), now bool) (AgentResult, error) {
	var w participant
	if err := e.inTx(ctx, func(t *txn) error {
		var err error
		if _, w, err = find(t); err != nil {
			return err
		}
		_, _, err = e.reloadSettings(t, w) // refused now, not when the turn ends
		return err
	}); err != nil {
		return AgentResult{}, err
	}
	if now || !inTurn(w.state) {
		return e.reloadNow(ctx, w, find)
	}
	res := AgentResult{ParticipantID: w.id, Pending: true, Text: w.name + " is in a turn: it reloads when the turn ends"}
	if _, waiting := e.reloads.LoadOrStore(w.id, struct{}{}); !waiting {
		go e.reloadAtTurnEnd(w, find)
	}
	return res, nil
}

// reloadAtTurnEnd waits until w's run leaves its turn, then reloads it. It gives up when the run
// changed or stopped meanwhile (someone stopped, resumed or reloaded it), or the daemon closes.
func (e *Engine) reloadAtTurnEnd(w participant, find func(t *txn) (participant, participant, error)) {
	defer e.reloads.Delete(w.id)
	ctx := context.Background()
	for !e.closing.Load() {
		time.Sleep(reloadPoll)
		var state, run string
		if err := e.db.QueryRowContext(ctx, `SELECT state, COALESCE(run_id,'') FROM participants WHERE id=?`, w.id).Scan(&state, &run); err != nil {
			return
		}
		if run != w.run || state == "gone" || state == "parked" {
			return
		}
		if !inTurn(state) {
			w.state = state
			e.reloadNow(ctx, w, find) // what it did is in its events; nobody waits for the answer
			return
		}
	}
}

// reloadNow stops w (unless it is stopped already), stores the settings the config gives now, and
// resumes it.
func (e *Engine) reloadNow(ctx context.Context, w participant, find func(t *txn) (participant, participant, error)) (AgentResult, error) {
	if w.state != "gone" && w.state != "parked" {
		if _, err := e.stopWorker(ctx, w, "reload", false); err != nil {
			return AgentResult{}, err
		}
	}
	if err := e.inTx(ctx, func(t *txn) error {
		_, w, err := find(t)
		if err != nil {
			return err
		}
		model, thinking, err := e.reloadSettings(t, w)
		if err != nil {
			return err
		}
		_, err = t.ExecContext(t.ctx, `UPDATE participants SET model=NULLIF(?,''), thinking=NULLIF(?,'') WHERE id=?`, model, thinking, w.id)
		return internal(err)
	}); err != nil {
		return AgentResult{}, err
	}
	res, err := e.resumeWorker(ctx, find, "", false)
	if err == nil {
		res.Text = "reloaded " + w.name
	}
	return res, err
}

// reloadSettings is the model and thinking level w starts with at a reload: what a spawn of its role
// gets from the config now, a field set by hand kept. A person's session keeps what it runs. A config
// that gives the role another harness than w's is an error.
func (e *Engine) reloadSettings(t *txn, w participant) (model, thinking string, err error) {
	var person, modelByHand, thinkingByHand bool
	var spawnedBy string
	if err := t.QueryRowContext(t.ctx, `SELECT person=1, COALESCE(spawned_by,''), COALESCE(model,''), COALESCE(thinking,''),
		model_by_hand, thinking_by_hand FROM participants WHERE id=?`, w.id).Scan(
		&person, &spawnedBy, &model, &thinking, &modelByHand, &thinkingByHand); err != nil {
		return "", "", internal(err)
	}
	if person {
		return model, thinking, nil
	}
	m, err := t.teamManifest(w.team)
	if err != nil {
		return "", "", err
	}
	h, newModel, newThinking, err := e.workerSettings(t, m, w.role, spawnedBy)
	if err != nil {
		return "", "", err
	}
	if w.harness != "" && h != w.harness {
		return "", "", errf(CodeInvalid, "%s runs %s and the config now gives it %s: a session cannot go on in another harness; stop %s and spawn a new worker",
			w.name, w.harness, h, w.name)
	}
	if !modelByHand {
		model = newModel
	}
	if !thinkingByHand {
		thinking = newThinking
	}
	return model, thinking, nil
}
