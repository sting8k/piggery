package core

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Watch rules (manifest `timers:`): the engine notices a participant working with no turn end
// for longer than working_for and tells the rule's target once per incident; an `every` rule
// tells the members of its role every period while someone else in the team works. Nothing is
// judged by content.

// watchRule is one entry of the manifest's `timers:` list.
type watchRule struct {
	On         string `yaml:"on"`
	Notify     string `yaml:"notify"` // a role or reports_to (notify loads and is ignored)
	WorkingFor string `yaml:"working_for"`
	SilentFor  string `yaml:"silent_for"` // the old name of working_for: still read, check warns
	Every      string `yaml:"every"`      // tell the members of On every period while another member works
}

var watchKeys = map[string]bool{"on": true, "notify": true, "working_for": true, "silent_for": true, "every": true}

// every returns the rule's every period: at least a minute, like a repeating runtime timer, and
// never with working_for or notify (it tells the members of its own role).
func (r watchRule) every() (time.Duration, error) {
	if r.WorkingFor != "" || r.SilentFor != "" || r.Notify != "" {
		return 0, fmt.Errorf("every takes no working_for and no notify: it tells the members of on")
	}
	d, err := time.ParseDuration(r.Every)
	if err == nil && d.Milliseconds() < minTimerEveryMs {
		err = fmt.Errorf("every must be at least %s", time.Duration(minTimerEveryMs)*time.Millisecond)
	}
	return d, err
}

// workingFor returns the rule's working_for duration (silent_for when only the old name is set).
func (r watchRule) workingFor() (time.Duration, error) {
	v := r.WorkingFor
	switch {
	case v != "" && r.SilentFor != "":
		return 0, fmt.Errorf("set working_for only (silent_for is its old name)")
	case v == "":
		v = r.SilentFor
	}
	if v == "" {
		return 0, fmt.Errorf("needs working_for or every")
	}
	d, err := time.ParseDuration(v)
	if err == nil && d <= 0 {
		err = fmt.Errorf("working_for must be positive")
	}
	return d, err
}

// timerWarnings says, in one line, how many rules still use the old key silent_for.
func timerWarnings(text string) []string {
	rules, _ := parseTimers(text)
	n := 0
	for _, r := range rules {
		if r.SilentFor != "" && r.WorkingFor == "" {
			n++
		}
	}
	switch n {
	case 0:
		return nil
	case 1:
		return []string{"1 timer uses silent_for; rename it working_for"}
	}
	return []string{fmt.Sprintf("%d timers use silent_for; rename them working_for", n)}
}

// parseTimers reads the manifest's `timers:` list.
func parseTimers(text string) ([]watchRule, error) {
	var t struct {
		Timers []watchRule `yaml:"timers"`
	}
	err := yaml.Unmarshal([]byte(text), &t)
	return t.Timers, err
}

// validateTimers checks the `timers:` list of a manifest being brought up.
func validateTimers(text string, m manifest) error {
	var raw struct {
		Timers []map[string]any `yaml:"timers"`
	}
	if err := yaml.Unmarshal([]byte(text), &raw); err != nil {
		return errf(CodeInvalid, "manifest: timers: %v", err)
	}
	for i, r := range raw.Timers {
		for k := range r {
			if !watchKeys[k] {
				return errf(CodeInvalid, "manifest: timers[%d]: unknown key %q", i, k)
			}
		}
	}
	rules, err := parseTimers(text)
	if err != nil {
		return errf(CodeInvalid, "manifest: timers: %v", err)
	}
	for i, r := range rules {
		if _, ok := m.Roles[r.On]; !ok {
			return errf(CodeInvalid, "manifest: timers[%d]: on: unknown role %q", i, r.On)
		}
		if r.Every != "" {
			if _, err := r.every(); err != nil {
				return errf(CodeInvalid, "manifest: timers[%d]: %v", i, err)
			}
			continue
		}
		if _, ok := m.Roles[r.Notify]; !ok && r.Notify != "reports_to" && r.Notify != AddrNotify {
			return errf(CodeInvalid, "manifest: timers[%d]: notify: %q is not a role, reports_to, or notify", i, r.Notify)
		}
		if _, err := r.workingFor(); err != nil {
			return errf(CodeInvalid, "manifest: timers[%d]: %v", i, err)
		}
	}
	return nil
}

