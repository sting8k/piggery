package core

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Agent is the `agent` tool: spawn, stop, resume, tail headless workers, found a team, and
// admit a solo. Core decides (gate, rows, mail, events); the RuntimeDriver owns processes and
// is only called after commit.
func (e *Engine) Agent(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	switch a.Action { // no process: these need no runtime
	case AgentFound:
		return e.found(ctx, c, a)
	case AgentAdmit:
		return e.admit(ctx, c, a)
	case AgentTemplates:
		return e.listTemplates(ctx, c)
	case AgentClose:
		return e.closeTeam(ctx, c, a)
	case AgentReopen:
		return e.reopen(ctx, c, a)
	}
	if len(e.runtimes) == 0 {
		return AgentResult{}, errf(CodeUnsupported, "no runtime driver configured")
	}
	switch a.Action {
	case AgentSpawn:
		return e.spawn(ctx, c, a)
	case AgentResume:
		return e.resume(ctx, c, a)
	case AgentStop:
		return e.stop(ctx, c, a)
	case AgentTail:
		return e.tail(ctx, c, a)
	}
	return AgentResult{}, errf(CodeInvalid, "unknown agent action %q", a.Action)
}

func (e *Engine) spawn(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	if a.Template != "" { // spawn template=: a taskforce (taskforce.go); an older daemon has no role here and refuses
		return e.spawnTaskforce(ctx, c, a)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" || isReserved(name) || a.Role == "" || a.Task == "" {
		return AgentResult{}, errf(CodeInvalid, "spawn needs role, a valid name, and task")
	}
	token, err := newToken()
	if err != nil {
		return AgentResult{}, internal(err)
	}
	// The worker's cwd (a.Cwd, or the spawner's) is resolved and its bounds checked before the
	// tx: the bounds may run git. An inherited cwd is bounded too: the spawner may sit outside
	// the root (an admin join).
	var from, root string
	if err := e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "agent.spawn", "agent")
		if err != nil {
			return err
		}
		return t.cwdAndRoot(p.id, &from, &root)
	}); err != nil {
		return AgentResult{}, err
	}
	dir := from
	if a.Cwd != "" {
		if dir, err = spawnDir(from, a.Cwd); err != nil {
			return AgentResult{}, err
		}
	}
	bounded := e.inBounds(ctx, cmp.Or(root, from), dir)
	var res AgentResult
	var cwd, ref, harness, model, thinking string
	err = e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "agent.spawn", "agent")
		if err != nil {
			return err
		}
		m, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		if _, ok := m.Roles[a.Role]; !ok {
			return errf(CodeInvalid, "role %q not in team manifest", a.Role)
		}
		if harness, model, thinking, err = e.workerSettings(t, m, a.Role, p.id, ""); err != nil {
			return err
		}
		if !contains(m.Roles[p.role].CanSpawn, a.Role) {
			return deny(&p, "agent.spawn", "permission", "can_spawn", "role "+p.role+" cannot spawn "+a.Role, nil,
				map[string]any{"role": a.Role})
		}
		depth, err := t.depth(p.id)
		if err != nil {
			return err
		}
		if limit, ok := m.Limits["depth"]; ok && depth+1 > limit {
			return deny(&p, "agent.spawn", "limit", "limits.depth", fmt.Sprintf("depth %d exceeds %d", depth+1, limit), nil,
				map[string]any{"depth": depth + 1})
		}
		if err := t.gateConcurrency(p, m, "agent.spawn", ""); err != nil {
			return err
		}
		if rule, ok := m.route(p.role, a.Role); !ok {
			return deny(&p, "agent.spawn", "routing", rule, "routing does not allow "+p.role+" -> "+a.Role, nil,
				map[string]any{"role": a.Role})
		}
		var taken int
		if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM participants WHERE team_id=? AND name=?`,
			p.team, name).Scan(&taken); err != nil {
			return internal(err)
		}
		if taken > 0 {
			return errf(CodeInvalid, "name %q already taken in team", name)
		}
		if err := t.QueryRowContext(t.ctx, `SELECT cwd FROM participants WHERE id=?`, p.id).Scan(&cwd); err != nil {
			return internal(err)
		}
		if dir != cwd && !m.Roles[p.role].CanSetCwd {
			return deny(&p, "agent.spawn", "permission", "can_set_cwd", "role "+p.role+" cannot choose a worker's cwd; omit cwd to use yours",
				nil, map[string]any{"cwd": dir})
		}
		if !bounded {
			return deny(&p, "agent.spawn", "bounds", "cwd.bounds", outOfBounds(dir), nil, map[string]any{"cwd": dir})
		}
		cwd = dir
		if ref, err = newUUID(); err != nil {
			return internal(err)
		}
		res = AgentResult{ParticipantID: newID(t.now), RunID: newID(t.now), TaskID: newID(t.now)}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO participants
			(id, run_id, kind, mode, name, cwd, team_id, role, reports_to, spawned_by, state, state_since,
			 last_activity, token_hash, harness_ref, created_at, model, thinking, harness, capabilities)
			VALUES (?,?,'agent','headless',?,?,?,?,?,?,'requested',?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),?,?)`,
			res.ParticipantID, res.RunID, name, cwd, p.team, a.Role, p.id, p.id, t.now, t.now,
			hashToken(token), ref, t.now, model, thinking, harness, e.driverCaps(harness)); err != nil {
			return internal(err)
		}
		if err := t.event(evt{typ: "spawned", participant: p.id, team: p.team, run: p.run, ref: res.ParticipantID,
			payload: map[string]any{"worker": res.ParticipantID, "run_id": res.RunID, "name": name, "role": a.Role,
				"depth": depth + 1, "task_id": res.TaskID, "cwd": cwd}}); err != nil {
			return err
		}
		seq, err := t.insertMessage(res.TaskID, "", p.team, p.id, res.ParticipantID, "", "", OpAssign, "", a.Task)
		res.TaskSeq = seq
		return err
	})
	if err != nil {
		return AgentResult{}, err
	}
	e.notifyAfterCommit(res.ParticipantID)
	return res, e.start(ctx, Spec{ParticipantID: res.ParticipantID, RunID: res.RunID, Token: token, Cwd: cwd,
		HarnessRef: ref, Harness: harness, Model: model, Thinking: thinking}, res)
}

