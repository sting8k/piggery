package core

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Send stores one message after the gate. A repeated (from, client_msg_id) returns the
// original id and writes nothing. A message over a declared mail limit is stored but held
// (limits.go); the recipient is not woken for it. The caller's role must have the send tool.
func (e *Engine) Send(ctx context.Context, c Caller, a SendArgs) (SendResult, error) {
	return e.send(ctx, c, a)
}

// messageRef turns "#N" (a message's global seq: what models and humans see) into the message id;
// a bare N is the same (models drop the "#"; an id is never all digits). Anything else is returned
// as is. An unknown #N is not_found naming it.
func (t *txn) messageRef(ref string) (string, error) {
	n, ok := strings.CutPrefix(ref, "#")
	if !ok && (ref == "" || strings.Trim(ref, "0123456789") != "") {
		return ref, nil
	}
	seq, err := strconv.ParseInt(n, 10, 64)
	if err != nil || seq <= 0 {
		return "", errf(CodeInvalid, "bad message ref %q: want #N or a message id", ref)
	}
	var id string
	err = t.QueryRowContext(t.ctx, `SELECT id FROM messages WHERE seq=?`, seq).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errf(CodeNotFound, "no message %s", ref)
	}
	if err != nil {
		return "", internal(err)
	}
	return id, nil
}

func (e *Engine) send(ctx context.Context, c Caller, a SendArgs) (SendResult, error) {
	var res SendResult
	var recipient string // participant to notify after commit
	var ccTo []string    // cc copy recipients to notify after commit
	var noticeTo string  // sender, when a hold notice was written
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.caller(c, "send")
		if err != nil {
			return err
		}
		if a.ReplyTo, err = t.messageRef(a.ReplyTo); err != nil {
			return err
		}
		if a.Target, err = t.messageRef(a.Target); err != nil {
			return err
		}
		if a.ClientMsgID != "" {
			var held sql.NullString
			err := t.QueryRowContext(t.ctx, `SELECT id, seq, held_reason FROM messages
				WHERE from_id=? AND client_msg_id=?`, p.id, a.ClientMsgID).Scan(&res.ID, &res.Seq, &held)
			if err == nil {
				res.Duplicate, res.Held = true, held.Valid
				if held.Valid {
					err := t.QueryRowContext(t.ctx, `SELECT COALESCE(json_extract(payload,'$.rule_id'),'') FROM events
						WHERE type='held' AND ref_id=? ORDER BY seq DESC LIMIT 1`, res.ID).Scan(&res.RuleID)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return internal(err)
					}
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return internal(err)
			}
		}
		m, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		res.ID = newID(t.now)
		g, err := t.sendGate(p, m, a, nil)
		if err != nil {
			return err
		}
		res.RuleID, recipient = g.rule, g.recipient
		if res.Seq, err = t.insertMessage(res.ID, a.ClientMsgID, p.team, p.id, g.toID, a.Kind,
			a.ReplyTo, a.Op, a.Target, a.Body); err != nil {
			return err
		}
		if ccTo, err = t.writeCCCopies(p, g.cc, res.ID, a.Kind, a.Body, res.RuleID != ""); err != nil {
			return err
		}
		if g.noGate {
			res.Held, res.RuleID = true, heldNoGate
			return t.holdNoGate(p, res.ID, g.toID)
		}
		if res.RuleID == "" {
			return nil
		}
		res.Held = true
		noticed, err := t.hold(p, res.ID, g.toID, res.RuleID, g.key)
		if noticed {
			noticeTo = p.id
		}
		return err
	})
	if err != nil {
		return SendResult{}, err
	}
	if !res.Duplicate && !res.Held && recipient != "" {
		e.notifyAfterCommit(recipient)
	}
	if !res.Held {
		for _, id := range ccTo {
			e.notifyAfterCommit(id)
		}
	}
	if noticeTo != "" {
		e.notifyAfterCommit(noticeTo)
	}
	return res, nil
}

// gatePlan is what the send gate decided for one message.
type gatePlan struct {
	toID, recipient string // recipient: participant to wake ("" for notify/board)
	rule, key       string // limit that holds it ("" = none) and its notice dedupe key
	cc              []participant
	crossTeam       bool // to another team's gate: no routing, no cc
	noGate          bool // to a member that left while its team has no gate: held (leave.go)
}

// gateTrace records each check of the send gate, for `why`. A nil trace records nothing.
type gateTrace struct{ checks []GateCheck }