// Watch evaluates the watch rules of every open team (daemon tick) and sends one notice per new
// incident. It returns the number of incidents fired. Only a participant that is working can be an
// incident (silentIncident), so a team with none is not read at all, and a rule opens a transaction only
// when one of its subjects is past working_for: a tick that finds nobody overdue is two reads, not a
// transaction per rule of every team (that woke the CPU ~10 times a second per team).
func (e *Engine) Watch(ctx context.Context) (int, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id, manifest FROM teams WHERE closed_at IS NULL AND EXISTS (
		SELECT 1 FROM participants p WHERE p.team_id=teams.id AND p.state='working' AND p.left_at IS NULL)`)
	if err != nil {
		return 0, internal(err)
	}
	type team struct{ id, text string }
	var teams []team
	for rows.Next() {
		var t team
		if err := rows.Scan(&t.id, &t.text); err != nil {
			rows.Close()
			return 0, internal(err)
		}
		teams = append(teams, t)
	}
	rows.Close()
	open := map[string]bool{}
	for _, tm := range teams {
		open[tm.id] = true
	}
	e.pruneEvery(open, e.now().UnixMilli())
	if len(teams) == 0 {
		return 0, rows.Err()
	}
	// Since when each (team, role) has had a working participant with no turn end: its oldest one.
	oldest := map[string]int64{}
	working := map[string][]string{} // team -> roles with a working participant
	orows, err := e.db.QueryContext(ctx, `SELECT team_id, role, MIN(MAX(state_since, COALESCE(last_turn_end,0)))
		FROM participants WHERE state='working' AND left_at IS NULL AND team_id IS NOT NULL GROUP BY team_id, role`)
	if err != nil {
		return 0, internal(err)
	}
	for orows.Next() {
		var team, role string
		var since int64
		if err := orows.Scan(&team, &role, &since); err != nil {
			orows.Close()
			return 0, internal(err)
		}
		oldest[team+"/"+role] = since
		working[team] = append(working[team], role)
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return 0, internal(err)
	}
	fired := 0
	for _, tm := range teams {
		rules, err := parseTimers(tm.text)
		if err != nil {
			continue
		}
		for i, r := range rules {
			if r.Every != "" {
				n, err := e.everyRule(ctx, tm.id, i, r, working[tm.id])
				if err != nil {
					return fired, err
				}
				fired += n
				continue
			}
			d, err := r.workingFor()
			if err != nil {
				continue // TeamUp validated; skip anything unreadable
			}
			if r.Notify == AddrNotify {
				continue // only the engine writes to notify (notifyWarnings)
			}
			if since, ok := oldest[tm.id+"/"+r.On]; !ok || e.now().UnixMilli()-since <= d.Milliseconds() {
				continue // the same age test as silentIncident: nobody is overdue, no transaction
			}
			var wake []string
			err = e.inTx(ctx, func(t *txn) error {
				n, w, err := t.watchRule(tm.id, i, r, d)
				fired += n
				wake = w
				return err
			})
			if err != nil {
				return fired, err
			}
			for _, id := range wake {
				e.notifyAfterCommit(id)
			}
		}
	}
	return fired, rows.Err()
}

// everyIdleGraceMs is how long a team may have no one working before its every periods are
// forgotten: a pause shorter than this (a lead answering the Human in short turns) keeps them.
// Chosen from measured gaps between a human's messages (92% <= 10 minutes).
const everyIdleGraceMs = 10 * 60_000

// pruneEvery forgets the every periods of a team that has had no one working for everyIdleGraceMs,
// so a team that starts work again after that is told one period after, not at once. working is
// the teams with someone working at now. Memory only: no read, so a tick stays cheap.
func (e *Engine) pruneEvery(working map[string]bool, now int64) {
	e.everyMu.Lock()
	defer e.everyMu.Unlock()
	for team, last := range e.everyBusy {
		if now-last < everyIdleGraceMs {
			continue
		}
		delete(e.everyBusy, team) // also when work is back: the gap was past the grace
		for k := range e.everyLast {
			if k[:strings.IndexByte(k, '/')] == team {
				delete(e.everyLast, k)
			}
		}
	}
	if e.everyBusy == nil {
		e.everyBusy = map[string]int64{}
	}
	for team := range working {
		e.everyBusy[team] = now
	}
}

// everyRule tells the members of r.On, once a period, who in the team is working, while a member
// of another role works and none of r.On does (one that is working is still at it). The period
// starts when such work is first seen; it lives in memory, so a daemon restart starts it again.
func (e *Engine) everyRule(ctx context.Context, teamID string, idx int, r watchRule, roles []string) (int, error) {
	d, err := r.every()
	if err != nil {
		return 0, nil // TeamUp validated; skip anything unreadable
	}
	other := false
	for _, role := range roles {
		if role == r.On {
			return 0, nil
		}
		other = true
	}
	if !other {
		return 0, nil
	}
	key, now := teamID+"/"+strconv.Itoa(idx), e.now().UnixMilli()
	e.everyMu.Lock()
	if e.everyLast == nil {
		e.everyLast = map[string]int64{}
	}
	last, seen := e.everyLast[key]
	if !seen {
		e.everyLast[key] = now
	}
	e.everyMu.Unlock()
	if !seen || now-last < d.Milliseconds() {
		return 0, nil
	}
	var wake []string
	err = e.inTx(ctx, func(t *txn) error {
		wake = nil
		var names []string
		rows, err := t.QueryContext(t.ctx, `SELECT name FROM participants WHERE team_id=? AND state='working'
			AND left_at IS NULL AND role<>? ORDER BY name`, teamID, r.On)
		if err != nil {
			return internal(err)
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return internal(err)
			}
			names = append(names, n)
		}
		rows.Close()
		targets, err := t.teamRole(teamID, r.On)
		if err != nil {
			return err
		}
		body := fmt.Sprintf("Working now: %s (rule every %s).", strings.Join(names, ", "), r.Every)
		for _, to := range targets {
			if to.state == "gone" {
				continue
			}
			if _, err := t.insertMessage(newID(t.now), "", teamID, AddrEngine, to.id, "", "", "", body); err != nil {
				return err
			}
			wake = append(wake, to.id)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	e.everyMu.Lock()
	e.everyLast[key] = now
	e.everyMu.Unlock()
	for _, id := range wake {
		e.notifyAfterCommit(id)
	}
	return len(wake), nil
}

// incident is one predicate hit: the subject, its dedupe key, and the notice text part.
type incident struct {
	subject participant
	key     string
	what    string // e.g. "has been working for 21m with no turn end"
}

// watchRule fires one rule of a team: every incident with an unseen key gets notices and a
// watch_fired event. It returns the incidents fired and the participants to wake.
func (t *txn) watchRule(teamID string, idx int, r watchRule, d time.Duration) (fired int, wake []string, err error) {
	subjects, err := t.teamRole(teamID, r.On)
	if err != nil {
		return 0, nil, err
	}
	// The incident key keeps the rule's first name, so an upgrade does not notice it twice.
	ruleID := fmt.Sprintf("timers[%d] silent_for", idx)
	var incidents []incident
	for _, p := range subjects {
		in, ok, err := t.silentIncident(p, d)
		if err != nil {
			return 0, nil, err
		}
		if ok {
			in.key = teamID + "/" + ruleID + "/" + in.key
			incidents = append(incidents, in)
		}
	}
	for _, in := range incidents {
		var seen int
		if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM events WHERE type='watch_fired' AND
			participant=? AND json_extract(payload,'$.key')=?`, in.subject.id, in.key).Scan(&seen); err != nil {
			return 0, nil, internal(err)
		}
		if seen > 0 {
			continue
		}
		targets, err := t.watchTargets(teamID, r.Notify, in.subject)
		if err != nil {
			return 0, nil, err
		}
		for _, to := range targets {
			who := label(to, in.subject)
			body := fmt.Sprintf("%s %s (rule working_for %s).", who, in.what, cmp.Or(r.WorkingFor, r.SilentFor))
			msg := newID(t.now)
			if _, err := t.insertMessage(msg, "", teamID, AddrEngine, to.id, "", "", "", body); err != nil {
				return 0, nil, err
			}
			wake = append(wake, to.id)
		}
		if err := t.event(evt{typ: "watch_fired", participant: in.subject.id, team: teamID, run: in.subject.run,
			payload: map[string]any{"rule": ruleID, "participant": in.subject.id, "key": in.key,
				"notified": len(targets)}}); err != nil {
			return 0, nil, err
		}
		fired++
	}
	return fired, wake, nil
}