func (e *Engine) resume(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	return e.resumeWorker(ctx, func(t *txn) (participant, participant, error) {
		return t.ownedWorker(c, a.Target, "agent.resume")
	}, a.Task, false)
}

// resumeWorker runs a stopped worker again in its session (gone, or parked once its old process
// is shown dead). find returns who resumes (its team is the worker's; id "" = the admin) and the
// worker. task, when set, is a message from who resumes, as at spawn. byMail: the daemon resumes a
// gone member because mail came for it (wake.go): the member goes on as a headless worker, and the
// event says so.
func (e *Engine) resumeWorker(ctx context.Context, find func(t *txn) (participant, participant, error), task string, byMail bool) (AgentResult, error) {
	token, err := newToken()
	if err != nil {
		return AgentResult{}, internal(err)
	}
	// A parked worker (reconcile could not verify its process) resumes only once the driver
	// shows its old process is dead or the pid reused; the check runs outside any tx.
	var before participant
	var cwd, root string // the worker's stored cwd and its team's root, to bound it outside any tx
	if err := e.inTx(ctx, func(t *txn) error {
		_, w, err := find(t)
		before = w
		if err != nil {
			return err
		}
		return t.cwdAndRoot(w.id, &cwd, &root)
	}); err != nil {
		return AgentResult{}, err
	}
	bounded := e.inBounds(ctx, cmp.Or(root, cwd), cwd)
	if before.state == "parked" && !e.oldProcessEnded(ctx, before) {
		return AgentResult{}, e.inTx(ctx, func(t *txn) error {
			p, _, err := find(t)
			if err != nil {
				return err
			}
			return deny(&p, "agent.resume", "target", "worker.unverified",
				before.name+"'s previous process may still be running", nil, map[string]any{"target": before.id})
		})
	}
	var res AgentResult
	var spec Spec
	var limited *Error  // the respawn limit parked the worker (committed, then returned)
	var notified string // reports_to to wake: it has the notice
	err = e.inTx(ctx, func(t *txn) error {
		p, w, err := find(t)
		if err != nil {
			return err
		}
		verifiedParked := w.state == "parked" && before.state == "parked" && w.run == before.run
		if w.state != "gone" && !verifiedParked {
			return errf(CodeInvalid, "%s is running (%s); only a stopped worker (gone, or parked) is resumed", w.name, w.state)
		}
		if verifiedParked {
			if _, err := t.markExited(w.id, w.run, Exit{Code: -1, Signal: "lost"}); err != nil {
				return err
			}
		}
		m, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		if limited, notified, err = t.gateRespawn(p, w, m); err != nil || limited != nil {
			return err
		}
		if err := t.gateConcurrency(p, m, "agent.resume", w.id); err != nil { // a parked w already counts
			return err
		}
		var cwd, ref, model, thinking, spawnedBy, mode, sessionRef string
		if err := t.QueryRowContext(t.ctx, `SELECT cwd, COALESCE(harness_ref,''), COALESCE(model,''), COALESCE(thinking,''),
			COALESCE(spawned_by,''), COALESCE(mode,''), COALESCE(session_ref,'') FROM participants WHERE id=?`, w.id).Scan(
			&cwd, &ref, &model, &thinking, &spawnedBy, &mode, &sessionRef); err != nil {
			return internal(err)
		}
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() { // never another directory in its place
			return errf(CodeInvalid, "%s's directory %s no longer exists; restore it to resume %s there, or spawn a new worker", w.name, cwd, w.name)
		}
		if !bounded {
			return deny(&p, "agent.resume", "bounds", "cwd.bounds", outOfBounds(cwd), nil, map[string]any{"cwd": cwd, "target": w.id})
		}
		// What was stored at spawn; what is missing (the chain found nothing then): the chain
		// again, from the worker's own ancestors. The choice is kept for the next resume.
		harness, m2, t2, err := e.workerSettings(t, m, w.role, spawnedBy, w.harness)
		if err != nil {
			return err
		}
		model, thinking = cmp.Or(model, m2), cmp.Or(thinking, t2)
		run := newID(t.now)
		if _, err := t.ExecContext(t.ctx, `UPDATE participants SET run_id=?, token_hash=?, last_turn_end=NULL,
			model=NULLIF(?,''), thinking=NULLIF(?,''), harness=?, capabilities=? WHERE id=?`, run, hashToken(token), model, thinking,
			harness, e.driverCaps(harness), w.id); err != nil {
			return internal(err)
		}
		if byMail {
			if ref, err = t.becomeWorker(w.id, mode, ref, sessionRef); err != nil {
				return err
			}
		}
		w.run = run
		res = AgentResult{ParticipantID: w.id, RunID: run}
		payload := map[string]any{"worker": w.id, "run_id": run}
		switch {
		case byMail:
			payload["by"] = "mail"
		case p.id == "":
			payload["by"] = "admin"
		}
		if task != "" {
			res.TaskID = newID(t.now)
			payload["task_id"] = res.TaskID
		}
		if err := t.event(evt{typ: "resumed", participant: p.id, team: p.team, run: p.run, ref: w.id,
			payload: payload}); err != nil {
			return err
		}
		if err := t.setState(&w, "requested", "resume"); err != nil {
			return err
		}
		if task != "" { // as at spawn: a message from the caller to the worker
			if res.TaskSeq, err = t.insertMessage(res.TaskID, "", p.team, p.id, w.id, "", "", OpAssign, "", task); err != nil {
				return err
			}
		}
		spec = Spec{ParticipantID: w.id, RunID: run, Token: token, Cwd: cwd, HarnessRef: ref, Harness: harness, Model: model,
			Thinking: thinking, Resume: true}
		return nil
	})
	if err != nil {
		return AgentResult{}, err
	}
	if limited != nil {
		if notified != "" {
			e.notifyAfterCommit(notified)
		}
		return AgentResult{}, limited
	}
	if res.TaskID != "" {
		e.notifyAfterCommit(res.ParticipantID)
	}
	return res, e.start(ctx, spec, res)
}

