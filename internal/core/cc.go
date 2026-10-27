package core

import (
	"errors"
	"strings"
)

// Cc: an allowed send also writes one copy per member of the cc roles of its routing rule
// (`cc: [roles]`) and per name the sender chose (SendArgs.CC), except the sender and the
// recipient, each once. A copy has the same kind and body, no reply_to (it is not in the reply
// chain) and no op, and cc_of = the original. Copies do not count toward the sender's rate, and
// share the original's hold: held with it, released with it. A reader of the
// original or of a copy sees the other copies' recipients (Message.Cc); all are members of the
// original's team, so they are in view.
//
// A name the sender chose is checked as a send to it would be (target, visibility, routing). One
// that would be refused, or that is no teammate (another team or solo: mail between teams is gate
// to gate, one recipient), is left out and told to the sender (SendResult.CcRefused); it never
// fails the send.

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

// senderCC resolves the names the sender chose: the participants that get a copy, and the names
// refused. crossTeam: the send goes to another team's gate, which takes no cc, so every name is.
func (t *txn) senderCC(p participant, m manifest, a SendArgs, toID string, crossTeam bool) (ok []participant, refused []CcRefusal, err error) {
	seen := map[string]bool{}
	for _, name := range a.CC {
		if name = strings.TrimSpace(name); name == "" || seen[name] {
			continue
		}
		seen[name] = true
		refuse := func(rule, reason string) {
			refused = append(refused, CcRefusal{To: name, RuleID: rule, Reason: reason})
		}
		if crossTeam {
			refuse("cc.cross_team", "mail to another team's gate goes to it alone, no cc")
			continue
		}
		q, other, err := t.resolveSendTarget(p, name)
		if e, denied := refusal(err); denied {
			refuse(e.RuleID, e.Message)
			continue
		} else if err != nil {
			return nil, nil, err
		}
		if other != nil {
			refuse("cc.cross_team", name+" is not in your team; mail between teams goes gate to gate, to one recipient")
			continue
		}
		if q.id == p.id || q.id == toID {
			continue // the sender and the recipient have it already
		}
		noGate, err := t.gateLeaver(p, q, SendArgs{To: name})
		if e, denied := refusal(err); denied {
			refuse(e.RuleID, e.Message)
			continue
		} else if err != nil {
			return nil, nil, err
		}
		if noGate {
			refuse("team.left", name+" left the team")
			continue
		}
		if rule, allow := m.route(p.role, q.role); !allow {
			refuse(rule, "routing does not allow "+p.role+" -> "+q.role)
			continue
		}
		ok = append(ok, q)
	}
	return ok, refused, nil
}

// refusal is the gate refusal inside err, if it is one.
func refusal(err error) (*Error, bool) {
	var d *denial
	if errors.As(err, &d) {
		return d.err, true
	}
	return nil, false
}

// writeCCCopies writes the copies of message orig (already inserted) to cc and returns their
// ids, to wake after commit unless held.
func (t *txn) writeCCCopies(p participant, cc []participant, orig, kind, body string, held bool) ([]string, error) {
	var recipients []string
	for _, q := range cc {
		id := newID(t.now)
		if _, err := t.insertMessage(id, "", p.team, p.id, q.id, kind, "", "", body); err != nil {
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
