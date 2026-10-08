package core_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

type gcFixture struct {
	e     *core.Engine
	db    *sql.DB
	rt    *fakeRuntime
	now   *time.Time
	dir   string
	teams map[string]string                 // name -> id
	roots map[string]string                 // name -> root
	who   map[string]map[string]core.Caller // team name -> participant name -> caller
}

// newGCFixture brings up lead/worker teams by name, each with lead (role lead) and lead2 (role worker).
func newGCFixture(t *testing.T, names ...string) gcFixture {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.UnixMilli(1_000_000)
	f := gcFixture{db: db, rt: &fakeRuntime{}, now: &now, dir: t.TempDir(),
		teams: map[string]string{}, roots: map[string]string{}, who: map[string]map[string]core.Caller{}}
	f.e = core.New(db, core.WithRuntime(f.rt), core.WithClock(func() time.Time { return *f.now }))
	for _, n := range names {
		team, err := f.e.TeamUp(ctx, core.TeamUpArgs{Manifest: leadWorker, Cwd: t.TempDir(), Name: n})
		if err != nil {
			t.Fatal(err)
		}
		f.teams[n], f.roots[n], f.who[n] = team.ID, team.RootCwd, map[string]core.Caller{}
		for p, role := range map[string]string{"lead": "lead", "lead2": "worker"} {
			j, err := f.e.Join(ctx, core.JoinArgs{Team: team.ID, Role: role, Name: p, Cwd: team.RootCwd})
			if err != nil {
				t.Fatal(err)
			}
			c, err := f.e.Authenticate(ctx, j.ID, j.Token)
			if err != nil {
				t.Fatal(err)
			}
			f.who[n][p] = c
		}
	}
	return f
}

