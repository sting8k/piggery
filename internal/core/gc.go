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
			WHERE team_id=? AND state<>'gone' ORDER BY created_at, rowid`, res.TeamID)
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

type GCTeam struct {
	TeamID   string         `json:"team_id"`
	Name     string         `json:"name"`
	ClosedAt int64          `json:"closed_at"`
	Counts   map[string]int `json:"counts"`
	Archive  string         `json:"archive,omitempty"`
	Deleted  bool           `json:"deleted"`
	Skipped  string         `json:"skipped,omitempty"` // why this team was left as it is
	// LogDirs and LogBytes are the run log dirs of its participants deleted with its rows.
	LogDirs  int   `json:"log_dirs,omitempty"`
	LogBytes int64 `json:"log_bytes,omitempty"`
}

type GCResult struct {
	Teams []GCTeam `json:"teams"`
	// ExpiredArchives are archive files deleted for being older than the archive retention.
	ExpiredArchives []string `json:"expired_archives,omitempty"`
}

// GCPlace is where gc works, set by the daemon: the archive directory, and the directory of a
// participant's run logs (the runtime driver's layout; nil: none).
type GCPlace struct {
	ArchiveDir string
	RunLogs    func(participantID string) string
}

// gcTables selects every row of a closed team, in delete order (dependents first). Each WHERE
// takes the team id wherever `?` appears. participants.token_hash is never archived.
var gcTables = []struct{ name, where string }{
	{"deliveries", `message_id IN (SELECT id FROM messages WHERE ` + gcMessages + `)`},
	// A run is known by the participant's current run, the runs that got the team's mail
	// (deliveries), processes and kept events. An older run's batch with no delivery (an
	// empty pull) is not reachable: it stays behind, harmless (no reference to the team).
	{"batches", `run_id IN (SELECT run_id FROM participants WHERE team_id=:t
		UNION SELECT run_id FROM deliveries WHERE message_id IN (SELECT id FROM messages WHERE ` + gcMessages + `)
		UNION SELECT run_id FROM events WHERE run_id IS NOT NULL AND participant IN (SELECT id FROM participants WHERE team_id=:t)
		UNION SELECT run_id FROM processes WHERE participant_id IN (SELECT id FROM participants WHERE team_id=:t))`},
	{"messages", gcMessages},
	{"timers", `team_id=:t`},
	{"processes", `participant_id IN (SELECT id FROM participants WHERE team_id=:t)`},
	{"events", `team_id=:t OR participant IN (SELECT id FROM participants WHERE team_id=:t)`},
	{"participants", `team_id=:t`},
	{"teams", `id=:t`},
}

const gcMessages = `team_id=:t OR from_id IN (SELECT id FROM participants WHERE team_id=:t)
	OR to_id IN (SELECT id FROM participants WHERE team_id=:t)`

// gcCrossRefs finds rows of other teams that reference the team's rows (they would dangle).
const gcCrossRefs = `SELECT
	(SELECT COUNT(*) FROM messages o WHERE NOT (` + gcMessagesO + `) AND (
		o.reply_to IN (SELECT id FROM messages WHERE ` + gcMessages + `) OR
		o.target   IN (SELECT id FROM messages WHERE ` + gcMessages + `) OR
		o.cc_of    IN (SELECT id FROM messages WHERE ` + gcMessages + `))),
	(SELECT COUNT(*) FROM participants o WHERE COALESCE(o.team_id,'')<>:t AND (
		o.reports_to IN (SELECT id FROM participants WHERE team_id=:t) OR
		o.spawned_by IN (SELECT id FROM participants WHERE team_id=:t)))`

const gcMessagesO = `COALESCE(o.team_id,'')=:t OR o.from_id IN (SELECT id FROM participants WHERE team_id=:t)
	OR o.to_id IN (SELECT id FROM participants WHERE team_id=:t)`

// GC archives, verifies, then deletes teams closed more than ClosedBeforeMs ago. Open teams are
// never selected. Per team: (a) archive file written 0600 and fsynced with its directory,
// (b) read back and counted against the DB, (c) one transaction re-checks the counts while
// deleting and writes a `gc` event with no team (it survives the delete). Any mismatch aborts
// that team's delete; the archive stays.
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
	for i := range res.Teams {
		if err := e.gcTeam(ctx, place.ArchiveDir, a.DryRun, &res.Teams[i]); err != nil {
			return res, err
		}
		if err := removeRunLogs(place.RunLogs, &res.Teams[i]); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (e *Engine) gcTeam(ctx context.Context, dir string, dry bool, g *GCTeam) error {
	var lines []ArchiveLine
	err := e.inTx(ctx, func(t *txn) error { // one snapshot of the team's rows
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
		if err := t.QueryRowContext(t.ctx, gcCrossRefs, sql.Named("t", g.TeamID)).Scan(&msgs, &parts); err != nil {
			return internal(err)
		}
		if msgs+parts > 0 {
			g.Skipped = fmt.Sprintf("referenced by other teams (%d messages, %d participants)", msgs, parts)
			return nil
		}
		var err error
		lines, err = t.teamRows(g.TeamID)
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
	g.Archive = filepath.Join(dir, fmt.Sprintf("%s-%d.jsonl", g.TeamID, g.ClosedAt))
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
		var closedAt sql.NullInt64
		if err := t.QueryRowContext(t.ctx, `SELECT closed_at FROM teams WHERE id=?`, g.TeamID).Scan(&closedAt); err != nil {
			return internal(err)
		}
		if !closedAt.Valid || closedAt.Int64 != g.ClosedAt {
			return errGCChanged
		}
		// The rows must still be exactly what the archive holds: a change without a new row
		// (e.g. a completion setting acked_at, which writes no event) must not be lost.
		now, err := t.teamRows(g.TeamID)
		if err != nil {
			return err
		}
		if !sameLines(now, back) {
			return errGCChanged
		}
		for _, tb := range gcTables {
			r, err := t.ExecContext(t.ctx, `DELETE FROM `+tb.name+` WHERE `+tb.where, sql.Named("t", g.TeamID))
			if err != nil {
				return internal(err)
			}
			if n, _ := r.RowsAffected(); int(n) != g.Counts[tb.name] {
				return errGCChanged // a row came or went since the archive: roll back
			}
		}
		return t.event(evt{typ: "gc", ref: g.TeamID,
			payload: map[string]any{"team": g.TeamID, "name": g.Name, "archive": g.Archive, "counts": g.Counts}})
	})
	if errors.Is(err, errGCChanged) {
		g.Skipped = "team changed since the archive was written; nothing deleted"
		return nil
	}
	if err != nil {
		return err
	}
	g.Deleted = true
	return nil
}

var errGCChanged = errors.New("gc: team changed since archive")

// removeRunLogs deletes the run log dir of each participant of a deleted team (its ids come
// from the verified archive) and counts dirs and bytes.
func removeRunLogs(runLogs func(string) string, g *GCTeam) error {
	if !g.Deleted || runLogs == nil {
		return nil
	}
	lines, err := ReadArchive(g.Archive)
	if err != nil {
		return internal(err)
	}
	for _, l := range lines {
		id, _ := l.Row["id"].(string)
		if l.Table != "participants" || id == "" || filepath.Base(id) != id {
			continue
		}
		dir := runLogs(id)
		var size int64
		err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if info, err := d.Info(); err == nil && d.Type().IsRegular() {
				size += info.Size()
			}
			return nil
		})
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return internal(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			return internal(err)
		}
		g.LogDirs++
		g.LogBytes += size
	}
	return nil
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

// teamRows reads every row gc would delete, as archive lines.
func (t *txn) teamRows(team string) ([]ArchiveLine, error) {
	var out []ArchiveLine
	for _, tb := range gcTables {
		rows, err := t.QueryContext(t.ctx, `SELECT * FROM `+tb.name+` WHERE `+tb.where, sql.Named("t", team))
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
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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
	c := make(map[string]int, len(gcTables))
	for _, tb := range gcTables {
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
