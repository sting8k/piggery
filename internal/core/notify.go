package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The reserved address `notify` is the engine's notification channel: only the engine writes to it
// (agents cannot send there, and manifest lines naming it are ignored: notifyWarnings). A message to
// notify is stored already acked (limits, dedupe and hops still see it), never delivered, and a
// post-commit sink (the server runs the hooks in ~/.piggery/hooks/notify.d) is told about it. There is no inbox,
// ack, or reply path for it.

// WithNotifySink sets the callback run after commit for each new message to `notify`. It must not block: the server runs its hook asynchronously.
func WithNotifySink(f func(messageID string)) Option { return func(e *Engine) { e.notifySink = f } }

func (e *Engine) notifyHookAfterCommit(messageID string) {
	if e.notifySink != nil {
		e.notifySink(messageID)
	}
}

// The kinds of notice, set by core only (the message's kind): what the operator is told.
const (
	noticeReply    = "reply"     // the gate answered a turn on team mail in its session, sending nothing
	noticeSettled  = "settled"   // the gate sent its last message and the team has nothing left to do
	noticeFailed   = "failed"    // the gate's turn on team mail failed: the mail is stuck
	noticeGateLost = "gate_lost" // the team has no live member (leave.go)
)

// NotifyMail is what the notify sink hook receives (one JSON line on stdin). Kind is one of the
// notice kinds above; Gate is the gate's name and Dir the team's root (a solo: its cwd). A gate_lost
// notice has no gate.
type NotifyMail struct {
	ID        string `json:"id"`
	FromLabel string `json:"from_label"`
	Team      string `json:"team"`
	Gate      string `json:"gate"`
	Dir       string `json:"dir"`
	Kind      string `json:"kind"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
}

// NotifyMail loads one message to notify for the sink.
func (e *Engine) NotifyMail(ctx context.Context, id string) (NotifyMail, error) {
	var out NotifyMail
	err := e.inTx(ctx, func(t *txn) (err error) {
		out, err = t.notifyMail(id)
		return err
	})
	return out, err
}

func (t *txn) notifyMail(id string) (NotifyMail, error) {
	out, err := t.notifyMails([]string{id})
	if err != nil {
		return NotifyMail{}, err
	}
	return out[0], nil
}

// notifyMails loads several messages to notify, in the order of ids, in four queries however many:
// the messages, their teams, and the gates that caused them (a notice is read on every ps).
func (t *txn) notifyMails(ids []string) ([]NotifyMail, error) {
	out := make([]NotifyMail, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	in, args := inList(ids)
	msgs, err := t.readMessages(participant{id: AddrNotify}, `to_id=? AND id IN (`+in+`)`, append([]any{AddrNotify}, args...)...)
	if err != nil {
		return nil, err
	}
	byID := map[string]*NotifyMail{}
	for _, m := range msgs {
		byID[m.ID] = &NotifyMail{ID: m.ID, FromLabel: m.FromLabel, Kind: m.Kind, Body: m.Body, CreatedAt: m.CreatedAt}
	}
	for _, id := range ids {
		if byID[id] == nil {
			return nil, errf(CodeNotFound, "no message %q to notify", id)
		}
	}
	rows, err := t.QueryContext(t.ctx, `SELECT m.id, COALESCE(t.name,''), COALESCE(t.root_cwd,'') FROM messages m
		LEFT JOIN teams t ON t.id=m.team_id WHERE m.id IN (`+in+`)`, args...)
	if err != nil {
		return nil, internal(err)
	}
	for rows.Next() {
		var id, team, dir string
		if err := rows.Scan(&id, &team, &dir); err != nil {
			rows.Close()
			return nil, internal(err)
		}
		if n := byID[id]; n != nil {
			n.Team, n.Dir = team, dir
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	// the gate that caused the notice (its notice event); a solo's directory is its cwd
	grows, err := t.QueryContext(t.ctx, `SELECT e.ref_id, p.name, p.cwd FROM events e JOIN participants p ON p.id=e.participant
		WHERE e.type='notice' AND e.ref_id IN (`+in+`) ORDER BY e.seq DESC`, args...)
	if err != nil {
		return nil, internal(err)
	}
	gates := map[string][2]string{}
	for grows.Next() {
		var ref string
		var gate, cwd sql.NullString
		if err := grows.Scan(&ref, &gate, &cwd); err != nil {
			grows.Close()
			return nil, internal(err)
		}
		gates[ref] = [2]string{gate.String, cwd.String} // newest first: the oldest ends up kept
	}
	grows.Close()
	if err := grows.Err(); err != nil {
		return nil, internal(err)
	}
	for i, id := range ids {
		n := *byID[id]
		g := gates[id]
		n.Gate = g[0]
		if n.Dir == "" {
			n.Dir = g[1]
		}
		out[i] = n
	}
	return out, nil
}

// gateNotice decides, at the end of p's turn (p is idle now, the batch n is the turn's), whether the
// operator is told, from state only and once: p must be the gate of its team, a
// session (not headless), or a solo. left is p's mail still pending after the turn. An ok turn:
// pending mail -> nothing (the engine wakes p again); no mail was given in the turn (the Human
// chatted) -> nothing; mail given and p sent nothing in the turn -> reply; p sent, the team is
// settled -> settled; else nothing (p is coordinating). Anything p writes into piggery counts as sent:
// it is for the team, and only chat text is for the Human. A failed turn given mail -> failed; an
// interrupted one -> nothing. It writes the notice and returns its id ("" = none) for the hook.
func (t *txn) gateNotice(p participant, n int64, outcome string, left int) (string, error) {
	if outcome == HarnessOutcomeIntr || (outcome == HarnessOutcomeOK && left > 0) {
		return "", nil
	}
	var mode, cwd sql.NullString
	if err := t.QueryRowContext(t.ctx, `SELECT mode, cwd FROM participants WHERE id=?`, p.id).Scan(&mode, &cwd); err != nil {
		return "", internal(err)
	}
	if mode.String == modeHeadless {
		return "", nil
	}
	who := p.name
	if p.team != "" {
		gate, ok, err := t.teamGate(p.team)
		if err != nil || !ok || gate.id != p.id {
			return "", err
		}
		var team string
		if err := t.QueryRowContext(t.ctx, `SELECT name FROM teams WHERE id=?`, p.team).Scan(&team); err != nil {
			return "", internal(err)
		}
		who += " (gate of " + team + ")"
	}
	var given int
	if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM deliveries WHERE run_id=? AND batch_seq=?`, p.run, n).Scan(&given); err != nil {
		return "", internal(err)
	}
	if given == 0 {
		return "", nil
	}
	kind, body := noticeFailed, who+" failed a turn on mail: the mail stays unacked until new mail arrives."
	var unseen int // a model turn that ended well read the rest, so it is acked
	if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM deliveries WHERE run_id=? AND batch_seq=? AND acked_at IS NULL`,
		p.run, n).Scan(&unseen); err != nil {
		return "", internal(err)
	}
	if unseen == 0 {
		body = who + " failed a turn on mail after reading it."
	}
	if outcome == HarnessOutcomeOK {
		var sent int
		if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM messages WHERE from_id=? AND created_at >=
			(SELECT opened_at FROM batches WHERE run_id=? AND batch_seq=?)`, p.id, p.run, n).Scan(&sent); err != nil {
			return "", internal(err)
		}
		switch {
		case sent == 0:
			kind, body = noticeReply, who+" finished a turn on mail and sent nothing: its answer is in its session."
		case p.team == "":
			return "", nil // a solo has no team to settle
		default:
			settled, err := t.teamSettled(p.team)
			if err != nil || !settled {
				return "", err
			}
			kind, body = noticeSettled, who+" sent its last message and the team is settled: nobody is working and no mail waits."
		}
	}
	id := newID(t.now)
	if _, err := t.insertMessage(id, "", p.team, AddrEngine, AddrNotify, kind, "", "", body); err != nil {
		return "", err
	}
	return id, t.event(evt{typ: "notice", participant: p.id, team: p.team, run: p.run, ref: id,
		payload: map[string]any{"kind": kind, "to": AddrNotify}})
}

