package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JoinAutoArgs is `join.auto`: a harness session with no PIGGERY_* registers with the daemon. No
// token: the socket's 0600 mode is the boundary.
type JoinAutoArgs struct {
	Cwd        string `json:"cwd"`
	Harness    string `json:"harness,omitempty"`
	Mode       string `json:"mode,omitempty"`
	HarnessRef string `json:"harness_ref"`    // same session -> same participant
	Name       string `json:"name,omitempty"` // default one word from harness_ref (names.go); suffixed on collision
	// Source is why the harness started this session (Claude: startup|resume|clear|compact);
	// recorded only, the host decides.
	Source string `json:"source,omitempty"`
	// Host is the harness process the session lives in ("claude:<pid>:<start time>"), shared by
	// its hooks and MCP server. A live participant with the same host is this session: join.auto
	// returns it with its run (the ref becomes one of its refs), and callers may authenticate by
	// host (AuthenticateHost).
	Host string `json:"host,omitempty"`
	// Transcript is the harness's own file of this session, kept on the participant for the CLI's
	// ctx, turns and tail (never read by the daemon). nil or an empty path keeps the one stored.
	Transcript *Transcript `json:"transcript,omitempty"`
}

// Transcript is a session's own file as its harness writes it, and the format a CLI reader knows
// it by (pi, claude, codex). Core stores and returns it as sent.
type Transcript struct {
	Path   string `json:"path"`
	Format string `json:"format"`
}

// transcriptCols are a.Transcript's path and format for an UPDATE with COALESCE: NULL when none
// was reported, so the stored one stays.
func (a JoinAutoArgs) transcriptCols() (any, any) {
	if a.Transcript == nil || a.Transcript.Path == "" {
		return nil, nil
	}
	return a.Transcript.Path, a.Transcript.Format
}

