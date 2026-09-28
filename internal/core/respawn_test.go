package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// limits.max_respawn_per_hour: N resumes of a worker in 60 minutes pass; the next one does not
// start it, parks it, and tells its lead and notify once; a later resume is denied quietly; once
// the oldest resume slides out of the hour, resume works again.
func TestRespawnLimit(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt := &fakeRuntime{}
	now := time.Unix(1_800_000_000, 0)
	var sunk []string
	e := core.New(db, core.WithRuntime(rt), core.WithClock(func() time.Time { return now }),
		core.WithNotifySink(func(id string) { sunk = append(sunk, id) }))
	man := strings.Replace(leadWorker, "limits: {depth: 2, concurrency: 2}", "limits: {depth: 2, concurrency: 2, max_respawn_per_hour: 2}", 1)
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: "lead", Cwd: team.RootCwd})
	lead, _ := e.Authenticate(ctx, j.ID, j.Token)
	w, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	run := w.RunID
	crash := func() {
		t.Helper()
		if err := e.ProcessExited(ctx, w.ParticipantID, run, core.Exit{Code: 1}); err != nil {
			t.Fatal(err)
		}
	}
	resume := func(at time.Duration) error {
		now = time.Unix(1_800_000_000, 0).Add(at)
		r, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"})
		if err == nil {
			run = r.RunID
		}
		return err
	}
	notices := func() (toLead, toNotify int) {
		t.Helper()
		in, _ := e.Inbox(ctx, lead, core.InboxArgs{})
		return len(in), len(sunk)
	}

	crash()
	for _, at := range []time.Duration{0, 30 * time.Minute} {
		if err := resume(at); err != nil {
			t.Fatalf("resume at %v within the limit: %v", at, err)
		}
		crash()
	}
	if err := resume(31 * time.Minute); rule(err) != "limit/"+core.RuleRespawn {
		t.Fatalf("third resume in the hour: %v", err)
	}
	if l, h := notices(); l != 1 || h != 1 || len(rt.starts) != 3 {
		t.Fatalf("after the limit: %d lead / %d notify notices, %d starts", l, h, len(rt.starts))
	}
	if err := resume(40 * time.Minute); rule(err) != "limit/"+core.RuleRespawn {
		t.Fatalf("resume of the parked worker inside the hour: %v", err)
	}
	if l, h := notices(); l != 1 || h != 1 {
		t.Fatalf("second refusal notified again: %d lead / %d notify", l, h)
	}
	// The resume at 0 is out of the hour at 61m (only the one at 30m is left).
	if err := resume(61 * time.Minute); err != nil || len(rt.starts) != 4 {
		t.Fatalf("resume after the window slid: %v, %d starts", err, len(rt.starts))
	}

	// A parked worker still holds a concurrency slot; agent stop frees it the way team down
	// does (the driver does not hold it: verify like reconcile). Doubt keeps it parked.
	crash()
	if err := resume(62 * time.Minute); rule(err) != "limit/"+core.RuleRespawn {
		t.Fatalf("resume at 62m: %v", err)
	}
	stop := func() error {
		_, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"})
		return err
	}
	state := func() string {
		var s string
		db.QueryRow(`SELECT state FROM participants WHERE id=?`, w.ParticipantID).Scan(&s)
		return s
	}
	var pid int
	if _, err := db.Exec(`UPDATE processes SET exited_at=NULL WHERE participant_id=? AND run_id=?`, w.ParticipantID, run); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT pid FROM processes WHERE participant_id=? AND run_id=?`, w.ParticipantID, run).Scan(&pid)
	if err := stop(); err == nil || state() != "parked" {
		t.Fatalf("stop with an inconclusive inspect: %v, state %s", err, state())
	}
	rt.inspect = map[int]core.ProcState{pid: core.ProcOurs}
	if err := stop(); err != nil || state() != "gone" || len(rt.killed) != 1 || len(rt.stopped) != 0 {
		t.Fatalf("stop of a parked worker: %v, state %s, killed %v, driver stops %v", err, state(), rt.killed, rt.stopped)
	}
}
