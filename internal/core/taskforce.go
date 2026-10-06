package core

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Taskforce: a solo or a team's gate calls a temporary team up from a
// template with `agent action=spawn template=<t> task=<text>`. The daemon makes the team (rooted at
// the caller's directory), spawns the template's auto_join_role as a headless worker that is the
// team's gate and reports to the caller, and gives it the task as its first mail. The caller and the
// taskforce talk over the gate-to-gate channel. teams.parent_id remembers the caller: the caller
// closes the taskforce, the chair closes it when done, and it is closed when the caller is gone or has
// left or its team closed, or its chair is gone (Taskforces, run by the daemon's tick), as team down. A taskforce never
// calls another one.

// taskforceSpec is the manifest's top-level `taskforce:` block. A template without it cannot be
// spawned as a taskforce (it can still be founded); every key of it lives here so a binary that does
// not know it runs the template as an ordinary one.
type taskforceSpec struct {
	// IdleFor: when every member of the taskforce has been idle that long, the caller gets one notice
	// per idle stretch. "" = no notice.
	IdleFor string `yaml:"idle_for"`
}

// idle is IdleFor as a duration (0: none). validateTaskforce has checked it parses.
func (s taskforceSpec) idle() time.Duration {
	d, _ := time.ParseDuration(s.IdleFor)
	return d
}

// validateTaskforce refuses a manifest whose taskforce block has a bad idle_for, or no role to be
// the chair (auto_join_role, else the only role). A key it does not know is not refused, so a later
// template that adds one still loads here (taskforceWarnings says it is ignored).
func validateTaskforce(m manifest) error {
	if m.Taskforce == nil {
		return nil
	}
	if m.Taskforce.IdleFor != "" {
		if d, err := time.ParseDuration(m.Taskforce.IdleFor); err != nil || d <= 0 {
			return errf(CodeInvalid, "manifest: taskforce.idle_for must be a positive duration like 20m, got %q", m.Taskforce.IdleFor)
		}
	}
	if m.sessionRole() == "" {
		return errf(CodeInvalid, "manifest: a taskforce needs auto_join_role (its chair) or a single role")
	}
	return nil
}

// taskforceParent is the caller of teamID when it is a taskforce ("" otherwise).
func (t *txn) taskforceParent(teamID string) (string, error) {
	if teamID == "" {
		return "", nil
	}
	var parent sql.NullString
	if err := t.QueryRowContext(t.ctx, `SELECT parent_id FROM teams WHERE id=?`, teamID).Scan(&parent); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", internal(err)
	}
	return parent.String, nil
}