// silentIncident reports p working with no turn end for longer than d. The key (relative to
// the rule) is the working stretch; last_activity is never used.
func (t *txn) silentIncident(p participant, d time.Duration) (incident, bool, error) {
	if p.state != "working" {
		return incident{}, false, nil
	}
	var lastTurnEnd sql.NullInt64
	if err := t.QueryRowContext(t.ctx, `SELECT last_turn_end FROM participants WHERE id=?`, p.id).Scan(&lastTurnEnd); err != nil {
		return incident{}, false, internal(err)
	}
	since := p.stateSince
	if lastTurnEnd.Valid && lastTurnEnd.Int64 > since {
		since = lastTurnEnd.Int64
	}
	if t.now-since <= d.Milliseconds() {
		return incident{}, false, nil
	}
	return incident{subject: p, key: fmt.Sprintf("%s/%d", p.id, p.stateSince),
		what: fmt.Sprintf("has been working for %s with no turn end", age(t.now-since))}, true, nil
}

// watchTargets resolves a rule's notify target for subject p.
func (t *txn) watchTargets(teamID, notify string, p participant) ([]participant, error) {
	switch notify {
	case "reports_to":
		if p.reportsTo == "" {
			return nil, nil
		}
		q, ok, err := t.participantByID(p.reportsTo)
		if err != nil || !ok {
			return nil, err
		}
		return []participant{q}, nil
	}
	all, err := t.teamRole(teamID, notify)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, q := range all {
		if q.id != p.id {
			out = append(out, q)
		}
	}
	return out, nil
}

// teamRole returns the members of role in teamID; a row that left the team is not a member.
func (t *txn) teamRole(teamID, role string) ([]participant, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+` FROM participants WHERE team_id=? AND role=? AND left_at IS NULL ORDER BY name`,
		teamID, role)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	var out []participant
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return nil, internal(err)
		}
		out = append(out, p)
	}
	return out, internal(rows.Err())
}

func age(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}