func (f gcFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Team down: workers stopped through the driver, everyone gone, timers off, closed_at, one
// team_down event; every verb refused afterwards; unacked mail stays unacked. A worker the
// engine stopped for the close is refused without a denied event (its extension calling while
// it stops is noise, live 2026-09-27); a session of the team calling in is still recorded.
func TestTeamDown(t *testing.T) {
	f := newGCFixture(t, "a")
	lead := f.who["a"]["lead"]
	w := agentFixture{e: f.e, rt: f.rt}.spawn(t, lead, "w1")
	m, err := f.e.Send(ctx, lead, core.SendArgs{To: "lead2", Body: "unread"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.WatchAdd(ctx, lead, core.TimerArgs{To: "lead2", InMs: 60_000, Body: "ping"}); err != nil {
		t.Fatal(err)
	}
	session, err := f.e.Join(ctx, core.JoinArgs{Team: f.teams["a"], Role: "lead", Name: "watcher", Cwd: f.roots["a"]})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: "a"})
	if err != nil || !eq(res.Stopped, []string{"w1"}) || !eq(f.rt.stopped, []string{w.ParticipantID}) {
		t.Fatalf("team down: %+v %v, driver stops %v", res, err, f.rt.stopped)
	}
	id := f.teams["a"]
	if n := f.count(t, `SELECT COUNT(*) FROM participants WHERE team_id=? AND state<>'gone'`, id); n != 0 {
		t.Fatalf("%d participants not gone", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM timers WHERE team_id=? AND active=1`, id); n != 0 {
		t.Fatalf("%d timers still active", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM processes WHERE participant_id=? AND exited_at IS NULL`, w.ParticipantID); n != 0 {
		t.Fatal("worker exit not recorded")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM events WHERE type='team_down' AND team_id=?`, id); n != 1 {
		t.Fatalf("team_down events: %d", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE id=? AND acked_at IS NULL`, m.ID); n != 1 {
		t.Fatal("closing the team acked mail")
	}

	var tok string // the worker's token from the driver spec
	for _, s := range f.rt.starts {
		if s.ParticipantID == w.ParticipantID {
			tok = s.Token
		}
	}
	if _, err := f.e.Authenticate(ctx, w.ParticipantID, tok); rule(err) != "token/team.closed" {
		t.Fatalf("closed team's worker authenticated: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM events WHERE type='denied' AND payload LIKE '%team.closed%'`); n != 0 {
		t.Fatalf("the stopped worker's call wrote %d team.closed denied events; want none", n)
	}
	if _, err := f.e.Authenticate(ctx, session.ID, session.Token); rule(err) != "token/team.closed" {
		t.Fatalf("closed team's session authenticated: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM events WHERE type='denied' AND payload LIKE '%team.closed%' AND participant=?`, session.ID); n != 1 {
		t.Fatalf("team.closed denied events for the session: %d; want 1", n)
	}
	if _, err := f.e.Join(ctx, core.JoinArgs{Team: id, Role: "lead", Name: "late", Cwd: t.TempDir()}); code(err) != core.CodeNotFound {
		t.Fatalf("join a closed team: %v", err)
	}
	if _, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: id}); code(err) != core.CodeInvalid {
		t.Fatalf("second team down: %v", err)
	}
}

// gc: dry run writes nothing; a closed team is archived, read back equal, deleted, and the gc
// event survives; an open team is untouched.
func TestGCArchivesThenDeletesOnlyClosedTeams(t *testing.T) {
	f := newGCFixture(t, "closed", "open")
	for _, n := range []string{"closed", "open"} {
		if _, err := f.e.Send(ctx, f.who[n]["lead"], core.SendArgs{To: "lead2", Body: "hi " + n}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: "closed"}); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(2 * time.Hour)
	closedID, openID := f.teams["closed"], f.teams["open"]
	openRows := f.count(t, `SELECT (SELECT COUNT(*) FROM messages WHERE team_id=?) + (SELECT COUNT(*) FROM events WHERE team_id=?)`, openID, openID)
	wantMsgs := f.count(t, `SELECT COUNT(*) FROM messages WHERE team_id=?`, closedID)
	wantEvents := f.count(t, `SELECT COUNT(*) FROM events WHERE team_id=?`, closedID)

	dry, err := f.e.GC(ctx, core.GCPlace{ArchiveDir: f.dir}, core.GCArgs{ClosedBeforeMs: time.Hour.Milliseconds(), DryRun: true})
	if err != nil || len(dry.Teams) != 1 || dry.Teams[0].Counts["messages"] != wantMsgs || dry.Teams[0].Deleted {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if ents, _ := os.ReadDir(f.dir); len(ents) != 0 || f.count(t, `SELECT COUNT(*) FROM teams WHERE id=?`, closedID) != 1 {
		t.Fatal("dry run wrote something")
	}

	res, err := f.e.GC(ctx, core.GCPlace{ArchiveDir: f.dir}, core.GCArgs{ClosedBeforeMs: time.Hour.Milliseconds()})
	if err != nil || len(res.Teams) != 1 || !res.Teams[0].Deleted {
		t.Fatalf("gc: %+v %v", res, err)
	}
	g := res.Teams[0]
	if st, err := os.Stat(g.Archive); err != nil || !permIs(st.Mode().Perm(), 0o600) || filepath.Dir(g.Archive) != f.dir {
		t.Fatalf("archive file: %v %v", st, err)
	}
	lines, err := core.ReadArchive(g.Archive)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, l := range lines {
		got[l.Table]++
		if _, ok := l.Row["token_hash"]; ok {
			t.Fatal("archive has token_hash")
		}
		if l.Table == "messages" && l.Row["body"] != "hi closed" {
			t.Fatalf("archived message: %v", l.Row)
		}
	}
	if got["messages"] != wantMsgs || got["events"] != wantEvents || got["participants"] != 2 || got["teams"] != 1 {
		t.Fatalf("archive counts %v, want messages %d events %d", got, wantMsgs, wantEvents)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM teams WHERE id=?`, `SELECT COUNT(*) FROM participants WHERE team_id=?`,
		`SELECT COUNT(*) FROM messages WHERE team_id=?`, `SELECT COUNT(*) FROM events WHERE team_id=?`} {
		if n := f.count(t, q, closedID); n != 0 {
			t.Fatalf("%s: %d rows left", q, n)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM events WHERE type='gc' AND ref_id=? AND team_id IS NULL`, closedID); n != 1 {
		t.Fatalf("gc events: %d", n)
	}
	if n := f.count(t, `SELECT (SELECT COUNT(*) FROM messages WHERE team_id=?) + (SELECT COUNT(*) FROM events WHERE team_id=?)`, openID, openID); n != openRows {
		t.Fatalf("open team rows %d -> %d", openRows, n)
	}
}

// A row added between the archive and the delete aborts the delete; so does a reference from
// another team.
func TestGCSkipsChangedOrReferencedTeams(t *testing.T) {
	f := newGCFixture(t, "a", "b")
	m, err := f.e.Send(ctx, f.who["a"]["lead"], core.SendArgs{To: "lead2", Body: "q"})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if _, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: n}); err != nil {
			t.Fatal(err)
		}
	}
	*f.now = f.now.Add(time.Hour)
	// b replies across teams to a's message (only possible by hand): a must be skipped.
	if _, err := f.db.Exec(`INSERT INTO messages(id, seq, team_id, from_id, to_id, reply_to, body, created_at)
		VALUES ('x', 999, ?, 'someone', 'else', ?, 'cross', 1)`, f.teams["b"], m.ID); err != nil {
		t.Fatal(err)
	}
	defer core.SetGCAfterSnapshot(func() { // b gains a row after its snapshot
		f.db.Exec(`INSERT INTO events(ts, type, team_id, payload) VALUES (1, 'late', ?, '{}')`, f.teams["b"])
	})()

	res, err := f.e.GC(ctx, core.GCPlace{ArchiveDir: f.dir}, core.GCArgs{})
	if err != nil || len(res.Teams) != 2 {
		t.Fatalf("gc: %+v %v", res, err)
	}
	want := map[string]string{"a": "referenced by others", "b": "changed since the archive"}
	for _, g := range res.Teams {
		if g.Deleted || !strings.HasPrefix(g.Skipped, want[g.Name]) {
			t.Fatalf("team %s: %+v", g.Name, g)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM teams`); n != 2 {
		t.Fatalf("teams left: %d", n)
	}
}

// gc removes what the daemon names for each participant of a deleted team, by id, and nothing
// else: the daemon's other files stay, and a session id a live participant still reports is not a
// key (nor is one that is no single path element).
func TestGCRemovesTheEntriesOfDeletedParticipantsOnly(t *testing.T) {
	f := newGCFixture(t, "a", "b")
	own := t.TempDir()
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lead, lead2 := f.who["a"]["lead"].ParticipantID, f.who["a"]["lead2"].ParticipantID
	write(filepath.Join(own, lead, "run1", "log"))
	write(filepath.Join(own, lead2))
	write(filepath.Join(own, "piggery.db"))
	write(filepath.Join(own, "shared"))
	// lead's session ids: one a participant of the open team b still reports, one that would climb out.
	for ref, id := range map[string]string{"shared": lead, "..": lead2} {
		if _, err := f.db.Exec(`UPDATE participants SET session_ref=? WHERE id=?`, ref, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`UPDATE participants SET harness_ref='shared' WHERE id=?`, f.who["b"]["lead"].ParticipantID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: "a"}); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(time.Hour)
	var keys []string
	place := core.GCPlace{ArchiveDir: f.dir, Own: func(k string) []string { keys = append(keys, k); return []string{filepath.Join(own, k)} }}
	res, err := f.e.GC(ctx, place, core.GCArgs{})
	if err != nil || len(res.Teams) != 1 || !res.Teams[0].Deleted || res.Teams[0].LogDirs != 2 {
		t.Fatalf("gc: %+v %v", res, err)
	}
	for _, k := range keys {
		if k != lead && k != lead2 {
			t.Fatalf("the daemon was asked about %q; only the ids of the deleted participants are keys", k)
		}
	}
	for _, gone := range []string{filepath.Join(own, lead), filepath.Join(own, lead2)} {
		if _, err := os.Lstat(gone); err == nil {
			t.Fatalf("%s kept", gone)
		}
	}
	for _, kept := range []string{"piggery.db", "shared"} {
		if _, err := os.Lstat(filepath.Join(own, kept)); err != nil {
			t.Fatalf("%s: %v", kept, err)
		}
	}
}

// A solo session gone longer than the retention is archived and deleted like a closed team, with its
// entries; one gone more recently, and a live one, stay. The session that comes back joins as a new solo.
func TestGCDropsAGoneSolo(t *testing.T) {
	f := newGCFixture(t)
	root := t.TempDir()
	join := func(ref string) core.Caller {
		t.Helper()
		j, err := f.e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: root, Harness: "pi", Mode: "rpc", HarnessRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, err := f.e.Authenticate(ctx, j.ID, j.Token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	end := func(c core.Caller) {
		t.Helper()
		if err := f.e.Presence(ctx, c, core.PresenceArgs{Event: core.PresenceShutdown}); err != nil {
			t.Fatal(err)
		}
	}
	old, recent, live := join("old-1"), join("recent-1"), join("live-1")
	end(old)
	*f.now = f.now.Add(2 * time.Hour)
	end(recent)
	*f.now = f.now.Add(time.Minute)

	own := t.TempDir()
	for _, id := range []string{old.ParticipantID, recent.ParticipantID} {
		if err := os.MkdirAll(filepath.Join(own, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	place := core.GCPlace{ArchiveDir: f.dir, Own: func(k string) []string { return []string{filepath.Join(own, k)} }}
	res, err := f.e.GC(ctx, place, core.GCArgs{ClosedBeforeMs: time.Hour.Milliseconds()})
	if err != nil || len(res.Teams) != 0 || len(res.Solos) != 1 {
		t.Fatalf("gc: %+v %v", res, err)
	}
	g := res.Solos[0]
	if g.ParticipantID != old.ParticipantID || !g.Deleted || g.LogDirs != 1 || g.Counts["participants"] != 1 {
		t.Fatalf("solo: %+v", g)
	}
	if lines, err := core.ReadArchive(g.Archive); err != nil || len(lines) == 0 {
		t.Fatalf("archive: %v %v", lines, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM participants WHERE id=?`, old.ParticipantID); n != 0 {
		t.Fatal("the solo's row is still there")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM participants WHERE id IN (?,?)`, recent.ParticipantID, live.ParticipantID); n != 2 {
		t.Fatalf("participants left: %d, want the recent and the live solo", n)
	}
	if _, err := os.Stat(filepath.Join(own, old.ParticipantID)); err == nil {
		t.Fatal("the dropped solo's entry is kept")
	}
	if _, err := os.Stat(filepath.Join(own, recent.ParticipantID)); err != nil {
		t.Fatal("a solo that is not old enough lost its entry")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM events WHERE type='gc' AND ref_id=?`, old.ParticipantID); n != 1 {
		t.Fatalf("gc events: %d", n)
	}

	j, err := f.e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: root, Harness: "pi", Mode: "rpc", HarnessRef: "old-1"})
	if err != nil || j.ID == old.ParticipantID || j.TeamID != "" {
		t.Fatalf("the dropped session coming back: %+v %v; want a new solo", j, err)
	}
}

// A name shared with a gone member of a closed team resolves to the live one; when every match
// is in a closed team the name is ambiguous as before (`piggery x kc`, 2026-09-30).
func TestAdminNameSkipsGoneMembersOfClosedTeams(t *testing.T) {
	f := newGCFixture(t, "a", "b")
	if _, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: "a"}); err != nil {
		t.Fatal(err)
	}
	_, err := f.e.Kill(ctx, core.AdminTarget{Target: "lead2"})
	if err == nil || !strings.Contains(err.Error(), "not a headless worker") { // resolved: b's session
		t.Fatalf("kill lead2 with team a closed: %v; want it to reach b's lead2", err)
	}
	if _, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Kill(ctx, core.AdminTarget{Target: "lead2"}); err == nil || !strings.Contains(err.Error(), "pass --team") {
		t.Fatalf("kill lead2 with both teams closed: %v; want the ambiguity", err)
	}
}
