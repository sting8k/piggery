package core_test

import (
	"database/sql"
	"errors"
	"github.com/sting8k/piggery/internal/store"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// wakeFixture is a team of three session members (harness pi, interactive): lead (the gate, the
// first to join), peer, and snd, who writes. lead and peer are gone, as when their persons quit.
type wakeFixture struct {
	agentFixture
	team          string
	snd           core.Caller
	lead, peer    string // participant ids
	peerRef, root string
}

func newWakeFixture(t *testing.T) wakeFixture {
	t.Helper()
	f := newAgentFixture(t)
	w := wakeFixture{agentFixture: f, team: f.teamID, peerRef: "peer-session-2"}
	f.db.QueryRow(`SELECT root_cwd FROM teams WHERE id=?`, f.teamID).Scan(&w.root)
	join := func(role, name, ref string) core.JoinResult {
		j, err := f.e.Join(ctx, core.JoinArgs{Team: f.teamID, Role: role, Name: name, Cwd: w.root})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`UPDATE participants SET harness='pi', mode='interactive', harness_ref=?, session_ref=?
			WHERE id=?`, ref, ref, j.ID); err != nil {
			t.Fatal(err)
		}
		return j
	}
	// f.lead and f.lead2 (role lead) already exist from the fixture: lead2 is not used here.
	w.lead = f.lead.ParticipantID
	f.db.Exec(`UPDATE participants SET harness='pi', mode='interactive', harness_ref='lead-session', session_ref='lead-session' WHERE id=?`, w.lead)
	pj := join("worker", "peer", "peer-session-1")
	f.db.Exec(`UPDATE participants SET session_ref=? WHERE id=?`, w.peerRef, pj.ID) // /clear: the newest session
	f.db.Exec(`INSERT INTO participant_refs(ref, participant_id) VALUES (?,?)`, w.peerRef, pj.ID)
	w.peer = pj.ID
	sj := join("worker", "snd", "snd-session")
	var err error
	if w.snd, err = f.e.Authenticate(ctx, sj.ID, sj.Token); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{w.lead, w.peer, f.lead2.ParticipantID} {
		if _, err := f.db.Exec(`UPDATE participants SET state='gone' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w wakeFixture) state(t *testing.T, id string) (state, mode string) {
	t.Helper()
	if err := w.db.QueryRow(`SELECT state, COALESCE(mode,'') FROM participants WHERE id=?`, id).Scan(&state, &mode); err != nil {
		t.Fatal(err)
	}
	return state, mode
}

func (w wakeFixture) send(t *testing.T, to, body string) {
	t.Helper()
	if _, err := w.e.Send(ctx, w.snd, core.SendArgs{To: to, Body: body}); err != nil {
		t.Fatal(err)
	}
}

func (w wakeFixture) wokeBy(t *testing.T, id string) int {
	t.Helper()
	var n int
	w.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='resumed' AND ref_id=? AND json_extract(payload,'$.by')='mail'`, id).Scan(&n)
	return n
}

// Mail for a gone member that is not the team's gate resumes its newest session as a headless
// worker, which then gets the mail; mail for the gone gate wakes nothing and waits for its
// person, who gets it when the session comes back.
func TestAutoWakeGoneMemberNotGate(t *testing.T) {
	w := newWakeFixture(t)

	w.send(t, "peer", "hello peer")
	waitFor(t, func() bool { s, _ := w.state(t, w.peer); return s != "gone" && s != "requested" })
	if s, m := w.state(t, w.peer); s == "gone" || m != "headless" || len(w.rt.starts) != 1 {
		t.Fatalf("peer: state %s mode %s, %d starts", s, m, len(w.rt.starts))
	}
	st := w.rt.starts[0]
	if !st.Resume || st.HarnessRef != w.peerRef || w.wokeBy(t, w.peer) != 1 {
		t.Fatalf("start %+v", st)
	}
	c, err := w.e.Authenticate(ctx, st.ParticipantID, st.Token)
	if err != nil {
		t.Fatal(err)
	}
	if in, err := w.e.Inbox(ctx, c, core.InboxArgs{}); err != nil || len(in) != 1 || in[0].Body != "hello peer" {
		t.Fatalf("the woken peer's inbox: %+v, %v", in, err)
	}

	w.send(t, "lead", "hello lead")
	time.Sleep(100 * time.Millisecond) // a wake would be asynchronous: give it the chance
	if s, _ := w.state(t, w.lead); s != "gone" || len(w.rt.starts) != 1 {
		t.Fatalf("the gone gate was woken: state %s, %d starts", s, len(w.rt.starts))
	}
	j, err := w.e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: w.root, HarnessRef: "lead-session", Harness: "pi", Mode: "interactive"})
	if err != nil || j.ID != w.lead {
		t.Fatalf("lead joins again: %+v, %v", j, err)
	}
	lc, _ := w.e.Authenticate(ctx, j.ID, j.Token)
	if in, err := w.e.Inbox(ctx, lc, core.InboxArgs{}); err != nil || len(in) != 1 || in[0].Body != "hello lead" {
		t.Fatalf("the returning gate's inbox: %+v, %v", in, err)
	}
}

// The same mail never wakes a member twice, whatever calls the notify path: a worker that died
// with it unacked stays gone; a newer message wakes it again.
func TestAutoWakeSameMailOnce(t *testing.T) {
	w := newWakeFixture(t)
	w.send(t, "peer", "one")
	waitFor(t, func() bool { s, _ := w.state(t, w.peer); return s == "starting" })
	if err := w.e.ProcessExited(ctx, w.peer, w.rt.starts[0].RunID, core.Exit{Code: 1}); err != nil {
		t.Fatal(err)
	}
	w.e.NotifyAfterCommit(w.peer)
	w.e.NotifyAfterCommit(w.peer)
	time.Sleep(100 * time.Millisecond)
	if s, _ := w.state(t, w.peer); s != "gone" || w.wokeBy(t, w.peer) != 1 {
		t.Fatalf("the same mail woke it again: state %s, %d wakes", s, w.wokeBy(t, w.peer))
	}
	w.send(t, "peer", "two")
	waitFor(t, func() bool { return w.wokeBy(t, w.peer) == 2 })
	if n := w.wokeBy(t, w.peer); n != 2 {
		t.Fatalf("a newer message did not wake it: %d wakes", n)
	}
}

