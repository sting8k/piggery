package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The inbox tool's history views: an agent whose context lost its mail (compaction) looks back at
// its own. Read-only: no delivery is recorded, no batch opened or touched, nothing acked (an ack
// comes only from completion(run, batch)); the only write is the `denied` event of a refusal.
//
// What is mine: mail to me (cc copies and engine notices included, held mail not: showing it would
// bypass the hold) and my own originals (not my cc copies of them: the original names them in cc),
// held ones marked. Mail to or from me across teams is mine.

const (
	historyPage    = 10 // lines of a recent page
	historyPreview = 80 // runes of a body in a line
)

// History returns the text of the view a for the caller: View "recent", the newest historyPage
// messages first (Before: those older than that #N), or View "message", the one in ID.
func (e *Engine) History(ctx context.Context, c Caller, a HistoryArgs) (string, error) {
	var text string
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.caller(c, "inbox")
		if err != nil {
			return err
		}
		now := time.UnixMilli(t.now)
		switch {
		case a.View == "recent" && a.ID == "":
			text, err = t.historyRecent(p, a.Before, now)
		case a.View == "message" && a.Before == "":
			text, err = t.historyMessage(p, a.ID, now)
		default:
			return errf(CodeInvalid, `view is "recent" (optional before) or "message" (with id)`)
		}
		return err
	})
	return text, err
}

// mine is the where clause of messages in my history (readMessages' unqualified columns).
const mine = `((to_id=? AND held_reason IS NULL) OR (from_id=? AND cc_of IS NULL))`

func (t *txn) historyRecent(p participant, before string, now time.Time) (string, error) {
	where, args := `seq IN (SELECT seq FROM messages WHERE `+mine, []any{p.id, p.id}
	if before != "" {
		id, err := t.messageRef(before)
		if err != nil {
			return "", err
		}
		where += ` AND seq < (SELECT seq FROM messages WHERE id=?)`
		args = append(args, id)
	}
	msgs, err := t.readMessages(p, where+fmt.Sprintf(` ORDER BY seq DESC LIMIT %d)`, historyPage+1), args...)
	if err != nil {
		return "", err
	}
	more := len(msgs) > historyPage // the extra, oldest one only says there is a next page
	if more {
		msgs = msgs[1:]
	}
	if len(msgs) == 0 {
		if before != "" {
			return "No older messages.", nil
		}
		return "No messages.", nil
	}
	lines := make([]string, len(msgs))
	for i, m := range msgs { // newest first
		to, err := t.addressName(m.To)
		if err != nil {
			return "", err
		}
		lines[len(msgs)-1-i] = historyLine(p, m, to, now)
	}
	if more {
		lines = append(lines, fmt.Sprintf("Older: %sinbox with view=recent before=#%d. One message in full: view=message id=#N.",
			p.toolPrefix, msgs[0].Seq))
	}
	return strings.Join(lines, "\n"), nil
}

func (t *txn) historyMessage(p participant, ref string, now time.Time) (string, error) {
	if ref == "" {
		return "", errf(CodeInvalid, "id is required for view message")
	}
	id, err := t.messageRef(ref)
	if err != nil {
		return "", err
	}
	msgs, err := t.readMessages(p, `id=? AND `+mine, id, p.id, p.id)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", deny(&p, "inbox", "visibility", "message.not_in_view", "message is not in the caller's view", nil,
			map[string]any{"id": ref})
	}
	m := msgs[0]
	var extra string
	if m.From == p.id {
		to, err := t.addressName(m.To)
		if err != nil {
			return "", err
		}
		extra = fmt.Sprintf(` to="%s"`, to)
	}
	return renderMessage(Delivered{Message: m}, now, extra+marks(p, m)), nil
}

// marks are the history header attributes of a message: unread (mail to me not acked yet) and held
// (my own mail a limit holds).
func marks(p participant, m Message) string {
	var s string
	if m.To == p.id && m.unacked {
		s += ` unread="true"`
	}
	if m.held {
		s += ` held="true"`
	}
	return s
}

// historyLine is one message of a recent page: #N, when, direction, kind, reply, marks, preview.
func historyLine(p participant, m Message, to string, now time.Time) string {
	sent := time.UnixMilli(m.CreatedAt)
	ago := "just now"
	if d := now.Sub(sent); d >= time.Minute {
		ago = shortAge(d) + " ago"
	}
	b := fmt.Sprintf("#%d %s, %s", m.Seq, sentAt(sent, now), ago)
	if m.From == p.id {
		b += " → " + to
	} else {
		b += " ← " + m.FromName
	}
	if m.Kind != "" {
		b += " kind=" + m.Kind
	}
	if m.ReplyTo != "" {
		b += fmt.Sprintf(" re #%d", m.ReplyToSeq)
	}
	if m.To == p.id && m.unacked {
		b += " unread"
	}
	if m.held {
		b += " held"
	}
	body := []rune(strings.Join(strings.Fields(m.Body), " "))
	if len(body) > historyPreview {
		body = append(body[:historyPreview], '…')
	}
	return b + ": " + string(body)
}

// addressName is the name of a participant id, or the id itself for an address (notify, engine)
// or a participant that is gone from the table.
func (t *txn) addressName(id string) (string, error) {
	q, found, err := t.participantByID(id)
	if err != nil || !found || q.name == "" {
		return id, err
	}
	return q.name, nil
}
