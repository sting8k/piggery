package core

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// TeamUp creates a team from a manifest. An explicit name must not clash with an open team; with
// none, the team is named after its directory, with a free suffix, as `agent found` does.
func (e *Engine) TeamUp(ctx context.Context, a TeamUpArgs) (Team, error) {
	m, warnings, err := loadManifest(a.Manifest)
	if err != nil {
		return Team{}, err
	}
	cwd, err := normalizeCwd(a.Cwd)
	if err != nil {
		return Team{}, err
	}
	name := strings.TrimSpace(a.Name)
	var team Team
	err = e.inTx(ctx, func(t *txn) error {
		name := name // a retry of this transaction names the team again
		if name == "" {
			var err error
			if name, err = t.freeTeamName(filepath.Base(cwd)); err != nil {
				return err
			}
		}
		var existing string
		err := t.QueryRowContext(t.ctx, `SELECT id FROM teams WHERE name=? AND closed_at IS NULL`, name).Scan(&existing)
		if err == nil {
			return &Error{Code: CodeInvalid, Message: "an open team already has name " + name, Details: existing}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return internal(err)
		}
		team, err = t.insertTeam(name, a.Manifest, m, cwd)
		return err
	})
	if err == nil {
		team.Warnings = warnings
	}
	return team, err
}

// manifestWarnings are what a manifest allows but probably does not mean: a role that pins a model
// or a thinking level while its harness is inherit. Model names and levels belong to one harness,
// so the pin holds only when the worker happens to run on it. Sorted by role; team up still works.
func manifestWarnings(m manifest) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(m.Roles)) {
		sp := m.Roles[name].Spawn
		for _, pin := range []struct{ key, val, what string }{{"model", sp.Model, "model names"}, {"thinking", sp.Thinking, "thinking levels"}} {
			if pin.val != "" && sp.Harness == "" {
				out = append(out, fmt.Sprintf("role %q sets spawn.%s %q but spawn.harness is inherit: %s belong to one harness, so pin spawn.harness too",
					name, pin.key, pin.val, pin.what))
			}
		}
	}
	return out
}

// validManifest parses a manifest and checks everything TeamUp refuses.
func validManifest(text string) (manifest, error) {
	m, err := parseManifest(text)
	if err != nil {
		return m, err
	}
	if err := noDeclaredTools(text); err != nil {
		return m, err
	}
	if err := validateTimers(text, m); err != nil {
		return m, err
	}
	if err := validateRoles(m); err != nil {
		return m, err
	}
	if err := validateTaskforce(m); err != nil {
		return m, err
	}
	for k := range m.Limits {
		if removedLimits[k] { // `none` never gets here: a limit set to none is left out
			return m, errf(CodeInvalid, "manifest: limits.%s was removed from piggery; delete the key (messages_per_participant_per_minute is the flood guard)", k)
		}
		if !knownLimits[k] {
			return m, errf(CodeInvalid, "manifest: limits.%s: unknown limit", k)
		}
	}
	// Spawning without declared limits would allow unbounded recursive spawns.
	for name, r := range m.Roles {
		_, depth := m.Limits["depth"]
		_, conc := m.Limits["concurrency"]
		if len(r.CanSpawn) > 0 && (!depth || !conc) {
			return m, errf(CodeInvalid, "manifest: role %q can spawn, so limits.depth and limits.concurrency must be numbers (not none)", name)
		}
	}
	return m, nil
}

// insertTeam stores a new open team (name already checked free) with its team_up event.
func (t *txn) insertTeam(name, text string, m manifest, cwd string) (Team, error) {
	team := Team{ID: newID(t.now), Name: name, Template: m.Template, RootCwd: cwd, CreatedAt: t.now}
	if _, err := t.ExecContext(t.ctx,
		`INSERT INTO teams(id, name, template_name, manifest, root_cwd, created_at) VALUES (?,?,?,?,?,?)`,
		team.ID, team.Name, team.Template, text, team.RootCwd, team.CreatedAt); err != nil {
		return Team{}, internal(err)
	}
	return team, t.event(evt{typ: "team_up", team: team.ID, ref: team.ID,
		payload: map[string]any{"name": team.Name, "template": team.Template, "root_cwd": team.RootCwd}})
}

