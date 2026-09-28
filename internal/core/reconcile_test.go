package core_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

type recoverFixture struct {
	e       *core.Engine
	rt      *fakeRuntime
	lead    core.Caller
	workers map[string]core.Caller // by name
	pids    map[string]int
}

// newRecoverFixture: a lead and spawned workers, as the daemon left them before it stopped.
func newRecoverFixture(t *testing.T, names ...string) (recoverFixture, func(string, ...any)) {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rt := &fakeRuntime{inspect: map[int]core.ProcState{}, inspectErr: errors.New("ps failed")}
	e := core.New(db, core.WithRuntime(rt))
	man := strings.Replace(leadWorker, "concurrency: 2", "concurrency: 10", 1)
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: "lead", Cwd: team.RootCwd})
	if err != nil {
		t.Fatal(err)
	}
	lead, _ := e.Authenticate(ctx, j.ID, j.Token)
	// The lead runs in an interactive pi session; a CLI participant (pull) sits beside it.
	if _, err := e.Identify(ctx, lead, core.IdentifyArgs{RunID: lead.RunID, Harness: "pi", Mode: "interactive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: "cli", Cwd: team.RootCwd}); err != nil {
		t.Fatal(err)
	}
	f := recoverFixture{e: e, rt: rt, lead: lead, workers: map[string]core.Caller{}, pids: map[string]int{}}
	af := agentFixture{e: e, rt: rt}
	for _, n := range names {
		f.workers[n] = af.spawn(t, lead, n)
		f.pids[n] = 100 + len(rt.starts)
		// The worker's extension identifies with the mode its pi reports; it stays headless.
		w := f.workers[n]
		if _, err := e.Identify(ctx, w, core.IdentifyArgs{RunID: w.RunID, Harness: "pi", Mode: "rpc"}); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	return f, exec
}

func (f recoverFixture) states(t *testing.T) map[string]string {
	t.Helper()
	who, err := f.e.Who(ctx, f.lead)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, p := range who {
		out[p.Name] = p.State
	}
	return out
}

// The four worker branches plus a live session: each gets one reconcile event; only an
// ours process is killed; workers notify their lead once; no mail is acked.
func TestReconcileDecisions(t *testing.T) {
	f, exec := newRecoverFixture(t, "norow", "dead", "ours", "reused", "unknown")
	exec(`DELETE FROM processes WHERE participant_id=?`, f.workers["norow"].ParticipantID)
	f.rt.inspect[f.pids["dead"]] = core.ProcDead
	f.rt.inspect[f.pids["ours"]] = core.ProcOurs
	f.rt.inspect[f.pids["reused"]] = core.ProcReused
	// "unknown" has no entry: Inspect fails.
	for _, w := range f.workers {
		if err := f.e.Presence(ctx, w, core.PresenceArgs{Event: core.PresenceAgentStart}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.e.Send(ctx, f.lead, core.SendArgs{To: "dead", Body: "pending"}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // a second daemon start adds nothing for decided workers
		if err := f.e.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]string{"lead": "gone", "cli": "idle", "norow": "gone", "dead": "gone", "ours": "gone", "reused": "gone", "unknown": "parked"}
	if got := f.states(t); len(got) != len(want) {
		t.Fatalf("states = %v", got)
	} else {
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("states = %v, want %v", got, want)
			}
		}
	}
	if len(f.rt.killed) != 1 || f.rt.killed[0] != f.pids["ours"] {
		t.Fatalf("kill requested for %v, want only the ours pid %d", f.rt.killed, f.pids["ours"])
	}
	evs, _ := f.e.Log(ctx, core.LogArgs{Limit: 1000})
	reconciles := map[string]int{}
	for _, ev := range evs {
		if ev.Type == "reconcile" {
			reconciles[ev.Participant]++
		}
	}
	// lead + 4 decided workers once each; the parked one is re-checked on the second start;
	// the pull participant is never touched.
	if len(reconciles) != 6 || reconciles[f.workers["unknown"].ParticipantID] != 2 || reconciles[f.lead.ParticipantID] != 1 {
		t.Fatalf("reconcile events per participant = %v", reconciles)
	}
	notices, err := f.e.Inbox(ctx, f.lead, core.InboxArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 5 || notices[0].From != core.AddrEngine || !strings.Contains(notices[4].Body, "parked") {
		t.Fatalf("lead got %d notices: %+v", len(notices), notices)
	}
	d, err := f.e.Inbox(ctx, f.workers["dead"], core.InboxArgs{})
	if err != nil || len(d) != 2 { // its task and the pending mail, both still unacked
		t.Fatalf("dead worker's mail = %+v, %v", d, err)
	}
}

// A parked worker resumes only after the driver shows its old process ended.
func TestResumeParkedNeedsVerifiedProcess(t *testing.T) {
	f, _ := newRecoverFixture(t, "w1")
	if err := f.e.Reconcile(ctx); err != nil { // Inspect fails -> parked
		t.Fatal(err)
	}
	resume := func() error {
		_, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"})
		return err
	}
	if err := resume(); rule(err) != "target/worker.unverified" {
		t.Fatalf("resume while the check fails: %v", err)
	}
	f.rt.inspect[f.pids["w1"]] = core.ProcOurs
	if err := resume(); rule(err) != "target/worker.unverified" {
		t.Fatalf("resume while the old process is still ours: %v", err)
	}
	f.rt.inspect[f.pids["w1"]] = core.ProcDead
	if err := resume(); err != nil {
		t.Fatalf("resume after the old process is dead: %v", err)
	}
	if len(f.rt.starts) != 2 || f.rt.starts[1].HarnessRef != f.rt.starts[0].HarnessRef {
		t.Fatalf("starts = %+v", f.rt.starts)
	}
	if st := f.states(t)["w1"]; st != "starting" {
		t.Fatalf("w1 after resume is %s", st)
	}
}
