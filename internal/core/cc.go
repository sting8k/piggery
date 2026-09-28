package core

// Routing cc: an allowed send whose routing rule has `cc: [roles]` also writes one copy per member
// of those roles, except the sender and the recipient. A copy has the same thread, kind and body,
// expects_reply 0, no reply_to (it is not in the reply chain), and cc_of = the original. Copies do
// not count toward the sender's rate or the thread cap, and share the original's hold: held with
// it, released with it.

// ccRoles returns the cc roles of the first routing rule matching (from, to).
func (m manifest) ccRoles(from, to string) []string {
	for _, r := range m.Routing {
		if r.From == from && r.To == to {
			return r.CC
		}
	}
	return nil
}

// ccRecipients returns the members of the cc roles of the rule for (p.role, toRole), except
// the sender and the recipient, each once.
func (t *txn) ccRecipients(p participant, m manifest, toRole, toID string) ([]participant, error) {
	seen := map[string]bool{p.id: true, toID: true}
	var out []participant
	for _, role := range m.ccRoles(p.role, toRole) {
		members, err := t.teamRole(p.team, role)
		if err != nil {
			return nil, err
		}
		for _, q := range members {
			if !seen[q.id] {
				seen[q.id] = true
				out = append(out, q)
			}
		}
	}
	return out, nil
}

// writeCCCopies writes the copies of message orig (already inserted) to cc and returns their
// ids, to wake after commit unless held.
func (t *txn) writeCCCopies(p participant, cc []participant, orig, thread, kind, body string, held bool) ([]string, error) {
	var recipients []string
	for _, q := range cc {
		id := newID(t.now)
		if _, err := t.insertMessage(id, "", p.team, p.id, q.id, kind, thread, "", false, "", "", body); err != nil {
			return nil, err
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE messages SET cc_of=?, held_reason=? WHERE id=?`,
			orig, nullStrIf(held, heldPolicy), id); err != nil {
			return nil, internal(err)
		}
		recipients = append(recipients, q.id)
	}
	return recipients, nil
}

func nullStrIf(cond bool, s string) any {
	if cond {
		return s
	}
	return nil
}