// Join registers a participant in an open team and issues its token (only the hash is stored).
func (e *Engine) Join(ctx context.Context, a JoinArgs) (JoinResult, error) {
	name := strings.TrimSpace(a.Name)
	if name == "" || isReserved(name) {
		return JoinResult{}, errf(CodeInvalid, "invalid participant name %q", a.Name)
	}
	cwd, err := normalizeCwd(a.Cwd)
	if err != nil {
		return JoinResult{}, err
	}
	kind, harness, mode := orDefault(a.Kind, "agent"), orDefault(a.Harness, "cli"), orDefault(a.Mode, "pull")
	if mode == modeHeadless {
		return JoinResult{}, errf(CodeInvalid, "mode headless is only for spawned workers")
	}
	token, err := newToken()
	if err != nil {
		return JoinResult{}, internal(err)
	}
	openTeam := func(t *txn) (id, root string, err error) {
		err = t.QueryRowContext(t.ctx, `SELECT id, root_cwd FROM teams WHERE closed_at IS NULL AND (id=? OR name=?)`,
			a.Team, a.Team).Scan(&id, &root)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", errf(CodeNotFound, "no open team %q", a.Team)
		}
		return id, root, internal(err)
	}
	// A member's cwd is within the team's bounds, as a worker's: its workers inherit it. Checked
	// outside the tx: the bounds may run git.
	var root string
	if err := e.inTx(ctx, func(t *txn) (err error) { _, root, err = openTeam(t); return err }); err != nil {
		return JoinResult{}, err
	}
	if !e.inBounds(ctx, root, cwd) {
		return JoinResult{}, e.inTx(ctx, func(*txn) error {
			return deny(nil, "join", "bounds", "cwd.bounds", outOfBounds(cwd), nil, map[string]any{"cwd": cwd, "team": a.Team})
		})
	}
	var res JoinResult
	err = e.inTx(ctx, func(t *txn) error {
		teamID, _, err := openTeam(t)
		if err != nil {
			return err
		}
		m, err := t.teamManifest(teamID)
		if err != nil {
			return err
		}
		if _, ok := m.Roles[a.Role]; !ok {
			return errf(CodeInvalid, "role %q not in team manifest", a.Role)
		}
		var n int
		if err := t.QueryRowContext(t.ctx,
			`SELECT COUNT(*) FROM participants WHERE team_id=? AND name=?`, teamID, name).Scan(&n); err != nil {
			return internal(err)
		}
		if n > 0 {
			return errf(CodeInvalid, "name %q already taken in team", name)
		}
		res = JoinResult{ID: newID(t.now), Token: token, RunID: newID(t.now), TeamID: teamID}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO participants
			(id, run_id, kind, harness, mode, name, cwd, team_id, role, state, state_since, last_activity, token_hash, created_at, person)
			VALUES (?,?,?,?,?,?,?,?,?,'idle',?,?,?,?,1)`,
			res.ID, res.RunID, kind, harness, mode, name, cwd, teamID, a.Role, t.now, t.now, hashToken(token), t.now); err != nil {
			return internal(err)
		}
		return t.gateIfNone(teamID, res.ID)
	})
	if err != nil {
		return JoinResult{}, err
	}
	return res, nil
}

// Authenticate maps (PIGGERY_ID, PIGGERY_TOKEN) to the participant's current run.
// A refusal is an unauthorized error with a denied event (layer token).
// AuthenticateHost resolves a caller by its host process (JoinAutoArgs.Host) instead of a
// token: a Claude session's hooks run once each and keep nothing. Only a live session a person
// opened (not gone, not headless) is found; its current run is the caller's. Local trust, as
// join.auto (the socket is 0600).
func (e *Engine) AuthenticateHost(ctx context.Context, host string) (Caller, error) {
	var c Caller
	err := e.readOnly(ctx, func(t *txn) error {
		var closed sql.NullInt64
		p, err := scanParticipant(t.QueryRowContext(t.ctx, `SELECT `+participantCols+`,
			(SELECT closed_at FROM teams WHERE id=participants.team_id) FROM participants
			WHERE host=? AND state<>'gone' AND left_at IS NULL AND COALESCE(mode,'')<>'headless'
			ORDER BY created_at DESC, rowid DESC LIMIT 1`, host), &closed)
		if errors.Is(err, sql.ErrNoRows) {
			return &Error{Code: CodeUnauthorized, Message: "no live session with this host", RuleID: "host.unknown", Layer: "token"}
		}
		if err != nil {
			return internal(err)
		}
		if closed.Valid {
			return &Error{Code: CodeUnauthorized, Message: "the team is closed", RuleID: "team.closed", Layer: "token"}
		}
		c = Caller{ParticipantID: p.id, RunID: p.run, TeamID: p.team, Role: p.role, Name: p.name, ByHost: true}
		return nil
	})
	return c, err
}

func (e *Engine) Authenticate(ctx context.Context, id, token string) (Caller, error) {
	var c Caller
	err := e.inTx(ctx, func(t *txn) error {
		var hash, mode string
		var closed sql.NullInt64
		p, err := scanParticipant(t.QueryRowContext(t.ctx,
			`SELECT `+participantCols+`, token_hash, (SELECT closed_at FROM teams WHERE id=participants.team_id),
			COALESCE(mode,'') FROM participants WHERE id=?`, id), &hash, &closed, &mode)
		if errors.Is(err, sql.ErrNoRows) {
			return deny(nil, "authenticate", "token", "token.invalid", "unknown participant", nil,
				map[string]any{"claimed_id": id})
		}
		if err != nil {
			return internal(err)
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(token))) != 1 {
			return deny(&p, "authenticate", "token", "token.invalid", "token does not match", nil, nil)
		}
		if closed.Valid { // team down: every verb of its participants is refused here
			if mode == modeHeadless {
				// A worker team down stopped: its extension calling while it stops is not a
				// decision worth an event (it was noise in the log); refused all the same.
				return &Error{Code: CodeUnauthorized, Message: "the team is closed", RuleID: "team.closed", Layer: "token"}
			}
			return deny(&p, "authenticate", "token", "team.closed", "the team is closed", nil, nil)
		}
		c = Caller{ParticipantID: p.id, RunID: p.run, TeamID: p.team, Role: p.role, Name: p.name}
		return nil
	})
	return c, err
}

// Log returns events with seq > After, oldest first. Events are minimal: decisions (denied, held,
// released, reconcile, gc, notice, watch_fired) and team/worker lifecycle (team_up/down, spawned,
// spawn_failed, stopped, exited, resumed, respawn_limit). Message and turn history is read from the
// state tables (trace, inbox views, who).
func (e *Engine) Log(ctx context.Context, a LogArgs) ([]Event, error) {
	limit := a.Limit
	if limit <= 0 {
		limit = 200
	}
	rows, err := e.db.QueryContext(ctx, `SELECT seq, ts, type, COALESCE(participant,''), COALESCE(team_id,''),
		COALESCE(run_id,''), COALESCE(ref_id,''), payload FROM events
		WHERE seq > ? AND (? = '' OR team_id = ?) ORDER BY seq LIMIT ?`, a.After, a.Team, a.Team, limit)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var ev Event
		var payload string
		if err := rows.Scan(&ev.Seq, &ev.Ts, &ev.Type, &ev.Participant, &ev.TeamID, &ev.RunID, &ev.RefID, &payload); err != nil {
			return nil, internal(err)
		}
		ev.Payload = []byte(payload)
		out = append(out, ev)
	}
	return out, internal(rows.Err())
}

// modeHeadless: the daemon runs this participant now, a spawned worker or a person's session a mail
// woke (wake.go; the person's join sets the mode it reports again). Whose session it is lives in
// participants.person, which never changes.
const modeHeadless = "headless"

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// knownLimits are the manifest limits the engine enforces; any other key is a mistake.
var knownLimits = map[string]bool{"depth": true, "concurrency": true, "messages_per_participant_per_minute": true,
	"max_respawn_per_hour": true}

// removedLimits are limits that no longer exist (nothing enforced them by a mechanism the model
// cannot skip). A template still setting one is refused, not ignored; a team
// brought up before keeps its stored manifest, which the engine never checks again.
var removedLimits = map[string]bool{"messages_per_thread": true, "max_hops": true}