// gateRespawn applies limits.max_respawn_per_hour to resuming w: with N resumes of w in the last 60
// minutes already, w is not started. The first time, w goes parked with a respawn_limit event and
// one notice to its reports_to (not to notify: that is the engine's gate events only); the error is
// returned after that commits. While w stays parked a later resume is simply denied. notified is the
// reports_to that got the notice ("" = none).
func (t *txn) gateRespawn(p, w participant, m manifest) (*Error, string, error) {
	limit, ok := m.Limits["max_respawn_per_hour"]
	if !ok {
		return nil, "", nil
	}
	var n int
	if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM events WHERE type='resumed' AND ref_id=? AND ts>?`,
		w.id, t.now-3_600_000).Scan(&n); err != nil {
		return nil, "", internal(err)
	}
	if n < limit {
		return nil, "", nil
	}
	reason := fmt.Sprintf("%s resumed %d times in the last hour; parked (%s)", w.name, n, RuleRespawn)
	if w.state == "parked" {
		return nil, "", deny(&p, "agent.resume", "limit", RuleRespawn, reason, nil,
			map[string]any{"target": w.id, "resumes": n})
	}
	if err := t.setState(&w, "parked", "respawn_limit"); err != nil {
		return nil, "", err
	}
	if err := t.event(evt{typ: "respawn_limit", participant: w.id, team: w.team, run: w.run, ref: w.id,
		payload: map[string]any{"resumes": n, "limit": limit, "by": p.id}}); err != nil {
		return nil, "", err
	}
	if w.reportsTo != "" {
		if _, err := t.insertMessage(newID(t.now), "", w.team, AddrEngine, w.reportsTo, "", "", "", "", reason+"."); err != nil {
			return nil, "", err
		}
	}
	return &Error{Code: CodeDenied, Message: reason, RuleID: RuleRespawn, Layer: "limit"}, w.reportsTo, nil
}

// RuleRespawn is the respawn limit's rule id.
const RuleRespawn = "limits.max_respawn_per_hour"

// oldProcessEnded reports whether the driver shows w's recorded process is dead or reused.
// Any doubt (no driver, check failed, still ours) is false.
func (e *Engine) oldProcessEnded(ctx context.Context, w participant) bool {
	proc, exited, err := e.processOf(ctx, w.id, w.run)
	switch {
	case err != nil:
		return false
	case proc == nil || exited:
		return true
	}
	d := e.runtimeFor(w.harness)
	if d == nil {
		return false
	}
	st, err := d.Inspect(ctx, *proc)
	return err == nil && (st == ProcDead || st == ProcReused)
}

// start runs the driver after the spawn/resume transaction committed, then records the outcome:
// the process row and `starting`, or `gone` with the error (the worker's mail stays pending).
func (e *Engine) start(ctx context.Context, s Spec, res AgentResult) error {
	drv := e.runtimeFor(s.Harness) // workerSettings checked that it exists
	if err := e.inTx(ctx, func(t *txn) error {
		w, ok, err := t.participantByID(s.ParticipantID)
		if err != nil || !ok {
			return err
		}
		m, err := t.teamManifest(w.team)
		if err != nil {
			return err
		}
		w.toolPrefix = drv.ToolPrefix()
		s.AllowTools = m.Roles[w.role].Spawn.AllowTools
		s.RoleCard, err = t.roleCard(w, m)
		return err
	}); err != nil {
		return err
	}
	proc, startErr := drv.Start(ctx, s)
	closed := false // the team went down while the driver was starting the worker
	err := e.inTx(ctx, func(t *txn) error {
		w, ok, err := t.participantByID(s.ParticipantID)
		if err != nil || !ok || w.run != s.RunID {
			return err // superseded meanwhile: nothing to record for this run
		}
		var closedAt sql.NullInt64
		if err := t.QueryRowContext(t.ctx, `SELECT closed_at FROM teams WHERE id=?`, w.team).Scan(&closedAt); err != nil {
			return internal(err)
		}
		closed = closedAt.Valid
		if startErr != nil {
			if err := t.event(evt{typ: "spawn_failed", participant: w.id, team: w.team, run: w.run,
				payload: map[string]any{"error": startErr.Error()}}); err != nil {
				return err
			}
			return t.setState(&w, "gone", "start_failed")
		}
		cmd, err := json.Marshal(proc.Cmdline)
		if err != nil {
			return internal(err)
		}
		// gone already: the process died before this row existed (ProcessExited ran first).
		// Not when the team closed: then team down made it gone without seeing this process,
		// which is alive and is stopped below.
		var exitedAt any
		if w.state == "gone" && !closed {
			exitedAt = t.now
		}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO processes
			(participant_id, run_id, pid, pgid, start_time, cmdline, started_at, exited_at) VALUES (?,?,?,?,?,?,?,?)`,
			w.id, w.run, proc.PID, proc.PGID, proc.StartTime, string(cmd), t.now, exitedAt); err != nil {
			return internal(err)
		}
		if proc.HarnessRef != "" { // the harness named the session itself (Codex): resume it by that
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET harness_ref=? WHERE id=?`, proc.HarnessRef, w.id); err != nil {
				return internal(err)
			}
		}
		if w.state == "requested" && !closed { // identify may already have moved it on
			return t.setState(&w, "starting", "started")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if closed && startErr == nil {
		// A closed team has no live worker: stop the process just started.
		if _, err := e.stopWorker(ctx, participant{id: s.ParticipantID, run: s.RunID}, "team_closed", false); err != nil {
			return err
		}
		return errf(CodeInvalid, "the team closed while the worker was starting; it was stopped")
	}
	if startErr != nil {
		return &Error{Code: CodeStartFailed, Message: "start failed: " + startErr.Error(), Details: res}
	}
	if delivers(drv) { // its task and any mail waiting: no adapter pulls it
		e.notifyAfterCommit(s.ParticipantID)
	}
	return nil
}

func (e *Engine) stop(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	var w participant
	err := e.inTx(ctx, func(t *txn) error {
		var err error
		if _, w, err = t.ownedWorker(c, a.Target, "agent.stop"); err != nil {
			return err
		}
		if w.state == "gone" {
			return errf(CodeInvalid, "%s is not running", w.name)
		}
		return nil
	})
	if err != nil {
		return AgentResult{}, err
	}
	exit, err := e.stopWorker(ctx, w, "stopped", false)
	if err != nil {
		return AgentResult{}, err
	}
	return AgentResult{ParticipantID: w.id, RunID: w.run, Exit: &exit}, nil
}

// stopWorker ends w's current process and records it: the exit, a `stopped` event with cause
// and how, and gone (unless w was resumed meanwhile). When the driver does not hold w (parked
// after a restart or by the respawn limit, or a Start not registered yet) it is verified the
// way reconcile does: dead or pid reused -> gone; still ours -> KillVerified -> gone; any doubt
// -> an error, and w stays as it is. agent stop, team down, a start into a closed team and
// the admin kill (now: SIGKILL at once instead of the stop escalation) all stop workers here.
func (e *Engine) stopWorker(ctx context.Context, w participant, cause string, now bool) (Exit, error) {
	var exit *Exit
	how := "stop"
	if d := e.runtimeFor(w.harness); w.state != "parked" && d != nil {
		stop := d.Stop
		if now {
			stop, how = d.Kill, "kill"
		}
		if x, err := stop(ctx, w.id); err == nil {
			exit = &x
		}
	}
	if exit == nil {
		d := e.inspectWorker(ctx, w.id, w.run, w.harness)
		if d.state != "gone" {
			return Exit{}, errf(CodeInvalid, "%s could not be stopped: %s", w.name, d.reason)
		}
		exit, how = d.exit, d.reason
	}
	err := e.inTx(ctx, func(t *txn) error {
		if exit != nil {
			if _, err := t.markExited(w.id, w.run, *exit); err != nil {
				return err
			}
		}
		q, ok, err := t.participantByID(w.id)
		if err != nil || !ok {
			return err
		}
		payload := map[string]any{"cause": cause, "how": how}
		if exit != nil {
			payload["code"], payload["signal"] = exit.Code, exit.Signal
		}
		if err := t.event(evt{typ: "stopped", participant: q.id, team: q.team, run: w.run, payload: payload}); err != nil {
			return err
		}
		if q.run != w.run { // resumed meanwhile: the new run's state is not ours
			return nil
		}
		return t.setState(&q, "gone", cause)
	})
	if exit == nil {
		return Exit{}, err
	}
	return *exit, err
}

// ProcessExited is the driver's exit hook for (participant, run). It records the exit once;
// if that run is still current and not gone, the participant goes gone with an `exited` event.
// A later call for the same run (e.g. after Stop already recorded it) changes nothing.
// The hook may run before start() wrote the process row (the process died during startup):
// then the run, if current and not gone, still goes gone, and start() records the row as exited.
// So no order of (Start returns, exit hook) leaves a dead worker live.
func (e *Engine) ProcessExited(ctx context.Context, participantID, runID string, exit Exit) error {
	return e.inTx(ctx, func(t *txn) error {
		first, err := t.markExited(participantID, runID, exit)
		if err != nil {
			return err
		}
		p, ok, err := t.participantByID(participantID)
		if err != nil || !ok {
			return err
		}
		if !first {
			var rows int
			if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM processes WHERE participant_id=? AND run_id=?`,
				participantID, runID).Scan(&rows); err != nil {
				return internal(err)
			}
			if rows > 0 || p.run != runID || p.state == "gone" {
				return nil // already recorded, superseded, or already gone
			}
		}
		current := p.run == runID
		if err := t.event(evt{typ: "exited", participant: p.id, team: p.team, run: runID,
			payload: map[string]any{"code": exit.Code, "signal": exit.Signal, "current_run": current}}); err != nil {
			return err
		}
		if !current {
			return nil // resumed meanwhile: the new run's state is not ours
		}
		return t.setState(&p, "gone", "exited")
	})
}

