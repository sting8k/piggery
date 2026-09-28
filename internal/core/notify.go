package core

import "context"

// The reserved address `notify` is a notification channel: a message to notify is stored already
// acked (limits, dedupe and hops still see it), never delivered, and a post-commit sink (the server
// runs ~/.piggery/hooks/notify) is told about it. There is no inbox, ack, or reply path for it.

// WithNotifySink sets the callback run after commit for each new message to `notify` that is
// not held. It must not block: the server runs its hook asynchronously.
func WithNotifySink(f func(messageID string)) Option { return func(e *Engine) { e.notifySink = f } }

func (e *Engine) notifyHookAfterCommit(messageID string) {
	if e.notifySink != nil {
		e.notifySink(messageID)
	}
}

// NotifyMail is what the notify sink hook receives (one JSON line on stdin).
type NotifyMail struct {
	ID        string `json:"id"`
	FromLabel string `json:"from_label"`
	Team      string `json:"team"`
	Kind      string `json:"kind,omitempty"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
}

// NotifyMail loads one message to notify for the sink.
func (e *Engine) NotifyMail(ctx context.Context, id string) (NotifyMail, error) {
	var out NotifyMail
	err := e.inTx(ctx, func(t *txn) error {
		msgs, err := t.readMessages(participant{id: AddrNotify}, `id=? AND to_id=?`, id, AddrNotify)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return errf(CodeNotFound, "no message %q to notify", id)
		}
		m := msgs[0]
		out = NotifyMail{ID: m.ID, FromLabel: m.FromLabel, Kind: m.Kind, Body: m.Body, CreatedAt: m.CreatedAt}
		err = t.QueryRowContext(t.ctx, `SELECT COALESCE(t.name,'') FROM messages m LEFT JOIN teams t ON t.id=m.team_id
			WHERE m.id=?`, id).Scan(&out.Team)
		return internal(err)
	})
	return out, err
}
