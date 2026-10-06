package core

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Team down and gc of closed teams, kept safe: gc only touches closed teams, and deletes nothing
// it has not first archived to a file and read back.

type TeamDownArgs struct {
	Team string `json:"team"` // id or name
	// closedBy is the gate that closed its own team (agent close); "" = the admin.
	closedBy string
	// why is the daemon's reason when nobody closed it (caller_gone, start_failed): kept in the event.
	why string
}

type TeamDownResult struct {
	TeamID  string   `json:"team_id"`
	Stopped []string `json:"stopped"` // worker names stopped through the driver
	Failed  []string `json:"failed,omitempty"`
}

// TeamDown closes a team: closed_at, timers off, participants gone, one team_down event, all in
// one transaction; from then on Authenticate refuses its participants. Its live workers are then
// stopped through the driver (outside the tx, like agent stop). Nothing is acked. Calling it
// again on a closed team only retries workers still live (a failed stop).
func (e *Engine) TeamDown(ctx context.Context, a TeamDownArgs) (TeamDownResult, error) {
	var res TeamDownResult
	var workers []participant
	err := e.inTx(ctx, func(t *txn) error {
		var closed sql.NullInt64
		err := t.QueryRowContext(t.ctx, `SELECT id, closed_at FROM teams WHERE id=? OR name=?
			ORDER BY closed_at IS NOT NULL, created_at DESC LIMIT 1`, a.Team, a.Team).Scan(&res.TeamID, &closed)
		if errors.Is(err, sql.ErrNoRows) {
			return errf(CodeNotFound, "no team %q", a.Team)
		}
		if err != nil {
			return internal(err)
		}
		rows, err := t.QueryContext(t.ctx, `SELECT `+participantCols+`, COALESCE(mode,'') FROM participants
			WHERE team_id=? AND state<>'gone' ORDER BY `+joinOrder, res.TeamID)
		if err != nil {
			return internal(err)
		}
		defer rows.Close()
		var live []participant
		for rows.Next() {
			var mode string
			p, err := scanParticipant(rows, &mode)
			if err != nil {
				return internal(err)
			}
			if mode == modeHeadless {
				workers = append(workers, p)
			} else {
				live = append(live, p)
			}
		}
		if err := rows.Err(); err != nil {
			return internal(err)
		}
		if closed.Valid {
			if len(workers) == 0 {
				return errf(CodeInvalid, "team %s is already closed", res.TeamID)
			}
			return nil // retry the stops only
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE teams SET closed_at=? WHERE id=?`, t.now, res.TeamID); err != nil {
			return internal(err)
		}
		off, err := t.ExecContext(t.ctx, `UPDATE timers SET active=0 WHERE team_id=? AND active=1`, res.TeamID)
		if err != nil {
			return internal(err)
		}
		timersOff, _ := off.RowsAffected()
		for i := range live {
			if err := t.setState(&live[i], "gone", "team_down"); err != nil {
				return err
			}
		}
		by := "admin"
		if a.closedBy != "" {
			by = a.closedBy
		}
		if a.why != "" && a.closedBy == "" {
			by = a.why
		}
		return t.event(evt{typ: "team_down", team: res.TeamID, ref: res.TeamID, participant: a.closedBy,
			payload: map[string]any{"gone": len(live), "workers": len(workers), "timers_off": timersOff, "by": by}})
	})
	if err != nil {
		return TeamDownResult{}, err
	}
	for _, w := range workers {
		if err := e.stopForTeamDown(ctx, w); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", w.name, err))
			continue
		}
		res.Stopped = append(res.Stopped, w.name)
	}
	return res, nil
}

// stopForTeamDown stops one worker and marks it gone (stopWorker: a worker the driver does not
// hold is verified like reconcile; doubt leaves it as it is, and team down can be retried).
func (e *Engine) stopForTeamDown(ctx context.Context, w participant) error {
	if e.runtimeFor(w.harness) == nil {
		return errf(CodeUnsupported, "no runtime driver for harness %q", w.harness)
	}
	_, err := e.stopWorker(ctx, w, "team_down", false)
	return err
}

type GCArgs struct {
	ClosedBeforeMs int64 `json:"closed_before_ms"` // teams closed longer ago than this
	DryRun         bool  `json:"dry_run,omitempty"`
}

// GCTeam is what gc did with one closed team or, in GCResult.Solos, one gone solo (ParticipantID
// set, TeamID empty, Name its name, ClosedAt when it went gone).
type GCTeam struct {
	TeamID        string         `json:"team_id"`
	ParticipantID string         `json:"participant_id,omitempty"`
	Name          string         `json:"name"`
	ClosedAt      int64          `json:"closed_at"`
	Counts        map[string]int `json:"counts"`
	Archive       string         `json:"archive,omitempty"`
	Deleted       bool           `json:"deleted"`
	Skipped       string         `json:"skipped,omitempty"` // why this team was left as it is
	// LogDirs and LogBytes count the entries of its participants deleted with its rows, in the
	// run logs, run scratch and session data (GCPlace.Own): the name is from when it was only logs.
	LogDirs  int   `json:"log_dirs,omitempty"`
	LogBytes int64 `json:"log_bytes,omitempty"`
}

type GCResult struct {
	Teams []GCTeam `json:"teams"`
	// Solos are the sessions with no team gone longer than the same retention.
	Solos []GCTeam `json:"solos,omitempty"`
	// ExpiredArchives are archive files deleted for being older than the archive retention.
	ExpiredArchives []string `json:"expired_archives,omitempty"`
}

// GCPlace is where gc works, set by the daemon: the archive directory, and Own, the entries the
// daemon keeps for a participant or session known by key (its run logs, scratch and session data:
// the layout is the daemon's, core knows none; nil: none). Own gets only what is one path element.
type GCPlace struct {
	ArchiveDir string
	Own        func(key string) []string
}

// A gc unit is what one archive holds and one transaction deletes: a closed team, or a gone solo.
// Its rows are those of its participants ({parts}: the ids), plus the team's own. Each WHERE takes
// the unit's id wherever `:t` appears (a team id, or a participant id for a solo, which no team
// row or team_id column can equal, so the team's own conditions match nothing for a solo).
type gcTable struct{ name, where string }

type gcUnit struct {
	tables    []gcTable
	crossRefs string
}

const (
	gcTeamParts = `SELECT id FROM participants WHERE team_id=:t`
	gcSoloParts = `SELECT id FROM participants WHERE id=:t`
)

var gcTeamUnit, gcSoloUnit = newGCUnit(gcTeamParts), newGCUnit(gcSoloParts)

// newGCUnit selects every row of a unit, in delete order (dependents first).
// participants.token_hash is never archived.
func newGCUnit(parts string) gcUnit {
	msgs := `team_id=:t OR from_id IN ({parts}) OR to_id IN ({parts})`
	msgsO := `COALESCE(o.team_id,'')=:t OR o.from_id IN ({parts}) OR o.to_id IN ({parts})`
	u := gcUnit{tables: []gcTable{
		{"deliveries", `message_id IN (SELECT id FROM messages WHERE ` + msgs + `)`},
		// A run is known by the participant's current run, the runs that got the unit's mail
		// (deliveries), processes and kept events. An older run's batch with no delivery (an
		// empty pull) is not reachable: it stays behind, harmless (no reference to the unit).
		{"batches", `run_id IN (SELECT run_id FROM participants WHERE id IN ({parts})
		UNION SELECT run_id FROM deliveries WHERE message_id IN (SELECT id FROM messages WHERE ` + msgs + `)
		UNION SELECT run_id FROM events WHERE run_id IS NOT NULL AND participant IN ({parts})
		UNION SELECT run_id FROM processes WHERE participant_id IN ({parts}))`},
		{"messages", msgs},
		{"timers", `team_id=:t OR owner IN ({parts})`},
		{"processes", `participant_id IN ({parts})`},
		{"events", `team_id=:t OR participant IN ({parts})`},
		{"participant_refs", `participant_id IN ({parts})`},
		{"participants", `id IN ({parts})`},
		{"teams", `id=:t`},
	}}
	// Rows of others that reference the unit's rows (they would dangle).
	u.crossRefs = `SELECT
	(SELECT COUNT(*) FROM messages o WHERE NOT (` + msgsO + `) AND (
		o.reply_to IN (SELECT id FROM messages WHERE ` + msgs + `) OR
		o.target   IN (SELECT id FROM messages WHERE ` + msgs + `) OR
		o.cc_of    IN (SELECT id FROM messages WHERE ` + msgs + `))),
	(SELECT COUNT(*) FROM participants o WHERE o.id NOT IN ({parts}) AND (
		o.reports_to IN ({parts}) OR o.spawned_by IN ({parts})))`
	u.crossRefs = strings.ReplaceAll(u.crossRefs, "{parts}", parts)
	for i := range u.tables {
		u.tables[i].where = strings.ReplaceAll(u.tables[i].where, "{parts}", parts)
	}
	return u
}

// GC archives, verifies, then deletes teams closed more than ClosedBeforeMs ago, and solo sessions
// (no team) gone that long ago. Open teams and sessions not gone are never selected. Per unit:
// (a) archive file written 0600 and fsynced with its directory,
// (b) read back and counted against the DB, (c) one transaction re-checks the counts while
// deleting and writes a `gc` event with no team (it survives the delete). Any mismatch aborts
// that unit's delete; the archive stays.
func (e *Engine) GC(ctx context.Context, place GCPlace, a GCArgs) (GCResult, error) {
	if a.ClosedBeforeMs < 0 {
		return GCResult{}, errf(CodeInvalid, "closed_before must not be negative")
	}
	cutoff := e.now().UnixMilli() - a.ClosedBeforeMs
	rows, err := e.db.QueryContext(ctx, `SELECT id, name, closed_at FROM teams
		WHERE closed_at IS NOT NULL AND closed_at < ? ORDER BY closed_at`, cutoff)
	if err != nil {
		return GCResult{}, internal(err)
	}
	var res GCResult
	for rows.Next() {
		var g GCTeam
		if err := rows.Scan(&g.TeamID, &g.Name, &g.ClosedAt); err != nil {
			rows.Close()
			return GCResult{}, internal(err)
		}
		res.Teams = append(res.Teams, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GCResult{}, internal(err)
	}
	solos, err := e.db.QueryContext(ctx, `SELECT id, name, state_since FROM participants
		WHERE team_id IS NULL AND state='gone' AND state_since < ? ORDER BY state_since`, cutoff)
	if err != nil {
		return GCResult{}, internal(err)
	}
	for solos.Next() {
		var g GCTeam
		if err := solos.Scan(&g.ParticipantID, &g.Name, &g.ClosedAt); err != nil {
			solos.Close()
			return GCResult{}, internal(err)
		}
		res.Solos = append(res.Solos, g)
	}
	solos.Close()
	if err := solos.Err(); err != nil {
		return GCResult{}, internal(err)
	}
	for _, list := range []struct {
		gs   []GCTeam
		unit gcUnit
	}{{res.Teams, gcTeamUnit}, {res.Solos, gcSoloUnit}} {
		for i := range list.gs {
			g := &list.gs[i]
			if err := e.gcUnit(ctx, place.ArchiveDir, a.DryRun, list.unit, g); err != nil {
				return res, err
			}
			if err := e.removeOwn(ctx, place.Own, g); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// key is the id the unit's rows are selected by.
func (g *GCTeam) key() string {
	if g.ParticipantID != "" {
		return g.ParticipantID
	}
	return g.TeamID
}

func (e *Engine) gcUnit(ctx context.Context, dir string, dry bool, u gcUnit, g *GCTeam) error {
	var lines []ArchiveLine
	err := e.inTx(ctx, func(t *txn) error { // one snapshot of the unit's rows
		// A worker that is not gone may have a live process: its processes row is what
		// reconcile and team down need to find and stop it, so the team is kept.
		var live int
		if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM participants
			WHERE team_id=? AND mode=? AND state<>'gone'`, g.TeamID, modeHeadless).Scan(&live); err != nil {
			return internal(err)
		}
		if live > 0 {
			g.Skipped = fmt.Sprintf("live workers (%d not gone); run team down again", live)
			return nil
		}
		var msgs, parts int
		if err := t.QueryRowContext(t.ctx, u.crossRefs, sql.Named("t", g.key())).Scan(&msgs, &parts); err != nil {
			return internal(err)
		}
		if msgs+parts > 0 {
			g.Skipped = fmt.Sprintf("referenced by others (%d messages, %d participants)", msgs, parts)
			return nil
		}
		var err error
		lines, err = t.unitRows(u, g.key())
		return err
	})
	gcAfterSnapshot()
	if err != nil || g.Skipped != "" {
		return err
	}
	g.Counts = CountArchive(lines)
	if dry {
		return nil
	}
	g.Archive = filepath.Join(dir, fmt.Sprintf("%s-%d.jsonl", g.key(), g.ClosedAt))
	if err := writeArchive(g.Archive, lines); err != nil {
		return internal(err)
	}
	back, err := ReadArchive(g.Archive)
	if err != nil {
		return internal(err)
	}
	if !sameCounts(CountArchive(back), g.Counts) {
		g.Skipped = "archive read back does not match the DB"
		return nil
	}
	err = e.inTx(ctx, func(t *txn) error {
		// Still closed (a team) or still gone (a solo) since the same moment.
		var closedAt sql.NullInt64
		q := `SELECT closed_at FROM teams WHERE id=?`
		if g.ParticipantID != "" {
			q = `SELECT state_since FROM participants WHERE id=? AND state='gone' AND team_id IS NULL`
		}
		if err := t.QueryRowContext(t.ctx, q, g.key()).Scan(&closedAt); errors.Is(err, sql.ErrNoRows) {
			return errGCChanged
		} else if err != nil {
			return internal(err)
		}
		if !closedAt.Valid || closedAt.Int64 != g.ClosedAt {
			return errGCChanged
		}
		// The rows must still be exactly what the archive holds: a change without a new row
		// (e.g. a completion setting acked_at, which writes no event) must not be lost.
		now, err := t.unitRows(u, g.key())
		if err != nil {
			return err
		}
		if !sameLines(now, back) {
			return errGCChanged
		}
		for _, tb := range u.tables {
			r, err := t.ExecContext(t.ctx, `DELETE FROM `+tb.name+` WHERE `+tb.where, sql.Named("t", g.key()))
			if err != nil {
				return internal(err)
			}
			if n, _ := r.RowsAffected(); int(n) != g.Counts[tb.name] {
				return errGCChanged // a row came or went since the archive: roll back
			}
		}
		payload := map[string]any{"team": g.TeamID, "name": g.Name, "archive": g.Archive, "counts": g.Counts}
		if g.ParticipantID != "" {
			payload["participant"] = g.ParticipantID
		}
		return t.event(evt{typ: "gc", ref: g.key(), payload: payload})
	})
	if errors.Is(err, errGCChanged) {
		g.Skipped = "changed since the archive was written; nothing deleted"
		return nil
	}
	if err != nil {
		return err
	}
	g.Deleted = true
	return nil
}