func (tr *gateTrace) add(check, result, rule, detail string) {
	if tr != nil {
		tr.checks = append(tr.checks, GateCheck{Check: check, Result: result, RuleID: rule, Detail: detail})
	}
}

// sendGate is the gate of a send by p (token/run already checked): the role's grant of send,
// target and visibility, routing, reply_to, mail limits and routing cc. It writes nothing except
// through the returned denial. Send and Why both call it, so `why` reports the decision Send would make.
func (t *txn) sendGate(p participant, m manifest, a SendArgs, tr *gateTrace) (gatePlan, error) {
	var g gatePlan
	if err := m.granted(p, "send", "send"); err != nil {
		return g, err
	}
	tr.add("permission", "pass", "tools", "role "+p.role+" has tool send")
	var toRole string
	switch a.To {
	case AddrBoard:
		if err := t.gateBoard(p, m, a); err != nil {
			return g, err
		}
		tr.add("board", "pass", "can_pin", "role "+p.role+" can pin; target/board limit ok")
		g.toID = AddrBoard
	default:
		if a.Target != "" || (a.Op != "" && a.Op != OpAssign) {
			return g, errf(CodeInvalid, "target and op replace/remove are only valid for board; op assign for a member")
		}
		if a.To == AddrNotify { // the engine's channel: no routing rule opens it
			return g, deny(&p, "send", "routing", "routing.notify", "notify is the engine's channel: agents cannot send to it", nil,
				map[string]any{"to": a.To})
		}
		if a.Body == "" {
			return g, errf(CodeInvalid, "body is required")
		}
		q, other, err := t.resolveSendTarget(p, a.To)
		if err != nil {
			return g, err
		}
		g.toID, toRole, g.recipient = q.id, q.role, q.id
		if other != nil && a.Op == OpAssign {
			return g, errf(CodeInvalid, "op assign is for a member of your team")
		}
		if a.Op == OpAssign && q.reportsTo != p.id {
			return g, deny(&p, "send", "permission", "assign.not_reports_to", "only the member's reports_to can assign it a task", nil,
				map[string]any{"to": a.To})
		}
		if other != nil {
			// Another team: gate to gate only; neither team's routing nor cc applies.
			if err := t.gateCrossTeam(p, q, *other, a); err != nil {
				return g, err
			}
			tr.add("team_gate", "pass", "", fmt.Sprintf("%s is the gate of its team and %s the gate of team %s", p.name, a.To, other.name))
			g.crossTeam = true
			break
		}
		if g.noGate, err = t.gateLeaver(p, q, a); err != nil {
			return g, err
		}
		tr.add("visibility", "pass", "", fmt.Sprintf("%s is %s, role %s, in team %s", a.To, q.id, q.role, p.team))
		rule, ok := m.route(p.role, toRole)
		if !ok {
			return g, deny(&p, "send", "routing", rule, "routing does not allow "+p.role+" -> "+toRole, nil,
				map[string]any{"to": a.To})
		}
		tr.add("routing", "pass", rule, "allows "+p.role+" -> "+toRole)
	}
	if a.ReplyTo != "" {
		var fromID, parentTo string
		// Mail from or to the caller is in view whatever its team (cross-team mail keeps the
		// sender's team); a pin only on the caller's own board.
		var team string
		err := t.QueryRowContext(t.ctx, `SELECT from_id, to_id, COALESCE(team_id,'') FROM messages WHERE id=?`,
			a.ReplyTo).Scan(&fromID, &parentTo, &team)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && fromID != p.id && parentTo != p.id && (parentTo != AddrBoard || team != p.team)) {
			return g, deny(&p, "send", "visibility", "reply_to.not_in_view", "reply_to is not in the caller's view", nil,
				map[string]any{"reply_to": a.ReplyTo})
		}
		if err != nil {
			return g, internal(err)
		}
		tr.add("reply_to", "pass", "", "in view")
	}
	if a.To == AddrBoard {
		return g, nil // board pins are exempt from mail limits (the board has its own) and cc
	}
	// Limits are checked before the insert (counts exclude this message).
	var err error
	if g.rule, g.key, err = t.mailHold(p, m); err != nil {
		return g, err
	}
	if g.rule != "" {
		tr.add("limits", "hold", g.rule, "over the limit now: stored but held until an admin releases it")
	} else {
		tr.add("limits", "pass", "", "no declared mail limit is reached")
	}
	if g.crossTeam {
		return g, nil
	}
	if g.cc, err = t.ccRecipients(p, m, toRole, g.toID); err != nil {
		return g, err
	}
	if len(g.cc) > 0 {
		names := make([]string, len(g.cc))
		for i, q := range g.cc {
			names[i] = q.name
		}
		tr.add("cc", "info", "", "copies to "+strings.Join(names, ", "))
	}
	return g, nil
}