// spawnTaskforce is `spawn template=`: see the file's header.
func (e *Engine) spawnTaskforce(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	if a.Role != "" || a.Task == "" || (a.Name != "" && isReserved(strings.TrimSpace(a.Name))) {
		return AgentResult{}, errf(CodeInvalid, "spawn template= needs task (and optionally name, the taskforce's team name, and cwd, where it works: your directory by default); role is for a worker (a taskforce has its own chair)")
	}
	token, err := newToken()
	if err != nil {
		return AgentResult{}, internal(err)
	}
	var from, root string
	if err := e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "agent.spawn", "agent")
		if err != nil {
			return err
		}
		if err := t.mayCallTaskforce(p); err != nil {
			return err
		}
		return t.cwdAndRoot(p.id, &from, &root)
	}); err != nil {
		return AgentResult{}, err
	}
	if e.templates == nil {
		return AgentResult{}, errf(CodeNotFound, "no template %q", a.Template)
	}
	text, err := e.templates(a.Template, from)
	if errors.Is(err, fs.ErrNotExist) {
		return AgentResult{}, errf(CodeNotFound, "no template %q in ~/.piggery/templates (agent action=templates lists them)", a.Template)
	}
	if err != nil {
		return AgentResult{}, errf(CodeInvalid, "template %s: %v", a.Template, err)
	}
	m, _, err := loadManifest(text)
	if err != nil {
		return AgentResult{}, err
	}
	// The taskforce works in a.Cwd (relative to the caller's directory or absolute) or the caller's own
	// directory, inside the same bounds as a worker's cwd, measured from the caller's root.
	dir := from
	if a.Cwd != "" {
		if dir, err = spawnDir(from, a.Cwd); err != nil {
			return AgentResult{}, err
		}
	}
	bounded := e.inBounds(ctx, cmp.Or(root, from), dir)
	var res AgentResult
	var ref, harness, model, thinking, team string
	err = e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "agent.spawn", "agent")
		if err != nil {
			return err
		}
		if err := t.mayCallTaskforce(p); err != nil {
			return err
		}
		if m.Taskforce == nil {
			return deny(&p, "agent.spawn", "permission", "taskforce.not_a_taskforce",
				fmt.Sprintf("template %s has no taskforce: block, so it cannot be called with spawn template= (found it instead)", a.Template), nil,
				map[string]any{"template": a.Template})
		}
		if !bounded {
			return deny(&p, "agent.spawn", "bounds", "cwd.bounds", outOfBounds(dir), nil, map[string]any{"cwd": dir})
		}
		// The chair is depth 1 inside its own team: depth is counted from the taskforce, not from the
		// caller's chain (depth() stops at a chair).
		if limit, ok := m.Limits["depth"]; ok && 1 > limit {
			return deny(&p, "agent.spawn", "limit", "limits.depth", fmt.Sprintf("the taskforce's own depth limit is %d", limit), nil, nil)
		}
		// One caller's open taskforces count against its team's concurrency limit.
		callerM, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		if limit, ok := callerM.Limits["concurrency"]; ok {
			var live int
			if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM teams WHERE parent_id=? AND closed_at IS NULL`, p.id).Scan(&live); err != nil {
				return internal(err)
			}
			if live >= limit {
				return deny(&p, "agent.spawn", "limit", "limits.concurrency", fmt.Sprintf("%d open taskforces, limit %d", live, limit), nil,
					map[string]any{"live": live})
			}
		}
		role := m.sessionRole()
		if harness, model, thinking, err = e.workerSettings(t, m, role, p.id, ""); err != nil {
			return err
		}
		teamName, err := t.freeTeamName(cmp.Or(strings.TrimSpace(a.Name), a.Template))
		if err != nil {
			return err
		}
		tm, err := t.insertTeam(teamName, text, m, dir)
		if err != nil {
			return err
		}
		team = tm.ID
		if _, err := t.ExecContext(t.ctx, `UPDATE teams SET parent_id=? WHERE id=?`, p.id, team); err != nil {
			return internal(err)
		}
		if ref, err = newUUID(); err != nil {
			return internal(err)
		}
		res = AgentResult{ParticipantID: newID(t.now), RunID: newID(t.now), TaskID: newID(t.now), TeamID: team, TeamName: teamName}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO participants
			(id, run_id, kind, mode, name, cwd, team_id, role, reports_to, spawned_by, state, state_since,
			 last_activity, token_hash, harness_ref, created_at, model, thinking, harness, capabilities, person)
			VALUES (?,?,'agent','headless',?,?,?,?,?,?,'requested',?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),?,?,0)`,
			res.ParticipantID, res.RunID, role, dir, team, role, p.id, p.id, t.now, t.now,
			hashToken(token), ref, t.now, model, thinking, harness, e.driverCaps(harness)); err != nil {
			return internal(err)
		}
		if err := t.setGate(team, res.ParticipantID); err != nil {
			return err
		}
		if err := t.event(evt{typ: "spawned", participant: p.id, team: p.team, run: p.run, ref: res.ParticipantID,
			payload: map[string]any{"worker": res.ParticipantID, "run_id": res.RunID, "name": role, "role": role,
				"task_id": res.TaskID, "cwd": dir, "taskforce": teamName, "template": a.Template}}); err != nil {
			return err
		}
		seq, err := t.insertMessage(res.TaskID, "", team, p.id, res.ParticipantID, "", "", OpAssign, "", a.Task)
		res.TaskSeq = seq
		return err
	})
	if err != nil {
		return AgentResult{}, err
	}
	e.notifyAfterCommit(res.ParticipantID)
	if err := e.start(ctx, Spec{ParticipantID: res.ParticipantID, RunID: res.RunID, Token: token, Cwd: dir,
		HarnessRef: ref, Harness: harness, Model: model, Thinking: thinking}, res); err != nil {
		_, _ = e.TeamDown(ctx, TeamDownArgs{Team: team, why: "start_failed"}) // no team without a chair
		return AgentResult{}, err
	}
	return res, nil
}