// markExited records the exit of (participant, run) unless already recorded; it reports
// whether this call recorded it.
func (t *txn) markExited(participantID, runID string, exit Exit) (bool, error) {
	at := exit.At
	if at == 0 {
		at = t.now
	}
	r, err := t.ExecContext(t.ctx, `UPDATE processes SET exited_at=?, exit_code=?
		WHERE participant_id=? AND run_id=? AND exited_at IS NULL`, at, exit.Code, participantID, runID)
	if err != nil {
		return false, internal(err)
	}
	n, err := r.RowsAffected()
	return n == 1, internal(err)
}

func (e *Engine) tail(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	var w participant
	err := e.inTx(ctx, func(t *txn) error {
		_, q, err := t.ownedWorker(c, a.Target, "agent.tail")
		if err != nil {
			return err
		}
		w = q
		return nil
	})
	if err != nil {
		return AgentResult{}, err
	}
	lines := a.Lines
	if lines <= 0 {
		lines = 50
	}
	d := e.runtimeFor(w.harness)
	if d == nil {
		return AgentResult{}, errf(CodeUnsupported, "no runtime driver for harness %q", w.harness)
	}
	recs, err := d.Tail(w.id, lines)
	if err != nil {
		return AgentResult{}, internal(err)
	}
	return AgentResult{ParticipantID: w.id, RunID: w.run, Records: recs}, nil
}