// teamSettled: no member of the team, or of an open taskforce one of its members called (its answer
// is still to come), is requested, starting, working or awaiting a permission, and no mail waits
// (unacked, not held) for any of them that has not left, gone ones included: mail wakes a gone
// member, so its unread mail is a wake on its way or a refused one. The mail count is ps's
// (pendingMail).
func (t *txn) teamSettled(team string) (bool, error) {
	const inTeam = `(team_id=? OR team_id IN (SELECT id FROM teams WHERE closed_at IS NULL
		AND parent_id IN (SELECT id FROM participants WHERE team_id=?)))`
	var busy int
	if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM participants WHERE `+inTeam+`
		AND state IN ('requested','starting','working','awaiting_permission')`, team, team).Scan(&busy); err != nil || busy > 0 {
		return false, internal(err)
	}
	members := map[string]bool{}
	rows, err := t.QueryContext(t.ctx, `SELECT id FROM participants WHERE `+inTeam+` AND left_at IS NULL`, team, team)
	if err != nil {
		return false, internal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, internal(err)
		}
		members[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, internal(err)
	}
	mail, err := t.QueryContext(t.ctx, pendingMail)
	if err != nil {
		return false, internal(err)
	}
	defer mail.Close()
	for mail.Next() {
		var to string
		var unacked, held int
		if err := mail.Scan(&to, &unacked, &held); err != nil {
			return false, internal(err)
		}
		if members[to] && unacked > 0 {
			return false, nil
		}
	}
	return true, internal(mail.Err())
}

// maxErrorText is the longest error text a lead is told, in runes.
const maxErrorText = 300

// tellLead is what a failed turn does when gateNotice told the operator nothing (a headless worker, a
// member that is not its team's gate, a gate whose turn read no mail): one engine mail to the member
// p reports to, saying p is idle, its task and the harness's error, so the lead can mail it to go on.
// A failed turn does not wake itself, so without this nobody would know. A member that reports to
// nobody, or to a member that has left, has no one to tell. It returns the lead to wake ("" = none told); p is never woken.
// errText is shown as given, cut to one line (the harness's words: core never reads them).
func (t *txn) tellLead(p participant, errText string) (string, error) {
	if p.reportsTo == "" {
		return "", nil
	}
	var live int
	if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM participants WHERE id=? AND left_at IS NULL`, p.reportsTo).Scan(&live); err != nil || live == 0 {
		return "", internal(err) // a lead that left reads nothing
	}
	title := "none"
	var task string
	err := t.QueryRowContext(t.ctx, `SELECT body FROM messages WHERE to_id=? AND op=? AND held_reason IS NULL ORDER BY seq DESC LIMIT 1`,
		p.id, OpAssign).Scan(&task)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", internal(err)
	}
	if at := assignmentTitle(task); at != "" {
		title = at
	}
	text := strings.Join(strings.Fields(errText), " ")
	if r := []rune(text); len(r) > maxErrorText {
		text = string(r[:maxErrorText]) + "…"
	}
	if text == "" {
		text = "the harness gave no error text"
	}
	body := fmt.Sprintf("%s (%s) ended a turn on an error and is idle. Its task: %s.\nError: %s. Mail it to go on, or tail it.", p.name, p.role, title, text)
	if _, err := t.insertMessage(newID(t.now), "", p.team, AddrEngine, p.reportsTo, noticeFailed, "", "", body); err != nil {
		return "", err
	}
	return p.reportsTo, nil
}
