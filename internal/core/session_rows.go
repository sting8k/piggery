package core

// One harness session can have several participant rows: the member row of a team that closed, and
// the solo row the session joined again with. Team down keeps
// the member row as it was (the session's own reopen gives it back), so the rows meet again only when
// the team reopens: dropSuperseded ends the old one then, and Reconcile checks it for open teams.
//
// sharesSession is the condition "row o is of the session that participant ?1 is of": o has one of its
// refs (?2 harness_ref, ?3 session_ref), or participant_refs ties them. Use it inside a query that
// names the table as o.
const sharesSession = `(o.harness_ref IN (NULLIF(?2,''), NULLIF(?3,'')) OR o.session_ref IN (NULLIF(?2,''), NULLIF(?3,''))
	OR o.id IN (SELECT participant_id FROM participant_refs WHERE ref IN (?2, ?3))
	OR o.harness_ref IN (SELECT ref FROM participant_refs WHERE participant_id=?1)
	OR o.session_ref IN (SELECT ref FROM participant_refs WHERE participant_id=?1))`

// dropSuperseded marks as left every person's row of team that has not left and whose session has a
// newer row (inserted later) that has not left either, outside a closed team (or in this one: the row that reopened
// it). Such a row is the stale one: the session went on as the newer row, so it must neither be woken
// nor listed as a second member. Its mail and workers move to the gate with the team's other leavers
// (rerouteToGate). It returns how many rows were dropped.
func (t *txn) dropSuperseded(team string) (int, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT id, COALESCE(harness_ref,''), COALESCE(session_ref,''), rowid
		FROM participants WHERE team_id=? AND left_at IS NULL AND person=1`, team)
	if err != nil {
		return 0, internal(err)
	}
	type row struct {
		id, ref, sessionRef string
		rowid               int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ref, &r.sessionRef, &r.rowid); err != nil {
			rows.Close()
			return 0, internal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, internal(err)
	}
	n := 0
	for _, r := range all {
		var newer bool
		if err := t.QueryRowContext(t.ctx, `SELECT EXISTS (SELECT 1 FROM participants o WHERE o.id<>?1 AND o.left_at IS NULL
			AND o.rowid>?4 AND (o.team_id IS NULL OR o.team_id IN (SELECT id FROM teams WHERE closed_at IS NULL))
			AND `+sharesSession+`)`, r.id, r.ref, r.sessionRef, r.rowid).Scan(&newer); err != nil {
			return n, internal(err)
		}
		if !newer {
			continue
		}
		p, ok, err := t.participantByID(r.id)
		if err != nil || !ok {
			return n, err
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE participants SET left_at=?, run_id=? WHERE id=?`, t.now, newID(t.now), r.id); err != nil {
			return n, internal(err)
		}
		if err := t.setState(&p, "gone", "superseded"); err != nil {
			return n, err
		}
		if err := t.event(evt{typ: "left", participant: r.id, team: team, ref: r.id,
			payload: map[string]any{"superseded": true}}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
