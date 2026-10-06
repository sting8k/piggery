package core

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Operator diagnostics: `why A B` and `doctor`. Both are admin verbs and read-only.

// GateCheck is one check of the send gate as `why` reports it.
type GateCheck struct {
	Check  string `json:"check"`  // caller, visibility, team_gate, routing, reply_to, board, limits, cc
	Result string `json:"result"` // pass, deny, hold, info
	RuleID string `json:"rule_id,omitempty"`
	Detail string `json:"detail"`
}

type WhyArgs struct {
	From string `json:"from"`           // participant id or name
	To   string `json:"to"`             // as in send: a name/id, notify or board
	Team string `json:"team,omitempty"` // team id or name, when From's name is ambiguous
}

type WhyResult struct {
	WhyVerdict // the send verb
}

// WhyVerdict is one path through the send gate: its checks and the outcome.
type WhyVerdict struct {
	Checks  []GateCheck `json:"checks"`
	Verdict string      `json:"verdict"` // allow, deny, hold
	RuleID  string      `json:"rule_id,omitempty"`
	Layer   string      `json:"layer,omitempty"`
}

// Why runs the send gate for a message From -> To as it stands now, with the same function
// Send uses, and returns each check and the verdict. Nothing is written: the transaction is always rolled back and a denial's event is dropped.
func (e *Engine) Why(ctx context.Context, a WhyArgs) (WhyResult, error) {
	var out WhyResult
	err := e.readOnly(ctx, func(t *txn) error {
		p, err := t.participantForAdmin(a.From, a.Team)
		if err != nil {
			return err
		}
		m, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		caller := fmt.Sprintf("%s is %s, role %s, run %s, state %s", a.From, p.id, p.role, p.run, p.state)
		out.WhyVerdict, err = t.whyGate(p, m, SendArgs{To: a.To, Body: "(why)"}, caller)
		return err
	})
	return out, err
}

// whyGate runs sendGate with a trace that starts with the caller line, and turns its result into
// a verdict.
func (t *txn) whyGate(p participant, m manifest, a SendArgs, caller string) (WhyVerdict, error) {
	var v WhyVerdict
	tr := &gateTrace{}
	tr.add("caller", "pass", "", caller)
	g, err := t.sendGate(p, m, a, tr)
	var d *denial
	switch {
	case errors.As(err, &d):
		tr.add(d.err.Layer, "deny", d.err.RuleID, d.err.Message)
		v.Verdict, v.RuleID, v.Layer = "deny", d.err.RuleID, d.err.Layer
	case err != nil:
		return v, err
	case g.rule != "":
		v.Verdict, v.RuleID, v.Layer = "hold", g.rule, "limit"
	default:
		v.Verdict = "allow"
	}
	v.Checks = tr.checks
	return v, nil
}

// readOnly runs fn in a transaction that is always rolled back.
func (e *Engine) readOnly(ctx context.Context, fn func(t *txn) error) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return internal(err)
	}
	defer tx.Rollback()
	return fn(&txn{Tx: tx, ctx: ctx, now: e.now().UnixMilli(), solo: e.soloManifest(), shared: e.sharedPrompts, taskforces: e.taskforceLines})
}

// participantForAdmin resolves a participant by id, or by name within team (id or name) when
// given; a name in several teams without team is invalid, unless all but one of the matches are
// gone members of closed teams (what is left of a finished team is not a rival for the name).
func (t *txn) participantForAdmin(who, team string) (participant, error) {
	q := `SELECT ` + participantCols + `, COALESCE((SELECT closed_at IS NOT NULL FROM teams WHERE id=participants.team_id), 0)
		FROM participants WHERE (id=? OR name=?)`
	args := []any{who, who}
	if team != "" {
		q += ` AND team_id IN (SELECT id FROM teams WHERE id=? OR name=?)`
		args = append(args, team, team)
	}
	rows, err := t.QueryContext(t.ctx, q, args...)
	if err != nil {
		return participant{}, internal(err)
	}
	defer rows.Close()
	var found, live []participant
	for rows.Next() {
		var closed bool
		p, err := scanParticipant(rows, &closed)
		if err != nil {
			return participant{}, internal(err)
		}
		found = append(found, p)
		if !closed || p.state != "gone" {
			live = append(live, p)
		}
	}
	if err := rows.Err(); err != nil {
		return participant{}, internal(err)
	}
	if len(live) > 0 {
		found = live
	}
	switch len(found) {
	case 0:
		return participant{}, errf(CodeNotFound, "no participant %q", who)
	case 1:
		return found[0], nil
	}
	teams := make([]string, len(found))
	for i, p := range found {
		teams[i] = "(solo)"
		if p.team != "" {
			if err := t.QueryRowContext(t.ctx, `SELECT name FROM teams WHERE id=?`, p.team).Scan(&teams[i]); err != nil {
				return participant{}, internal(err)
			}
		}
	}
	return participant{}, errf(CodeInvalid, "%q names %d participants, in teams %s; pass --team", who, len(found), strings.Join(teams, ", "))
}

// Finding is one problem doctor reports.
type Finding struct {
	Kind   string   `json:"kind"`
	Detail string   `json:"detail"`
	IDs    []string `json:"ids"`
}

type DoctorResult struct {
	Findings []Finding `json:"findings"`
}

// DoctorStaleMs is how long a batch may stay open or a turn stay unsettled before doctor flags it.
const DoctorStaleMs = 30 * 60 * 1000

