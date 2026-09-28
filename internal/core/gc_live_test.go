package core_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// Invariant: a closed team has no live worker that is not tracked, and gc never deletes the
// processes row of a worker that is not gone.

// driverLike is the local driver's contract that matters here: Stop fails ("no worker") for a
// participant whose Start has not registered a process (parked after a restart, or a Start
// still in flight). gate, when set, holds Start until closed.
type driverLike struct {
	*fakeRuntime
	mu      sync.Mutex
	held    map[string]bool
	gate    chan struct{}
	started chan struct{}
}

func (d *driverLike) Start(ctx context.Context, s core.Spec) (core.Proc, error) {
	if d.gate != nil {
		d.started <- struct{}{}
		<-d.gate
	}
	p, err := d.fakeRuntime.Start(ctx, s)
	d.mu.Lock()
	d.held[s.ParticipantID] = true
	d.mu.Unlock()
	return p, err
}

func (d *driverLike) Stop(ctx context.Context, id string) (core.Exit, error) {
	d.mu.Lock()
	ok := d.held[id]
	d.mu.Unlock()
	if !ok {
		return core.Exit{}, errors.New("no worker for participant " + id)
	}
	return d.fakeRuntime.Stop(ctx, id)
}

type liveFixture struct {
	e    *core.Engine
	db   *sql.DB
	rt   *driverLike
	now  *time.Time
	team string
	lead core.Caller
}

func newLiveFixture(t *testing.T) liveFixture {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rt := &driverLike{fakeRuntime: &fakeRuntime{}, held: map[string]bool{}}
	now := time.Unix(1_800_000_000, 0)
	e := core.New(db, core.WithRuntime(rt), core.WithClock(func() time.Time { return now }))
	man := strings.Replace(leadWorker, "routing:\n", "routing:\n  - {from: lead, to: notify, allow: true}\n", 1)
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: "lead", Cwd: team.RootCwd})
	lead, _ := e.Authenticate(ctx, j.ID, j.Token)
	return liveFixture{e: e, db: db, rt: rt, now: &now, team: team.ID, lead: lead}
}

func (f liveFixture) spawn(t *testing.T) string {
	t.Helper()
	r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return r.ParticipantID
}

func (f liveFixture) state(t *testing.T, id string) (state string, exited bool) {
	t.Helper()
	var at sql.NullInt64
	if err := f.db.QueryRow(`SELECT p.state, r.exited_at FROM participants p
		JOIN processes r ON r.participant_id=p.id AND r.run_id=p.run_id WHERE p.id=?`, id).Scan(&state, &at); err != nil {
		t.Fatal(err)
	}
	return state, at.Valid
}

func (f liveFixture) gc(t *testing.T) core.GCTeam {
	t.Helper()
	*f.now = f.now.Add(time.Minute)
	g, err := f.e.GC(ctx, core.GCPlace{ArchiveDir: t.TempDir()}, core.GCArgs{})
	if err != nil || len(g.Teams) != 1 {
		t.Fatalf("gc = %+v, %v", g, err)
	}
	return g.Teams[0]
}

// Regression: a worker whose stop cannot be verified keeps the team from gc.
func TestGCSkipsTeamWithLiveWorker(t *testing.T) {
	f := newLiveFixture(t)
	w := f.spawn(t)
	delete(f.rt.held, w) // the driver lost it; the inspect is inconclusive (no entry)
	down, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: f.team})
	if err != nil || len(down.Failed) != 1 {
		t.Fatalf("team down = %+v, %v; want w1 failed", down, err)
	}
	if st, _ := f.state(t, w); st == "gone" {
		t.Fatal("unverified worker marked gone")
	}
	if g := f.gc(t); g.Deleted || g.Skipped == "" {
		t.Fatalf("gc deleted a team with a live worker: %+v", g)
	}
}

// A parked worker (after a restart the driver holds nothing) is verified like reconcile: ours
// -> KillVerified -> gone, so team down completes and gc can proceed.
func TestTeamDownParkedWorkerByInspect(t *testing.T) {
	f := newLiveFixture(t)
	w := f.spawn(t)
	delete(f.rt.held, w)
	if _, err := f.db.Exec(`UPDATE participants SET state='parked' WHERE id=?`, w); err != nil {
		t.Fatal(err)
	}
	f.rt.inspect = map[int]core.ProcState{101: core.ProcOurs}
	down, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: f.team})
	if err != nil || len(down.Stopped) != 1 || len(f.rt.killed) != 1 || len(f.rt.stopped) != 0 {
		t.Fatalf("team down = %+v, %v; killed %v, stopped %v", down, err, f.rt.killed, f.rt.stopped)
	}
	if st, exited := f.state(t, w); st != "gone" || !exited {
		t.Fatalf("worker %s, exited %v", st, exited)
	}
	if g := f.gc(t); !g.Deleted {
		t.Fatalf("gc after a clean team down: %+v", g)
	}
}

// A Start still in flight when the team goes down: team down finds no process (gone), and the
// start stops the process it just got instead of leaving it alive in a closed team.
func TestStartAfterTeamDownStopsTheWorker(t *testing.T) {
	f := newLiveFixture(t)
	f.rt.gate, f.rt.started = make(chan struct{}), make(chan struct{})
	spawned := make(chan error, 1)
	go func() {
		_, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "t"})
		spawned <- err
	}()
	<-f.rt.started
	if _, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: f.team}); err != nil {
		t.Fatal(err)
	}
	close(f.rt.gate)
	if err := <-spawned; code(err) != core.CodeInvalid {
		t.Fatalf("spawn racing team down: %v", err)
	}
	w := f.rt.starts[0].ParticipantID
	if st, exited := f.state(t, w); st != "gone" || !exited || len(f.rt.stopped) != 1 {
		t.Fatalf("worker %s, exited %v, stopped %v", st, exited, f.rt.stopped)
	}
}

// A change that adds no row (acked_at set, which with minimal events writes no event)
// between the archive and the delete must stop the delete: the archive would be stale.
func TestGCSkipsTeamChangedWithoutNewRows(t *testing.T) {
	f := newLiveFixture(t)
	ask, err := f.e.Send(ctx, f.lead, core.SendArgs{To: core.AddrNotify, Body: "may I?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.TeamDown(ctx, core.TeamDownArgs{Team: f.team}); err != nil {
		t.Fatal(err)
	}
	defer core.SetGCAfterSnapshot(func() {
		if _, err := f.db.Exec(`UPDATE messages SET acked_at=acked_at+1 WHERE id=?`, ask.ID); err != nil {
			t.Error(err)
		}
	})()
	if g := f.gc(t); g.Deleted || g.Skipped == "" {
		t.Fatalf("gc deleted a team whose rows changed after the archive: %+v", g)
	}
}