// mayCallTaskforce is who may spawn template=: a solo, or the gate of its team; never a member of a
// taskforce (a taskforce does not call a taskforce).
func (t *txn) mayCallTaskforce(p participant) error {
	if p.team == "" {
		return nil
	}
	parent, err := t.taskforceParent(p.team)
	if err != nil {
		return err
	}
	if parent != "" {
		return deny(&p, "agent.spawn", "permission", "taskforce.nested",
			"a taskforce does not call another taskforce: do the work with your own team, or ask your caller", nil, nil)
	}
	gate, ok, err := t.teamGate(p.team)
	if err != nil {
		return err
	}
	if !ok || gate.id != p.id {
		name := "(none)"
		if ok {
			name = gate.name
		}
		return deny(&p, "agent.spawn", "permission", "taskforce.not_gate",
			fmt.Sprintf("only a solo or your team's gate calls a taskforce; ask %s", name), map[string]any{"gate": name}, map[string]any{"gate": name})
	}
	return nil
}

// closeTaskforce is `agent action=close team=<name>`: the caller closes a taskforce it called up.
func (e *Engine) closeTaskforce(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	var p participant
	var id, name string
	err := e.inTx(ctx, func(t *txn) error {
		var err error
		if p, err = t.callerGranted(c, "agent.close", "agent"); err != nil {
			return err
		}
		err = t.QueryRowContext(t.ctx, `SELECT id, name FROM teams WHERE closed_at IS NULL AND parent_id=? AND (id=? OR name=?)`,
			p.id, a.Team, a.Team).Scan(&id, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return deny(&p, "agent.close", "permission", "taskforce.not_yours",
				fmt.Sprintf("%q is not a taskforce you called up (only its caller closes it, from outside)", a.Team), nil, map[string]any{"team": a.Team})
		}
		return internal(err)
	})
	if err != nil {
		return AgentResult{}, err
	}
	r, err := e.TeamDown(ctx, TeamDownArgs{Team: id, closedBy: p.id})
	if err != nil {
		return AgentResult{}, err
	}
	return AgentResult{TeamID: r.TeamID, TeamName: name, Stopped: r.Stopped, Failed: r.Failed}, nil
}