// Doctor checks state and event invariants an operator should look at. It changes nothing.
func (e *Engine) Doctor(ctx context.Context) (DoctorResult, error) {
	out := DoctorResult{Findings: []Finding{}}
	err := e.readOnly(ctx, func(t *txn) error {
		stale := t.now - DoctorStaleMs
		checks := []struct {
			kind, query string
			args        []any
			detail      func(cols []string) string
		}{
			// A batch with ended_at was closed unacked (interrupted or failed turn): not open.
			{"open_batch", `SELECT p.id, b.run_id, b.batch_seq, strftime('%Y-%m-%dT%H:%M:%SZ', b.opened_at/1000, 'unixepoch') FROM batches b
				JOIN participants p ON p.run_id = b.run_id
				WHERE b.completed_at IS NULL AND b.ended_at IS NULL AND b.opened_at < ? AND p.state <> 'gone'`, []any{stale},
				func(c []string) string { return fmt.Sprintf("batch %s of run %s open since %s", c[2], c[1], c[3]) }},
			{"unsettled_start", `SELECT id, run_id, strftime('%Y-%m-%dT%H:%M:%SZ', state_since/1000, 'unixepoch')
				FROM participants WHERE state = 'working' AND state_since < ?`, []any{stale},
				func(c []string) string {
					return fmt.Sprintf("working since %s in run %s with no agent_settled", c[2], c[1])
				}},
			{"batch_order", `SELECT a.run_id, a.batch_seq, b.batch_seq FROM batches a
				JOIN batches b ON b.run_id = a.run_id AND b.opened_at > a.opened_at AND b.batch_seq <= a.batch_seq`, nil,
				func(c []string) string {
					return fmt.Sprintf("run %s opened batch %s after batch %s", c[0], c[2], c[1])
				}},
			{"gone_unacked", `SELECT p.id, COUNT(*), MIN(m.id) FROM participants p
				JOIN messages m ON m.to_id = p.id AND m.acked_at IS NULL AND m.held_reason IS NULL
				WHERE p.state = 'gone' GROUP BY p.id`, nil,
				func(c []string) string { return fmt.Sprintf("gone with %s unacked messages, oldest %s", c[1], c[2]) }},
			{"held", `SELECT COALESCE((SELECT json_extract(ev.payload,'$.rule_id') FROM events ev
					WHERE ev.type = 'held' AND ev.ref_id = COALESCE(m.cc_of, m.id) ORDER BY ev.seq DESC LIMIT 1),''),
				COUNT(*), MIN(m.id) FROM messages m WHERE m.held_reason IS NOT NULL GROUP BY 1`, nil,
				func(c []string) string { return fmt.Sprintf("%s messages held by %s, oldest %s", c[1], c[0], c[2]) }},
			{"headless_no_process", `SELECT p.id, p.run_id, p.state,
					CASE WHEN r.participant_id IS NULL THEN 'no process row' ELSE 'process exited' END
				FROM participants p LEFT JOIN processes r ON r.participant_id = p.id AND r.run_id = p.run_id
				WHERE p.mode = 'headless' AND p.state NOT IN ('gone','parked','requested')
					AND (r.participant_id IS NULL OR r.exited_at IS NOT NULL)`, nil,
				func(c []string) string { return fmt.Sprintf("state %s in run %s but %s", c[2], c[1], c[3]) }},
			{"parked", `SELECT id, run_id, strftime('%Y-%m-%dT%H:%M:%SZ', state_since/1000, 'unixepoch') FROM participants WHERE state = 'parked'`, nil,
				func(c []string) string { return fmt.Sprintf("parked since %s (run %s)", c[2], c[1]) }},
			// An adapter out of step with the daemon: a field one side added is dropped.
			{"protocol", `SELECT id, run_id, protocol_version, COALESCE(harness,'') FROM participants
				WHERE state <> 'gone' AND protocol_version IS NOT NULL AND protocol_version <> ?`, []any{ProtocolVersion},
				func(c []string) string {
					return fmt.Sprintf("its %s adapter speaks protocol %s, the daemon %d: run `piggery setup %s` and restart the session",
						cmp.Or(c[3], "harness"), c[2], ProtocolVersion, cmp.Or(c[3], "<harness>"))
				}},
		}
		for _, c := range checks {
			rows, err := t.QueryContext(t.ctx, c.query, c.args...)
			if err != nil {
				return internal(fmt.Errorf("doctor %s: %w", c.kind, err))
			}
			n, _ := rows.Columns()
			for rows.Next() {
				vals := make([]sql.NullString, len(n))
				ptrs := make([]any, len(n))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					rows.Close()
					return internal(err)
				}
				cols := make([]string, len(n))
				for i, v := range vals {
					cols[i] = v.String
				}
				out.Findings = append(out.Findings, Finding{Kind: c.kind, Detail: c.detail(cols), IDs: findingIDs(c.kind, cols)})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return internal(err)
			}
		}
		return nil
	})
	return out, err
}

// findingIDs picks the ids a finding points at: the participant (or run) and the message.
func findingIDs(kind string, cols []string) []string {
	switch kind {
	case "batch_order":
		return []string{cols[0]}
	case "gone_unacked":
		return []string{cols[0], cols[2]}
	case "held":
		return []string{cols[2]}
	}
	return []string{cols[0], cols[1]}
}