// ownedWorker resolves target in the caller's view and requires that the caller spawned it or
// that it reports to the caller (gate layer 5).
func (t *txn) ownedWorker(c Caller, target, verb string) (participant, participant, error) {
	p, err := t.callerGranted(c, verb, "agent")
	if err != nil {
		return p, participant{}, err
	}
	w, err := t.resolveInView(p, target, verb)
	if err != nil {
		return p, w, err
	}
	var spawnedBy string
	if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(spawned_by,'') FROM participants WHERE id=?`, w.id).Scan(&spawnedBy); err != nil {
		return p, w, internal(err)
	}
	if spawnedBy != p.id && w.reportsTo != p.id {
		return p, w, deny(&p, verb, "target", "agent.not_owner", w.name+" was not spawned by and does not report to "+p.name,
			nil, map[string]any{"target": w.id})
	}
	return p, w, nil
}

// gateConcurrency refuses a new live headless worker when the team is at limits.concurrency.
// except (the worker being resumed) is not counted.
func (t *txn) gateConcurrency(p participant, m manifest, verb, except string) error {
	limit, ok := m.Limits["concurrency"]
	if !ok {
		return nil
	}
	var live int
	if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM participants
		WHERE team_id=? AND mode='headless' AND state<>'gone' AND id<>?`, p.team, except).Scan(&live); err != nil {
		return internal(err)
	}
	if live >= limit {
		return deny(&p, verb, "limit", "limits.concurrency", fmt.Sprintf("%d live workers, limit %d", live, limit), nil,
			map[string]any{"live": live})
	}
	return nil
}

// depth is the length of id's spawned_by chain (a joined participant is 0). A taskforce's chair
// (spawned by the caller of its team, outside it) counts as depth 1 inside its own team: the
// taskforce's limits.depth is its own.
func (t *txn) depth(id string) (int, error) {
	d := 0
	for seen := map[string]bool{}; !seen[id]; d++ {
		seen[id] = true
		var parent sql.NullString
		var chair bool
		err := t.QueryRowContext(t.ctx, `SELECT p.spawned_by, COALESCE(p.spawned_by = (SELECT parent_id FROM teams WHERE id=p.team_id), 0)
			FROM participants p WHERE p.id=?`, id).Scan(&parent, &chair)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !parent.Valid) {
			return d, nil
		}
		if err != nil {
			return 0, internal(err)
		}
		if chair {
			return d + 1, nil
		}
		id = parent.String
	}
	return d, errf(CodeInternal, "spawned_by cycle at %s", id)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// newUUID returns a random (v4) UUID, the harness session id of a new worker.
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