// JoinAuto registers session a. A session whose newest participant is a solo or a member of an open
// team resumes it (same role and name, new run and token), only if it is gone and not a headless
// worker (live -> CodeInvalid, headless -> CodeNotFound). Otherwise the session is a new solo: a
// participant with no team, whatever its cwd. TeamID "" = solo.
func (e *Engine) JoinAuto(ctx context.Context, a JoinAutoArgs) (JoinResult, error) {
	if a.HarnessRef == "" || a.Mode == modeHeadless {
		return JoinResult{}, errf(CodeInvalid, "harness_ref is required; mode headless is only for spawned workers")
	}
	cwd, err := normalizeCwd(a.Cwd)
	if err != nil {
		return JoinResult{}, err
	}
	token, err := newToken()
	if err != nil {
		return JoinResult{}, internal(err)
	}
	// A person's session that mail woke as a worker (wake.go): the person takes it back, so the
	// worker stops first, outside any transaction. Mail could wake it again in the gap: a few tries.
	for try := 0; try < takeBackTries; try++ {
		w, ok, err := e.wokenSession(ctx, a.HarnessRef)
		if err != nil {
			return JoinResult{}, err
		}
		if !ok {
			break
		}
		if err := e.takeBack(ctx, w); err != nil {
			return JoinResult{}, err
		}
	}
	var res JoinResult
	err = e.inTx(ctx, func(t *txn) error {
		if a.Host != "" { // the same process again (its MCP server and hooks, or /clear): no new participant
			var id, team, run string
			err := t.QueryRowContext(t.ctx, `SELECT id, COALESCE(team_id,''), run_id FROM participants WHERE host=?
				AND state<>'gone' AND left_at IS NULL AND COALESCE(mode,'')<>'headless'
				ORDER BY created_at DESC, rowid DESC LIMIT 1`, a.Host).Scan(&id, &team, &run)
			if err == nil {
				res = JoinResult{ID: id, TeamID: team, RunID: run} // no token: it authenticates by host
				path, format := a.transcriptCols()
				if _, err := t.ExecContext(t.ctx, `UPDATE participants SET session_ref=?, transcript=COALESCE(?,transcript),
					transcript_format=COALESCE(?,transcript_format) WHERE id=?`, a.HarnessRef, path, format, id); err != nil {
					return internal(err)
				}
				_, err := t.ExecContext(t.ctx, `INSERT INTO participant_refs(ref, participant_id) SELECT ?, ?
					WHERE ? <> COALESCE((SELECT harness_ref FROM participants WHERE id=?),'') ON CONFLICT(ref) DO NOTHING`,
					a.HarnessRef, id, a.HarnessRef, id)
				return internal(err)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return internal(err)
			}
		}
		res = JoinResult{Token: token, RunID: newID(t.now)}
		// The newest row wins: after leaving a team (found) the old row is gone in the old team.
		// A row that left (found elsewhere, replaced or merged by reopen) never comes back.
		var mode, host sql.NullString
		var person bool
		p, err := scanParticipant(t.QueryRowContext(t.ctx, `SELECT `+participantCols+`, mode, host, person FROM participants
			WHERE (harness_ref=? OR id IN (SELECT participant_id FROM participant_refs WHERE ref=?)) AND left_at IS NULL
			AND (team_id IS NULL OR team_id IN (SELECT id FROM teams WHERE closed_at IS NULL))
			ORDER BY created_at DESC, rowid DESC LIMIT 1`, a.HarnessRef, a.HarnessRef), &mode, &host, &person)
		switch {
		case err == nil && !person:
			// A worker's session opened by hand (e.g. to inspect it) must never take the worker over.
			// A person's session that mail woke is taken back (takeBack, above): once it is gone.
			return errf(CodeNotFound, "session belongs to a headless worker")
		case err == nil && p.state != "gone" && (a.Host == "" || !host.Valid) && !(p.state == "parked" && mode.String != "headless"):
			// A parked session (the respawn limit refused the wake; no process) is the person's again.
			// Only a session that is gone can come back; a live one keeps its run and token. A
			// live one with another host lost its process without saying so (it reported no
			// end): the new process takes it over, as a resume.
			return errf(CodeInvalid, "participant is live")
		case err == nil: // resume: same participant, new run and token
			res.ID, res.TeamID = p.id, p.team
			path, format := a.transcriptCols()
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET run_id=?, token_hash=?, last_turn_end=NULL,
				cwd=?, harness=COALESCE(?,harness), mode=COALESCE(?,CASE WHEN mode='headless' THEN 'interactive' ELSE mode END), host=?, session_ref=?,
				transcript=COALESCE(?,transcript), transcript_format=COALESCE(?,transcript_format) WHERE id=?`,
				res.RunID, hashToken(token), cwd, nullStr(a.Harness), nullStr(a.Mode), nullStr(a.Host), a.HarnessRef,
				path, format, p.id); err != nil {
				return internal(err)
			}
			p.run = res.RunID
			return t.setState(&p, "idle", "join.auto")
		case !errors.Is(err, sql.ErrNoRows):
			return internal(err)
		}
		res.ID, err = t.insertSession("", "", cwd, res.RunID, token, a)
		return err
	})
	if err != nil {
		return JoinResult{}, err
	}
	return res, nil
}

// moveSession hands what a live session reported (its host, capabilities, tool prefix, model
// and thinking, current session id, transcript) and its other session ids from participant from to participant to, when the
// session goes on as another participant (found by a member, reopen by the old gate). Without
// it, auth by host would find nobody and resuming a /clear'd id would miss the new row.
func (t *txn) moveSession(from, to string) error {
	if _, err := t.ExecContext(t.ctx, `UPDATE participants SET (host, capabilities, tool_prefix, session_model, session_thinking,
		session_ref, transcript, transcript_format) = (SELECT host, capabilities, tool_prefix, session_model, session_thinking,
		session_ref, transcript, transcript_format FROM participants WHERE id=?) WHERE id=?`,
		from, to); err != nil {
		return internal(err)
	}
	if _, err := t.ExecContext(t.ctx, `UPDATE participants SET host=NULL WHERE id=?`, from); err != nil {
		return internal(err)
	}
	_, err := t.ExecContext(t.ctx, `UPDATE participant_refs SET participant_id=? WHERE participant_id=?`, to, from)
	return internal(err)
}

// insertSession adds harness session a as a new idle participant of role in teamID ("" = a
// solo, no team and no role), and returns its id. Its name is a.Name (suffixed on collision); else,
// for a solo whose session had a participant before (its team was closed), that participant's name
// (suffixed on collision); else a free word (freeWord).
func (t *txn) insertSession(teamID, role, cwd, run, token string, a JoinAutoArgs) (string, error) {
	var name string
	var err error
	former, err := t.formerName(teamID, a.HarnessRef)
	if err != nil {
		return "", err
	}
	if n := strings.TrimSpace(a.Name); n != "" && !isReserved(n) {
		name, err = t.freeName(teamID, n)
	} else if former != "" {
		name, err = t.freeSoloName(former)
	} else {
		name, err = t.freeWord(teamID, a.HarnessRef)
	}
	if err != nil {
		return "", err
	}
	id := newID(t.now)
	path, format := a.transcriptCols()
	if _, err := t.ExecContext(t.ctx, `INSERT INTO participants
		(id, run_id, kind, harness, mode, name, cwd, team_id, role, state, state_since, last_activity,
		 token_hash, harness_ref, created_at, host, session_ref, transcript, transcript_format, person)
		VALUES (?,?,'agent',?,?,?,?,?,?,'idle',?,?,?,?,?,?,?,?,?,1)`,
		id, run, nullStr(a.Harness), nullStr(a.Mode), name, cwd, nullStr(teamID), nullStr(role), t.now, t.now,
		hashToken(token), a.HarnessRef, t.now, nullStr(a.Host), a.HarnessRef, path, format); err != nil {
		return "", internal(err)
	}
	return id, nil
}

// sessionRole is the role a harness session joins as: auto_join_role, else the only role, else "".
func (m manifest) sessionRole() string {
	if m.AutoJoinRole != "" || len(m.Roles) != 1 {
		return m.AutoJoinRole
	}
	for only := range m.Roles {
		return only
	}
	return ""
}

// freeName returns name, or name-2, name-3, … when taken in the team (teamID "": by a live
// solo).
func (t *txn) freeName(teamID, name string) (string, error) {
	for i := 1; ; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d", name, i)
		}
		taken, err := t.nameTaken(teamID, cand, false)
		if err != nil || !taken {
			return cand, err
		}
	}
}

// nameTaken: name is a participant's in the team (teamID "": a live solo's), or, with teams,
// an open team's (a send to it would reach that team: resolveSendTarget).
func (t *txn) nameTaken(teamID, name string, teams bool) (bool, error) {
	var n int
	q := `SELECT (SELECT COUNT(*) FROM participants WHERE team_id=? AND name=?)`
	args := []any{teamID, name}
	if teamID == "" {
		q, args = `SELECT (SELECT COUNT(*) FROM participants WHERE team_id IS NULL AND state<>'gone' AND name=?)`, []any{name}
	}
	if teams {
		q += ` + (SELECT COUNT(*) FROM teams WHERE closed_at IS NULL AND name=?)`
		args = append(args, name)
	}
	if err := t.QueryRowContext(t.ctx, q, args...).Scan(&n); err != nil {
		return false, internal(err)
	}
	return n > 0, nil
}

// wokenSession is the participant of session ref when it is a person's session that mail woke
// (a person's row, run headless now) and its worker is not gone.
func (e *Engine) wokenSession(ctx context.Context, ref string) (w participant, ok bool, err error) {
	err = e.inTx(ctx, func(t *txn) error {
		var mode sql.NullString
		var person bool
		p, err := scanParticipant(t.QueryRowContext(t.ctx, `SELECT `+participantCols+`, mode, person FROM participants
			WHERE (harness_ref=? OR id IN (SELECT participant_id FROM participant_refs WHERE ref=?)) AND left_at IS NULL
			AND (team_id IS NULL OR team_id IN (SELECT id FROM teams WHERE closed_at IS NULL))
			ORDER BY created_at DESC, rowid DESC LIMIT 1`, ref, ref), &mode, &person)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return internal(err)
		}
		w, ok = p, person && mode.String == modeHeadless && p.state != "gone"
		return nil
	})
	return w, ok, err
}

// takeBackTries bounds the stops one join makes (see JoinAuto).
const takeBackTries = 3

// takeBackGrace is how long a woken worker may finish its turn when its person comes back: the join
// sits inside a hook with a budget (Codex's SessionStart: 3 s). After that Driver.Kill ends it
// (SIGTERM, a wait of its KillWait, 2 s, then SIGKILL), so the stop takes at most takeBackGrace +
// KillWait = 2.5 s, before the join's own work. A turn is cut then; its mail is unacked and comes
// again.
const takeBackGrace = 500 * time.Millisecond

// takeBack stops w, a worker woken in a person's session: as agent stop, with Kill after
// takeBackGrace. The driver interface is unchanged, the timer is ours.
func (e *Engine) takeBack(ctx context.Context, w participant) error {
	if d := e.runtimeFor(w.harness); d != nil && w.state != "parked" {
		timer := time.AfterFunc(takeBackGrace, func() { d.Kill(ctx, w.id) })
		defer timer.Stop()
	}
	_, err := e.stopWorker(ctx, w, "taken_back", false)
	return err
}
