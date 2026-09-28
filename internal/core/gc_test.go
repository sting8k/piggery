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
	if st, err := os.Stat(g.Archive); err != nil || st.Mode().Perm() != 0o600 || filepath.Dir(g.Archive) != f.dir {
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
	if _, err := f.db.Exec(`INSERT INTO messages(id, seq, team_id, from_id, to_id, thread_id, reply_to, body, created_at)
		VALUES ('x', 999, ?, 'someone', 'else', 'x', ?, 'cross', 1)`, f.teams["b"], m.ID); err != nil {
		t.Fatal(err)
	}
	defer core.SetGCAfterSnapshot(func() { // b gains a row after its snapshot
		f.db.Exec(`INSERT INTO events(ts, type, team_id, payload) VALUES (1, 'late', ?, '{}')`, f.teams["b"])
	})()

	res, err := f.e.GC(ctx, core.GCPlace{ArchiveDir: f.dir}, core.GCArgs{})
	if err != nil || len(res.Teams) != 2 {
		t.Fatalf("gc: %+v %v", res, err)
	}
	want := map[string]string{"a": "referenced by other teams", "b": "team changed since the archive"}
	for _, g := range res.Teams {
		if g.Deleted || !strings.HasPrefix(g.Skipped, want[g.Name]) {
			t.Fatalf("team %s: %+v", g.Name, g)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM teams`); n != 2 {
		t.Fatalf("teams left: %d", n)
	}
}