// A person who opens the session a worker was woken in takes it back: the worker stops, and the
// same participant (id, name, role, unacked mail) is theirs again in its old mode. A worker the
// daemon spawned keeps its session.
func TestTakeBackByThePerson(t *testing.T) {
	w := newWakeFixture(t)
	w.send(t, "peer", "while you were away")
	waitFor(t, func() bool { s, _ := w.state(t, w.peer); return s == "starting" })
	if s, m := w.state(t, w.peer); m != "headless" || s != "starting" {
		t.Fatalf("not woken: %s %s", s, m)
	}

	j, err := w.e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: w.root, HarnessRef: w.peerRef, Harness: "pi", Mode: "interactive"})
	if err != nil || j.ID != w.peer {
		t.Fatalf("take back: %+v, %v", j, err)
	}
	if s, m := w.state(t, w.peer); s != "idle" || m != "interactive" || len(w.rt.stopped) != 1 {
		t.Fatalf("after take back: %s %s, stops %v", s, m, w.rt.stopped)
	}
	pc, _ := w.e.Authenticate(ctx, j.ID, j.Token)
	if in, err := w.e.Inbox(ctx, pc, core.InboxArgs{}); err != nil || len(in) != 1 {
		t.Fatalf("the person's inbox: %+v, %v", in, err)
	}

	sp := w.spawn(t, w.snd, "daemon-worker")
	w.db.Exec(`UPDATE participants SET harness_ref='wref', state='gone' WHERE id=?`, sp.ParticipantID)
	var ce *core.Error
	if _, err := w.e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: w.root, HarnessRef: "wref", Harness: "pi", Mode: "interactive"}); !errors.As(err, &ce) || ce.Code != core.CodeNotFound {
		t.Fatalf("a daemon-spawned worker's session was taken over: %v", err)
	}
}

// staleRow is the sequence behind F4 and F5 of the lifecycle audit: a team T with a lead, a guest
// admitted from a solo session x; T closes, x joins again as a new solo b, another solo reopens T.
// x's old row is a gone member of the open team again while b is live in the same session.
type staleRow struct {
	e          *core.Engine
	db         *sql.DB
	rt         *fakeRuntime
	gate       core.Caller // the solo that reopened T
	old, b     core.Caller // x's old member row, x's new solo row
	oldName    string
	team, root string
}

func newStaleRow(t *testing.T) staleRow {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rt := &fakeRuntime{}
	e := core.New(db, core.WithRuntime(rt), core.WithTemplates(func(string, string) (string, error) {
		return `template: guests
auto_join_role: lead
roles:
  lead:   {can_spawn: [guest], tools: [send, inbox, who, agent]}
  guest:  {tools: [inbox, who]}
routing:
  - {from: lead, to: guest, allow: true}
limits: {depth: 2, concurrency: 2}
`, nil
	}))
	f := staleRow{e: e, db: db, rt: rt, root: t.TempDir()}
	solo := func(ref string) core.Caller {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: f.root, Harness: "pi", Mode: "rpc", HarnessRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	agent := func(c core.Caller, a core.AgentArgs) core.AgentResult {
		t.Helper()
		r, err := e.Agent(ctx, c, a)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	lead := solo("lead-1")
	team := agent(lead, core.AgentArgs{Action: core.AgentFound})
	f.team = team.TeamName
	f.old = solo("x-2")
	db.QueryRow(`SELECT name FROM participants WHERE id=?`, f.old.ParticipantID).Scan(&f.oldName)
	agent(lead, core.AgentArgs{Action: core.AgentAdmit, Target: f.oldName, Role: "guest"})
	if _, err := e.TeamDown(ctx, core.TeamDownArgs{Team: team.TeamID}); err != nil {
		t.Fatal(err)
	}
	f.b = solo("x-2") // the same session, a new solo row
	if f.b.ParticipantID == f.old.ParticipantID {
		t.Fatal("the session did not get a new row after team down")
	}
	f.gate = solo("oth-3")
	agent(f.gate, core.AgentArgs{Action: core.AgentReopen, Team: team.TeamName})
	return f
}

// A person's session is one process: when a team closes, the session joins again as a solo, and
// the team is reopened by someone else, the session's old member row is not a member of the reopened
// team: it left at reopen, so mail to it is refused and no worker starts on the transcript the
// person has open as the solo (lifecycle audit F4; the wake guard that used to catch this is gone).
func TestReopenDropsTheStaleRowOfALiveSession(t *testing.T) {
	f := newStaleRow(t)
	var left sql.NullInt64
	f.db.QueryRow(`SELECT left_at FROM participants WHERE id=?`, f.old.ParticipantID).Scan(&left)
	if !left.Valid {
		t.Fatal("the stale member row did not leave when the team reopened")
	}
	if _, err := f.e.Send(ctx, f.gate, core.SendArgs{To: f.old.ParticipantID, Body: "are you there"}); rule(err) != "visibility/team.left" {
		t.Fatalf("mail to the stale row: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // a wake would be asynchronous: give it the chance
	if len(f.rt.starts) != 0 {
		t.Fatalf("%d starts; want no process in a session that is open as another row", len(f.rt.starts))
	}
}
