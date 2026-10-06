package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Admin quick actions on one participant. Admin only: agents get no new power.

// WithAbortPush registers f, which pushes abort to a live non-headless session's connections
// and returns how many it reached (0 = the session is not connected). f must not block.
func WithAbortPush(f func(participantID string) int) Option {
	return func(e *Engine) { e.abortPush = f }
}

// AdminTarget names a participant by name or id; Team when the name is in several teams.
type AdminTarget struct {
	Target string `json:"target"`
	Team   string `json:"team,omitempty"`
}

type AbortResult struct {
	ParticipantID string `json:"participant_id"`
	How           string `json:"how"` // "rpc": the driver told the worker; "push": the session's harness was told
}

// adminTarget resolves a live participant for an admin action; mode says whether it is headless.
func (e *Engine) adminTarget(ctx context.Context, a AdminTarget) (participant, bool, error) {
	p, headless, _, err := e.adminTargetCaps(ctx, a)
	return p, headless, err
}

// adminTargetCaps is adminTarget with the target's capabilities as stored.
func (e *Engine) adminTargetCaps(ctx context.Context, a AdminTarget) (participant, bool, sql.NullString, error) {
	var p participant
	var headless bool
	var caps sql.NullString
	err := e.readOnly(ctx, func(t *txn) error {
		var err error
		if p, err = t.participantForAdmin(a.Target, a.Team); err != nil {
			return err
		}
		var mode string
		if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(mode,''), capabilities FROM participants WHERE id=?`,
			p.id).Scan(&mode, &caps); err != nil {
			return internal(err)
		}
		headless = mode == modeHeadless
		return nil
	})
	return p, headless, caps, err
}

// unsupported refuses a command p's harness does not declare.
func unsupported(p participant, what string) error {
	return errf(CodeUnsupported, "%s: %s is not supported by this harness", p.name, what)
}

// Abort cancels the participant's current turn; it stays alive with its context. The batch
// that turn was on is not acked (no completion), so its mail is delivered again. A headless
// worker is told by the driver; another session through the abort push to its harness.
func (e *Engine) Abort(ctx context.Context, a AdminTarget) (AbortResult, error) {
	p, headless, caps, err := e.adminTargetCaps(ctx, a)
	if err != nil {
		return AbortResult{}, err
	}
	if p.state == "gone" {
		return AbortResult{}, errf(CodeInvalid, "%s is not running", p.name)
	}
	if lacksCap(caps, CapAbort) {
		return AbortResult{}, unsupported(p, "abort")
	}
	res := AbortResult{ParticipantID: p.id, How: "push"}
	switch {
	case headless:
		d := e.runtimeFor(p.harness)
		if d == nil {
			return AbortResult{}, errf(CodeUnsupported, "no runtime driver for harness %q", p.harness)
		}
		if delivers(d) { // before the driver's cancellations arrive: they must not redeliver
			if err := e.inTx(ctx, func(t *txn) error { return t.endDeliveries(p.id) }); err != nil {
				return AbortResult{}, err
			}
		}
		if err := d.Abort(p.id); errors.Is(err, ErrNotRunning) {
			return AbortResult{}, errf(CodeInvalid, "%s is not running", p.name)
		} else if err != nil {
			return AbortResult{}, internal(err)
		}
		res.How = "rpc"
	case e.abortPush == nil || e.abortPush(p.id) == 0:
		return AbortResult{}, errf(CodeInvalid, "%s is not connected", p.name)
	}
	// The aborted turn may send no end (a Claude interrupt runs no Stop): close it here, or it
	// would hold back every later wake.
	return res, e.inTx(ctx, func(t *txn) error {
		if err := t.closeOpenTurns(p.run); err != nil {
			return err
		}
		return t.event(evt{typ: "aborted", participant: p.id, team: p.team, run: p.run, payload: map[string]any{"how": res.How}})
	})
}

// Kill ends a headless worker's process group at once (SIGKILL, no grace) and records the
// exit: a stopped event (cause killed, how kill) and gone. A worker the driver does not hold
// (parked) is verified first, as stop does. Sessions a human opened are never killed.
func (e *Engine) Kill(ctx context.Context, a AdminTarget) (AgentResult, error) {
	w, headless, err := e.adminTarget(ctx, a)
	if err != nil {
		return AgentResult{}, err
	}
	if !headless {
		return AgentResult{}, errf(CodeInvalid, "%s is not a headless worker; a session can only be aborted", w.name)
	}
	if w.state == "gone" {
		return AgentResult{}, errf(CodeInvalid, "%s is not running", w.name)
	}
	if e.runtimeFor(w.harness) == nil {
		return AgentResult{}, errf(CodeUnsupported, "no runtime driver for harness %q", w.harness)
	}
	exit, err := e.stopWorker(ctx, w, "killed", true)
	if err != nil {
		return AgentResult{}, err
	}
	return AgentResult{ParticipantID: w.id, RunID: w.run, Exit: &exit}, nil
}

// Resume runs a stopped headless worker again, as its spawner's agent resume does (same session,
// stored model, thinking and harness; limits apply), without the owner check: the admin may
// resume any worker. No task: its pending mail is what it reads first. The resumed event says
// by admin.
func (e *Engine) Resume(ctx context.Context, a AdminTarget) (AgentResult, error) {
	return e.resumeWorker(ctx, func(t *txn) (participant, participant, error) {
		w, err := t.participantForAdmin(a.Target, a.Team)
		if err != nil {
			return participant{}, w, err
		}
		var mode string
		if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(mode,'') FROM participants WHERE id=?`, w.id).Scan(&mode); err != nil {
			return participant{}, w, internal(err)
		}
		if mode != modeHeadless {
			return participant{}, w, errf(CodeInvalid, "%s is not a headless worker; a session is started by its Human", w.name)
		}
		var closed sql.NullInt64
		if err := t.QueryRowContext(t.ctx, `SELECT closed_at FROM teams WHERE id=?`, w.team).Scan(&closed); err != nil {
			return participant{}, w, internal(err)
		}
		if closed.Valid {
			return participant{}, w, errf(CodeInvalid, "%s's team is closed", w.name)
		}
		return participant{team: w.team}, w, nil // the admin: no participant, the worker's team
	}, "", false)
}