// insertMessage writes a message with the next daemon-assigned seq.
func (t *txn) insertMessage(id, clientMsgID, team, from, to, kind, replyTo, op, target, body string) (int64, error) {
	var seq int64
	if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM messages`).Scan(&seq); err != nil {
		return 0, internal(err)
	}
	// Mail to notify is done once stored (its hook runs after commit): acked at insert, never
	// delivered or waiting to be read.
	var acked any
	if to == AddrNotify {
		acked = t.now
	}
	_, err := t.ExecContext(t.ctx, `INSERT INTO messages
		(id, seq, client_msg_id, team_id, from_id, to_id, kind, reply_to, op, target, body, created_at, acked_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, seq, nullStr(clientMsgID), nullStr(team), from, to, nullStr(kind), nullStr(replyTo),
		nullStr(op), nullStr(target), body, t.now, acked)
	return seq, internal(err)
}

// Inbox returns the caller's unacked messages and records one delivery per message.
// Pull (Batch nil) deliveries have batch_seq NULL and can never be acked.
func (e *Engine) Inbox(ctx context.Context, c Caller, a InboxArgs) ([]Delivered, error) {
	if a.View != "" {
		return e.inboxView(ctx, c, a)
	}
	var out []Delivered
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.caller(c, "inbox")
		if err != nil {
			return err
		}
		var batch any // nil = pull
		if a.Batch == nil {
			// The model's inbox during a turn the daemon counts (adapter events): the turn's batch.
			b, ok, err := t.openTurnBatch(p)
			if err != nil {
				return err
			}
			if ok {
				batch = b.n
			}
		} else {
			n := *a.Batch
			if n < 1 {
				return errf(CodeInvalid, "batch must be >= 1")
			}
			if err := t.openBatch(p, n); err != nil {
				return err
			}
			batch = n
		}
		msgs, err := t.readMessages(p, `to_id=? AND acked_at IS NULL AND held_reason IS NULL`, p.id)
		if err != nil {
			return err
		}
		out = make([]Delivered, 0, len(msgs))
		for _, m := range msgs {
			var did int64
			if batch != nil {
				err := t.QueryRowContext(t.ctx, `SELECT id FROM deliveries
					WHERE run_id=? AND batch_seq=? AND message_id=? AND acked_at IS NULL`, p.run, batch, m.ID).Scan(&did)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return internal(err)
				}
			}
			if did == 0 {
				r, err := t.ExecContext(t.ctx, `INSERT INTO deliveries(message_id, run_id, batch_seq, delivered_at)
					VALUES (?,?,?,?)`, m.ID, p.run, batch, t.now)
				if err != nil {
					return internal(err)
				}
				if did, err = r.LastInsertId(); err != nil {
					return internal(err)
				}
			}
			again, err := t.redelivered(m.ID, did)
			if err != nil {
				return err
			}
			out = append(out, Delivered{DeliveryID: did, Redelivered: again, Message: m})
		}
		return nil
	})
	return out, err
}

// redelivered reports whether message id has a delivery other than delivery did: it was given
// before (and, being still unacked, is given again).
func (t *txn) redelivered(id string, did int64) (bool, error) {
	var again bool
	err := t.QueryRowContext(t.ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE message_id=? AND id<>?)`, id, did).Scan(&again)
	return again, internal(err)
}

// openBatch admits deliveries into (run, n): closed -> batch_closed; a new n below the
// run's highest batch -> batch_order.
func (t *txn) openBatch(p participant, n int64) error {
	var completed sql.NullInt64
	err := t.QueryRowContext(t.ctx, `SELECT completed_at FROM batches WHERE run_id=? AND batch_seq=?`,
		p.run, n).Scan(&completed)
	if err == nil {
		if completed.Valid {
			return denyAs(CodeBatchClosed, &p, "inbox", "target", "batch.closed", fmt.Sprintf("batch %d is closed", n), nil,
				map[string]any{"batch": n})
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return internal(err)
	}
	var highest int64
	if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(MAX(batch_seq),0) FROM batches WHERE run_id=?`,
		p.run).Scan(&highest); err != nil {
		return internal(err)
	}
	if n < highest {
		return denyAs(CodeBatchOrder, &p, "inbox", "target", "batch.order",
			fmt.Sprintf("batch %d is below the run's highest batch %d", n, highest), nil,
			map[string]any{"batch": n, "highest": highest})
	}
	if _, err := t.ExecContext(t.ctx, `INSERT INTO batches(run_id, batch_seq, opened_at) VALUES (?,?,?)`,
		p.run, n, t.now); err != nil {
		return internal(err)
	}
	return nil
}