var errGCChanged = errors.New("gc: team changed since archive")

// removeOwn deletes what place.Own names for each participant of a deleted unit (its ids come from
// the verified archive), and for the session ids it reported (session_ref, harness_ref) that
// nothing else uses any more: a session that went on as another participant keeps its data. A key
// is one path element or is skipped, so a self-reported id can never name a path. It counts
// entries and bytes.
func (e *Engine) removeOwn(ctx context.Context, own func(string) []string, g *GCTeam) error {
	if !g.Deleted || own == nil {
		return nil
	}
	lines, err := ReadArchive(g.Archive)
	if err != nil {
		return internal(err)
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if l.Table != "participants" {
			continue
		}
		id, _ := l.Row["id"].(string)
		keys := []string{id}
		for _, col := range []string{"session_ref", "harness_ref"} {
			if ref, _ := l.Row[col].(string); ref != "" {
				var used int
				if err := e.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM participants WHERE id=? OR session_ref=? OR harness_ref=?
					UNION ALL SELECT 1 FROM participant_refs WHERE ref=?)`, ref, ref, ref, ref).Scan(&used); err != nil {
					return internal(err)
				}
				if used == 0 {
					keys = append(keys, ref)
				}
			}
		}
		for _, k := range keys {
			if seen[k] || !oneElement(k) {
				continue
			}
			seen[k] = true
			for _, path := range own(k) {
				size, err := removeEntry(path)
				if err != nil {
					return internal(err)
				}
				if size >= 0 {
					g.LogDirs++
					g.LogBytes += size
				}
			}
		}
	}
	return nil
}

// oneElement: k is a single path element (not "", ".", "..", nor holding a separator).
func oneElement(k string) bool {
	return k != "" && k != "." && k != ".." && !strings.ContainsAny(k, "/\\\x00")
}

// removeEntry deletes path (a link is removed, not followed) and returns the bytes of the regular
// files it held; -1 when there was nothing.
func removeEntry(path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info, err := d.Info(); err == nil && d.Type().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return size, os.RemoveAll(path)
}

// ExpireArchives deletes the gc archives (*.jsonl) in dir last written more than keep ago, by
// the engine's clock, and returns their names. keep <= 0 keeps them all.
func (e *Engine) ExpireArchives(dir string, keep time.Duration) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, internal(err)
	}
	cutoff := e.now().Add(-keep)
	var out []string
	for _, d := range ents {
		info, err := d.Info()
		if err != nil || !d.Type().IsRegular() || filepath.Ext(d.Name()) != ".jsonl" || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, d.Name())); err != nil {
			return out, internal(err)
		}
		out = append(out, d.Name())
	}
	return out, nil
}

// gcAfterSnapshot runs between reading the rows and deleting them (a test seam for the
// "row added after the archive" race).
var gcAfterSnapshot = func() {}

// unitRows reads every row gc would delete for unit id, as archive lines.
func (t *txn) unitRows(u gcUnit, id string) ([]ArchiveLine, error) {
	var out []ArchiveLine
	for _, tb := range u.tables {
		rows, err := t.QueryContext(t.ctx, `SELECT * FROM `+tb.name+` WHERE `+tb.where, sql.Named("t", id))
		if err != nil {
			return nil, internal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, internal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, internal(err)
			}
			row := make(map[string]any, len(cols))
			for i, c := range cols {
				if c == "token_hash" {
					continue
				}
				if b, ok := vals[i].([]byte); ok {
					vals[i] = string(b)
				}
				row[c] = vals[i]
			}
			out = append(out, ArchiveLine{Table: tb.name, Row: row})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, internal(err)
		}
	}
	return out, nil
}

// ArchiveLine is one line of a gc archive: a row of a table.
type ArchiveLine struct {
	Table string         `json:"table"`
	Row   map[string]any `json:"row"`
}

// writeArchive writes lines to path (0600) through a temp file: fsync the file, rename, fsync
// the directory, so a crash leaves either no archive or a complete one.
func writeArchive(path string, lines []ArchiveLine) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// ReadArchive reads a gc archive (also `piggery --admin archive show`).
func ReadArchive(path string) ([]ArchiveLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ArchiveLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var l ArchiveLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil || l.Table == "" {
			return nil, fmt.Errorf("%s:%d: not an archive line", path, n)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// CountArchive counts rows per table; every table gc archives is present, 0 included.
func CountArchive(lines []ArchiveLine) map[string]int {
	c := make(map[string]int, len(gcTeamUnit.tables))
	for _, tb := range gcTeamUnit.tables {
		c[tb.name] = 0
	}
	for _, l := range lines {
		c[l.Table]++
	}
	return c
}

// sameLines reports whether two archive snapshots hold the same rows, compared as JSON (a
// read-back archive has JSON numbers where the DB has integers).
func sameLines(a, b []ArchiveLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, err1 := json.Marshal(a[i])
		y, err2 := json.Marshal(b[i])
		if err1 != nil || err2 != nil || string(x) != string(y) {
			return false
		}
	}
	return true
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