// Taskforces is the daemon tick's pass over the open taskforces: one whose caller is gone, has left
// or whose team closed is closed as team down; one whose members have all been idle for its
// taskforce.idle_for gets its caller one notice per idle stretch. It returns how many it acted on.
func (e *Engine) Taskforces(ctx context.Context) (int, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id FROM teams WHERE closed_at IS NULL AND parent_id IS NOT NULL`)
	if err != nil {
		return 0, internal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, internal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, internal(err)
	}
	acted := 0
	for _, id := range ids {
		var v taskforceVerdict
		if err := e.inTx(ctx, func(t *txn) (err error) {
			v, err = t.checkTaskforce(id)
			return err
		}); err != nil {
			return acted, err
		}
		switch {
		case v.orphan:
			if _, err := e.TeamDown(ctx, TeamDownArgs{Team: id, why: "caller_gone"}); err != nil {
				return acted, err
			}
			acted++
		case v.chair != "":
			// Closed first, then told: a notice is never sent for a team that is still open.
			if _, err := e.TeamDown(ctx, TeamDownArgs{Team: id, why: "chair_gone"}); err != nil {
				return acted, err
			}
			if err := e.inTx(ctx, func(t *txn) error {
				_, err := t.insertMessage(newID(t.now), "", v.callerTeam, AddrEngine, v.caller, "", "", "", "",
					fmt.Sprintf("Taskforce %s closed: its chair %s stopped.", v.name, v.chair))
				return err
			}); err != nil {
				return acted, err
			}
			e.notifyAfterCommit(v.caller)
			acted++
		case v.wake != "":
			e.notifyAfterCommit(v.wake)
			acted++
		}
	}
	return acted, nil
}

// taskforceVerdict is what one tick decides about one taskforce: orphan (close it); chair, the name
// of its chair that is gone (close it, then tell caller, in callerTeam, that team name closed); or
// wake, the caller a notice was just written to.
type taskforceVerdict struct {
	orphan                          bool
	chair, caller, callerTeam, name string
	wake                            string
}

// checkTaskforce decides one taskforce: orphan (its caller is gone, left, or in a closed team); its
// chair gone (state gone, left, or parked); or a notice written to the caller once every member has
// been idle for idle_for. A gone chair is never brought back: mail does not wake a gate and only the
// caller's own team can resume a worker, so the taskforce would only sit there. A parked chair (its
// process could not be verified at a daemon start) counts: nothing resumes it either, and team down
// verifies it before it stops it. The chair is the member that reports to the caller; the gate may
// have passed to someone else.
func (t *txn) checkTaskforce(id string) (v taskforceVerdict, err error) {
	var text string
	var parent string
	var noticed sql.NullInt64
	if err := t.QueryRowContext(t.ctx, `SELECT manifest, parent_id, idle_noticed, name FROM teams WHERE id=?`, id).Scan(&text, &parent, &noticed, &v.name); err != nil {
		return v, internal(err)
	}
	var left, teamClosed sql.NullInt64
	var state string
	err = t.QueryRowContext(t.ctx, `SELECT state, left_at, (SELECT closed_at FROM teams WHERE id=participants.team_id)
		FROM participants WHERE id=?`, parent).Scan(&state, &left, &teamClosed)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (state == "gone" || left.Valid || teamClosed.Valid)) {
		v.orphan = true
		return v, nil
	}
	if err != nil {
		return v, internal(err)
	}
	var chair string
	err = t.QueryRowContext(t.ctx, `SELECT name, state, left_at FROM participants WHERE team_id=? AND reports_to=?`, id, parent).Scan(&chair, &state, &left)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return v, internal(err)
	}
	if err == nil && (state == "gone" || state == "parked" || left.Valid) {
		caller, ok, err := t.participantByID(parent)
		if err != nil || !ok {
			return v, err
		}
		v.chair, v.caller, v.callerTeam = chair, caller.id, caller.team
		return v, nil
	}
	m, err := parseManifest(text)
	if err != nil || m.Taskforce == nil || m.Taskforce.idle() == 0 {
		return v, nil
	}
	rows, err := t.QueryContext(t.ctx, `SELECT state, state_since FROM participants WHERE team_id=? AND left_at IS NULL`, id)
	if err != nil {
		return v, internal(err)
	}
	defer rows.Close()
	var since int64
	n := 0
	for rows.Next() {
		var st string
		var at int64
		if err := rows.Scan(&st, &at); err != nil {
			return v, internal(err)
		}
		if st != "idle" && st != "gone" {
			return v, nil
		}
		since = max(since, at)
		n++
	}
	if err := rows.Err(); err != nil {
		return v, internal(err)
	}
	if n == 0 || t.now-since < m.Taskforce.idle().Milliseconds() || (noticed.Valid && noticed.Int64 == since) {
		return v, nil
	}
	caller, ok, err := t.participantByID(parent)
	if err != nil || !ok {
		return v, err
	}
	body := fmt.Sprintf("Taskforce %s (template %s) has been idle for %s. If you do not need it any more, close it: %sagent action=close team=%s.",
		v.name, m.Template, age(t.now-since), caller.toolPrefix, v.name)
	if _, err := t.insertMessage(newID(t.now), "", caller.team, AddrEngine, caller.id, "", "", "", "", body); err != nil {
		return v, err
	}
	if _, err := t.ExecContext(t.ctx, `UPDATE teams SET idle_noticed=? WHERE id=?`, since, id); err != nil {
		return v, internal(err)
	}
	v.wake = caller.id
	return v, nil
}

// taskforceLines lists the taskforces a session working in cwd can call, one line each: the cards
// of a solo and of a gate.
func (e *Engine) taskforceLines(cwd string) []string {
	if e.templateList == nil || e.templates == nil {
		return nil
	}
	refs, err := e.templateList(cwd)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range refs {
		text, err := e.templates(r.Name, cwd)
		if err != nil {
			continue
		}
		m, _, err := loadManifest(text)
		if err != nil || m.Taskforce == nil {
			continue
		}
		out = append(out, "- "+r.Name+": "+strings.Join(strings.Fields(m.Summary), " "))
	}
	return out
}

// callTaskforceText is what a solo's and a gate's card says about calling a taskforce.
func callTaskforceText(prefix string, lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return " You may call up a temporary team on your own judgment, like a subagent: " + prefix +
		"agent action=spawn template=<name> task=<what to do>; it works in your directory and answers you by mail" +
		" (send to the team's name to talk to it). Call one for a one-off second opinion, a review that matters, a decision or approach you are stuck on, or a short bounded job;" +
		" not for work you can do yourself. Each taskforce's summary says what it does and whether it edits. It does not close itself: you close it, when you are done with it, with " + prefix + "agent action=close team=<name>." +
		" Taskforces you can call:\n" + strings.Join(lines, "\n") + "\n"
}

// taskforceLinesOf is the taskforces p (a solo or a gate) can call, from its directory.
func (t *txn) taskforceLinesOf(p participant) []string {
	if t.taskforces == nil {
		return nil
	}
	var cwd string
	if err := t.QueryRowContext(t.ctx, `SELECT cwd FROM participants WHERE id=?`, p.id).Scan(&cwd); err != nil {
		return nil
	}
	return t.taskforces(cwd)
}

// taskforceCard is what a member's card adds about taskforces: the chair of one is told whose it is
// and that its result goes to the caller; the gate of an ordinary team lists the ones it can call.
func (t *txn) taskforceCard(p participant, m manifest) (string, error) {
	parent, err := t.taskforceParent(p.team)
	if err != nil {
		return "", err
	}
	gate, ok, err := t.teamGate(p.team)
	if err != nil || !ok || gate.id != p.id {
		return "", err
	}
	if parent != "" {
		var caller string
		if err := t.QueryRowContext(t.ctx, `SELECT name FROM participants WHERE id=?`, parent).Scan(&caller); err != nil {
			return "", internal(err)
		}
		return " You are a taskforce called by " + caller + ": your result goes to " + caller + " by mail (send to " + caller +
			"). Stay for follow-ups: " + caller + " closes the taskforce, you do not.", nil
	}
	if !contains(m.Roles[p.role].Tools, "agent") {
		return "", nil
	}
	return callTaskforceText(p.toolPrefix, t.taskforceLinesOf(p)), nil
}

// taskforceWarnings names the keys of the taskforce block this binary does not know and ignores.
func taskforceWarnings(text string) []string {
	var raw struct {
		Taskforce map[string]any `yaml:"taskforce"`
	}
	_ = yaml.Unmarshal([]byte(text), &raw)
	var out []string
	for _, k := range slices.Sorted(maps.Keys(raw.Taskforce)) {
		if k != "idle_for" {
			out = append(out, fmt.Sprintf("taskforce.%s: unknown key, ignored by this piggery (known: idle_for)", k))
		}
	}
	return out
}