type ModelArgs struct {
	AdminTarget
	Model    string `json:"model,omitempty"`    // as the worker's harness names it (pi: provider/model); "" keeps it
	Thinking string `json:"thinking,omitempty"` // as the harness names levels; "" keeps it
}

type ModelsResult struct {
	Models []string `json:"models"` // as SetModel takes them
}

// Models lists the models a live headless worker's harness offers, as SetModel takes them, for a
// picker. The list is the driver's and core never reads it. Refused: a session, a worker that is not
// running (a closed team's workers are gone), a harness that cannot list.
func (e *Engine) Models(ctx context.Context, a AdminTarget) (ModelsResult, error) {
	w, headless, caps, err := e.adminTargetCaps(ctx, a)
	if err != nil {
		return ModelsResult{}, err
	}
	if !headless {
		return ModelsResult{}, errf(CodeInvalid, "%s is not a headless worker; its model is chosen in its own session", w.name)
	}
	if w.state == "gone" {
		return ModelsResult{}, errf(CodeInvalid, "%s is not running", w.name)
	}
	d := e.runtimeFor(w.harness)
	if d == nil {
		return ModelsResult{}, errf(CodeUnsupported, "no runtime driver for harness %q", w.harness)
	}
	if lacksCap(caps, CapListModels) {
		return ModelsResult{}, errf(CodeInvalid, "%s: this harness cannot list its models", w.name)
	}
	models, err := d.Models(ctx, w.id)
	if errors.Is(err, ErrNotRunning) {
		return ModelsResult{}, errf(CodeInvalid, "%s is not running", w.name)
	}
	if err != nil {
		return ModelsResult{}, errf(CodeInvalid, "%s: %v", w.name, err)
	}
	if models == nil {
		models = []string{}
	}
	return ModelsResult{Models: models}, nil
}

type ModelResult struct {
	ParticipantID string `json:"participant_id"`
	Live          bool   `json:"live"` // the running worker switched now; else it applies at resume
}

// SetModel sets a headless worker's model, thinking level, or both. A running worker switches
// through its driver first (model, then thinking; from its next turn, context kept); if the
// harness refuses either, nothing is stored. The stored values are what resume uses. Event
// model_set. Agents have no way to do this.
func (e *Engine) SetModel(ctx context.Context, a ModelArgs) (ModelResult, error) {
	if a.Model == "" && a.Thinking == "" {
		return ModelResult{}, errf(CodeInvalid, "a model or a thinking level is required")
	}
	w, headless, caps, err := e.adminTargetCaps(ctx, a.AdminTarget)
	if err != nil {
		return ModelResult{}, err
	}
	if lacksCap(caps, CapSetModel) {
		return ModelResult{}, unsupported(w, "changing the model")
	}
	if !headless {
		return ModelResult{}, errf(CodeInvalid, "%s is not a headless worker; its model is chosen in its own session", w.name)
	}
	res := ModelResult{ParticipantID: w.id}
	if d := e.runtimeFor(w.harness); w.state != "gone" && d != nil {
		live, err := e.setLive(ctx, d, w.id, a)
		switch {
		case err == nil:
			res.Live = true
		case !errors.Is(err, ErrNotRunning) || live:
			return ModelResult{}, errf(CodeInvalid, "%s: nothing stored: %v", w.name, err)
		}
	}
	return res, e.inTx(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, `UPDATE participants SET model=COALESCE(NULLIF(?,''),model),
			thinking=COALESCE(NULLIF(?,''),thinking) WHERE id=?`, a.Model, a.Thinking, w.id); err != nil {
			return internal(err)
		}
		// The harness switched: that is what the session runs now, whatever it last reported (pi
		// reports a model or level it was told over rpc to no extension).
		if res.Live {
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET session_model=COALESCE(NULLIF(?,''),session_model),
				session_thinking=COALESCE(NULLIF(?,''),session_thinking) WHERE id=?`, a.Model, a.Thinking, w.id); err != nil {
				return internal(err)
			}
		}
		payload := map[string]any{"live": res.Live}
		if a.Model != "" {
			payload["model"] = a.Model
		}
		if a.Thinking != "" {
			payload["thinking"] = a.Thinking
		}
		return t.event(evt{typ: "model_set", participant: w.id, team: w.team, run: w.run, payload: payload})
	})
}

// setLive applies a to the running worker: the model, then the thinking level. modelSet reports
// that the model already switched when the thinking level then failed (the error says so).
func (e *Engine) setLive(ctx context.Context, d RuntimeDriver, id string, a ModelArgs) (modelSet bool, err error) {
	if a.Model != "" {
		if err := d.SetModel(ctx, id, a.Model); err != nil {
			return false, fmt.Errorf("kept its model: %w", err)
		}
	}
	if a.Thinking != "" {
		if err := d.SetThinking(ctx, id, a.Thinking); err != nil {
			if a.Model != "" {
				return true, fmt.Errorf("the running worker now has model %s but %w", a.Model, err)
			}
			return false, err
		}
	}
	return false, nil
}