// Completion closes (run, batch) and acks exactly its open deliveries: the CLI participant's
// ack after `inbox --batch N` (adapters end turns with harness.event). Only the caller's current
// run can complete; an already-closed batch acks nothing.
func (e *Engine) Completion(ctx context.Context, c Caller, a CompletionArgs) (CompletionResult, error) {
	res := CompletionResult{Acked: []string{}}
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.caller(c, "completion")
		if err != nil {
			return err
		}
		if a.Batch < 1 {
			return errf(CodeInvalid, "batch must be >= 1")
		}
		res.Acked, err = t.complete(p, a.Batch)
		return err
	})
	if err != nil {
		return CompletionResult{}, err
	}
	return res, nil
}

// complete closes (p's run, n) and acks exactly its open deliveries; an already-closed batch acks
// nothing. It returns the message ids acked.
func (t *txn) complete(p participant, n int64) ([]string, error) {
	acked := []string{}
	r, err := t.ExecContext(t.ctx, `INSERT INTO batches(run_id, batch_seq, opened_at, completed_at) VALUES (?,?,?,?)
		ON CONFLICT(run_id, batch_seq) DO UPDATE SET completed_at=excluded.completed_at WHERE completed_at IS NULL`,
		p.run, n, t.now, t.now)
	if err != nil {
		return nil, internal(err)
	}
	if n, err := r.RowsAffected(); err != nil || n == 0 {
		return nil, internal(err) // already closed: nothing to ack
	}
	rows, err := t.QueryContext(t.ctx, `SELECT id, message_id FROM deliveries
		WHERE run_id=? AND batch_seq=? AND acked_at IS NULL ORDER BY id`, p.run, n)
	if err != nil {
		return nil, internal(err)
	}
	type dm struct {
		did int64
		mid string
	}
	var ds []dm
	for rows.Next() {
		var d dm
		if err := rows.Scan(&d.did, &d.mid); err != nil {
			rows.Close()
			return nil, internal(err)
		}
		ds = append(ds, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	for _, d := range ds {
		if _, err := t.ExecContext(t.ctx, `UPDATE deliveries SET acked_at=? WHERE id=?`, t.now, d.did); err != nil {
			return nil, internal(err)
		}
		// The delivery is closed either way; the message is acked (and reported) only once,
		// even when another batch already acked it.
		// Only mail still addressed to the caller: mail moved to a new gate (leave.go) is
		// the gate's to ack.
		r, err := t.ExecContext(t.ctx, `UPDATE messages SET acked_at=? WHERE id=? AND to_id=? AND acked_at IS NULL`,
			t.now, d.mid, p.id)
		if err != nil {
			return nil, internal(err)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return nil, internal(err)
		}
		if n == 0 {
			continue
		}
		acked = append(acked, d.mid)
	}
	return acked, nil
}

// Who lists participants in the caller's view (step 1: same team).
func (e *Engine) Who(ctx context.Context, c Caller) ([]Presence, error) {
	var out []Presence
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "who", "who")
		if err != nil {
			return err
		}
		out = []Presence{}
		var root string // the caller's team root ("" for a solo)
		if p.team == "" {
			var cwd string
			if err := t.QueryRowContext(t.ctx, `SELECT cwd FROM participants WHERE id=?`, p.id).Scan(&cwd); err != nil {
				return internal(err)
			}
			out = append(out, Presence{Kind: WhoSolo, ID: p.id, Name: p.name, State: p.state, StateSince: p.stateSince,
				LastActivity: p.lastActivity, Cwd: cwd, Gate: true})
		} else {
			var team string
			if err := t.QueryRowContext(t.ctx, `SELECT name, root_cwd FROM teams WHERE id=?`, p.team).Scan(&team, &root); err != nil {
				return internal(err)
			}
			gate, _, err := t.teamGate(p.team)
			if err != nil {
				return err
			}
			rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+` FROM participants WHERE team_id=? ORDER BY `+joinOrder, p.team)
			if err != nil {
				return internal(err)
			}
			for rows.Next() {
				q, err := scanParticipant(rows)
				if err != nil {
					rows.Close()
					return internal(err)
				}
				out = append(out, Presence{Kind: WhoMember, ID: q.id, Name: q.name, Role: q.role, State: q.state,
					StateSince: q.stateSince, LastActivity: q.lastActivity, Team: team, Gate: q.id == gate.id})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return internal(err)
			}
		}
		others, err := t.relatedTeams(p.team)
		if err != nil {
			return err
		}
		for _, tm := range others {
			var cwd string
			if err := t.QueryRowContext(t.ctx, `SELECT root_cwd FROM teams WHERE id=?`, tm.id).Scan(&cwd); err != nil {
				return internal(err)
			}
			gate, _, err := t.teamGate(tm.id)
			if err != nil {
				return err
			}
			out = append(out, Presence{Kind: WhoTeam, ID: tm.id, Name: tm.name, Cwd: cwd, GateName: gate.name})
		}
		rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+`, cwd FROM participants
			WHERE team_id IS NULL AND state<>'gone' AND id<>? ORDER BY created_at, rowid`, p.id)
		if err != nil {
			return internal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var cwd string
			q, err := scanParticipant(rows, &cwd)
			if err != nil {
				return internal(err)
			}
			out = append(out, Presence{Kind: WhoSolo, ID: q.id, Name: q.name, State: q.state, StateSince: q.stateSince,
				LastActivity: q.lastActivity, Cwd: cwd, Gate: true, Admittable: root != "" && cwd == root})
		}
		return internal(rows.Err())
	})
	return out, err
}

