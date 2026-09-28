package core

import "context"

// Inbox views: read-only reads of mail the caller may see. A view records no delivery, opens or
// touches no batch, and never acks.
const ViewBoard = "board" // the live pins of the caller's team

// viewLimit is the default number of messages a view returns (the latest ones, in seq order).
const viewLimit = 50

func (e *Engine) inboxView(ctx context.Context, c Caller, a InboxArgs) ([]Delivered, error) {
	if a.Batch != nil {
		return nil, errf(CodeInvalid, "a view is read-only: it takes no batch")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = viewLimit
	}
	var msgs []Message
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "inbox", "inbox")
		if err != nil {
			return err
		}
		switch a.View {
		case ViewBoard:
			msgs, err = t.livePins(p)
		default:
			return errf(CodeInvalid, "unknown inbox view %q (board)", a.View)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	out := make([]Delivered, len(msgs))
	for i, m := range msgs {
		out[i] = Delivered{Message: m}
	}
	return out, nil
}
