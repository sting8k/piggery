package core

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Watch rules (manifest `timers:`): the engine notices a participant working with no turn end
// for longer than silent_for and tells the rule's target once per incident. Nothing is judged
// by content.

// watchRule is one entry of the manifest's `timers:` list.
type watchRule struct {
	On        string `yaml:"on"`
	Notify    string `yaml:"notify"` // a role, reports_to, or notify
	SilentFor string `yaml:"silent_for"`
}

var watchKeys = map[string]bool{"on": true, "notify": true, "silent_for": true}

// silence returns the rule's silent_for duration.
func (r watchRule) silence() (time.Duration, error) {
	if r.SilentFor == "" {
		return 0, fmt.Errorf("needs silent_for")
	}
	d, err := time.ParseDuration(r.SilentFor)
	if err == nil && d <= 0 {
		err = fmt.Errorf("silent_for must be positive")
	}
	return d, err
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
		if _, ok := m.Roles[r.Notify]; !ok && r.Notify != "reports_to" && r.Notify != AddrNotify {
			return errf(CodeInvalid, "manifest: timers[%d]: notify: %q is not a role, reports_to, or notify", i, r.Notify)
		}
		if _, err := r.silence(); err != nil {
			return errf(CodeInvalid, "manifest: timers[%d]: %v", i, err)
		}
	}
	return nil
}

// Watch evaluates every open team's watch rules (daemon tick) and sends one notice per new
// incident. It returns the number of incidents fired.
func (e *Engine) Watch(ctx context.Context) (int, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id, manifest FROM teams WHERE closed_at IS NULL`)
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
	fired := 0
	for _, tm := range teams {
		rules, err := parseTimers(tm.text)
		if err != nil {
			continue
		}
		for i, r := range rules {
			d, err := r.silence()
			if err != nil {
				continue // TeamUp validated; skip anything unreadable
			}
			var wake, hooks []string
			err = e.inTx(ctx, func(t *txn) error {
				n, w, h, err := t.watchRule(tm.id, i, r, d)
				fired += n
				wake, hooks = w, h
				return err
			})
			if err != nil {
				return fired, err
			}
			for _, id := range wake {
				e.notifyAfterCommit(id)
			}
			for _, id := range hooks {
				e.notifyHookAfterCommit(id)
			}
		}
	}
	return fired, rows.Err()
}

// incident is one predicate hit: the subject, its dedupe key, and the notice text part.
type incident struct {
	subject participant
	key     string
	what    string // e.g. "has been working for 21m with no turn end"
}

// watchRule fires one rule of a team: every incident with an unseen key gets notices and a
// watch_fired event. It returns the incidents fired, the participants to wake, and the
// notices to notify (for its hook).
func (t *txn) watchRule(teamID string, idx int, r watchRule, d time.Duration) (fired int, wake, hooks []string, err error) {
	subjects, err := t.teamRole(teamID, r.On)
	if err != nil {
		return 0, nil, nil, err
	}
	ruleID := fmt.Sprintf("timers[%d] silent_for", idx)
	var incidents []incident
	for _, p := range subjects {
		in, ok, err := t.silentIncident(p, d)
		if err != nil {
			return 0, nil, nil, err
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
			return 0, nil, nil, internal(err)
		}
		if seen > 0 {
			continue
		}
		targets, err := t.watchTargets(teamID, r.Notify, in.subject)
		if err != nil {
			return 0, nil, nil, err
		}
		for _, to := range targets {
			who := in.subject.name + " (" + in.subject.role + ")"
			if to.id != AddrNotify {
				who = label(to, in.subject)
			}
			body := fmt.Sprintf("%s %s (rule silent_for %s).", who, in.what, r.SilentFor)
			msg := newID(t.now)
			if _, err := t.insertMessage(msg, "", teamID, AddrEngine, to.id, "", msg, "", false, "", "", body); err != nil {
				return 0, nil, nil, err
			}
			if to.id == AddrNotify {
				hooks = append(hooks, msg)
			} else {
				wake = append(wake, to.id)
			}
		}
		if err := t.event(evt{typ: "watch_fired", participant: in.subject.id, team: teamID, run: in.subject.run,
			payload: map[string]any{"rule": ruleID, "participant": in.subject.id, "key": in.key,
				"notified": len(targets)}}); err != nil {
			return 0, nil, nil, err
		}
		fired++
	}
	return fired, wake, hooks, nil
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
	case AddrNotify:
		return []participant{{id: AddrNotify}}, nil
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

func (t *txn) teamRole(teamID, role string) ([]participant, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+` FROM participants WHERE team_id=? AND role=? ORDER BY name`,
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