// readMessages returns messages matching where (ordered by seq) with sender labels for reader.
func (t *txn) readMessages(reader participant, where string, args ...any) ([]Message, error) {
	// where uses unqualified columns of messages; the cc original is read by a subquery.
	rows, err := t.QueryContext(t.ctx, `SELECT id, seq, from_id, to_id, COALESCE(kind,''),
		COALESCE(reply_to,''), COALESCE(op,''), COALESCE(target,''), body, created_at,
		COALESCE(cc_of,''), COALESCE((SELECT o.to_id FROM messages o WHERE o.id = messages.cc_of),''),
		COALESCE((SELECT o.seq FROM messages o WHERE o.id = messages.reply_to),0),
		COALESCE((SELECT o.seq FROM messages o WHERE o.id = messages.cc_of),0)
		FROM messages WHERE `+where+` ORDER BY seq`, args...)
	if err != nil {
		return nil, internal(err)
	}
	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Seq, &m.From, &m.To, &m.Kind, &m.ReplyTo,
			&m.Op, &m.Target, &m.Body, &m.CreatedAt, &m.CcOf, &m.CcTo, &m.ReplyToSeq, &m.CcOfSeq); err != nil {
			rows.Close()
			return nil, internal(err)
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	known := map[string]participant{AddrEngine: {id: AddrEngine}}
	who := func(id string) (participant, error) {
		if q, ok := known[id]; ok {
			return q, nil
		}
		q, found, err := t.participantByID(id)
		if err != nil {
			return q, err
		}
		if !found {
			q = participant{id: id, name: id}
		}
		known[id] = q
		return q, nil
	}
	for i := range msgs {
		s, err := who(msgs[i].From)
		if err != nil {
			return nil, err
		}
		msgs[i].FromLabel, msgs[i].FromName = label(reader, s), cmp.Or(s.name, s.id)
		// Mail from another team names it (the notify hook gets the team separately).
		if s.team != "" && s.team != reader.team && reader.id != AddrNotify {
			var team string
			if err := t.QueryRowContext(t.ctx, `SELECT name FROM teams WHERE id=?`, s.team).Scan(&team); err != nil {
				return nil, internal(err)
			}
			msgs[i].FromLabel = s.name + " (" + s.role + ", team " + team + ")"
		}
		if msgs[i].CcOf != "" { // a cc copy: the original recipient, from the reader's view
			o, err := who(msgs[i].CcTo)
			if err != nil {
				return nil, err
			}
			msgs[i].CcTo = label(reader, o)
		}
	}
	return msgs, nil
}
