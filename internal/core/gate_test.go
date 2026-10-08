package core_test

import (
	"slices"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// A solo closes its gate (docs/guide.md "Closing your gate"): it and the others vanish from each other's who, a
// send from outside fails like an unknown name, a send from it says its gate is closed, mail already
// waiting from outside is held (nothing acked) until it opens, and opening restores all of it and
// wakes the recipient. A team it founds, and a taskforce it calls, are closed too, and the taskforce
// still talks to it, both ways, while a third solo sees neither.
func TestClosedGate(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const tf = "template: tf\nsummary: a reviewer\ntaskforce: {idle_for: 20m}\nauto_join_role: chair\n" +
		"roles:\n  chair: {tools: [send, inbox, who]}\n"
	const plain = "template: plain\nauto_join_role: chair\nroles:\n  chair: {tools: [send, inbox, who, agent]}\n"
	var woken []string
	rt := &fakeRuntime{}
	e := core.New(db, core.WithRuntime(rt), core.WithNotify(func(id string) { woken = append(woken, id) }),
		core.WithTemplates(func(name, _ string) (string, error) {
			if name == "plain" {
				return plain, nil
			}
			return tf, nil
		}))
	solo := func(ref string) core.Caller {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: t.TempDir(), Harness: "pi", Mode: "rpc", HarnessRef: ref, Name: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	a, b, c := solo("aa"), solo("bb"), solo("cc")
	sees := func(from core.Caller, name string) bool {
		t.Helper()
		who, err := e.Who(ctx, from)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(who, func(p core.Presence) bool { return p.Name == name && p.ID != from.ParticipantID })
	}
	send := func(from core.Caller, to string) error {
		t.Helper()
		_, err := e.Send(ctx, from, core.SendArgs{To: to, Body: "hi"})
		return err
	}
	gate := func(from core.Caller, action string) {
		t.Helper()
		if _, err := e.Agent(ctx, from, core.AgentArgs{Action: action}); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if err := send(b, "aa"); err != nil { // waiting when a closes
		t.Fatal(err)
	}
	gate(a, core.AgentGateClose)
	gate(a, core.AgentGateClose) // idempotent
	if sees(b, "aa") || sees(a, "bb") || sees(a, "cc") || !sees(b, "cc") {
		t.Fatal("a closed solo and the others must vanish from each other's who; open ones still see each other")
	}
	if rule(send(b, "aa")) != "visibility/visibility.unknown_target" || rule(send(a, "bb")) != "visibility/gate.closed_self" {
		t.Fatalf("send in %v, out %v", send(b, "aa"), send(a, "bb"))
	}
	inbox := func() int {
		got, err := e.Inbox(ctx, a, core.InboxArgs{})
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if n := inbox(); n != 0 {
		t.Fatalf("mail from outside delivered to a closed gate: %d", n)
	}
	var held, acked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE held_reason='gate_closed'`).Scan(&held); err != nil || held != 1 {
		t.Fatalf("held = %d, %v", held, err)
	}
	woken = nil
	gate(a, core.AgentGateOpen)
	if !sees(b, "aa") || !sees(a, "bb") || send(b, "aa") != nil || send(a, "bb") != nil {
		t.Fatal("opening restores who and send both ways")
	}
	if !slices.Contains(woken, a.ParticipantID) || inbox() != 2 {
		t.Fatalf("the released mail wakes its recipient and is delivered: woken %v", woken)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE acked_at IS NOT NULL AND from_id<>'engine'`).Scan(&acked); err != nil || acked != 0 {
		t.Fatalf("acked = %d, %v", acked, err)
	}

	// A closed solo's team and taskforce are closed; the taskforce and its caller still reach each other.
	gate(a, core.AgentGateClose)
	f, err := e.Agent(ctx, a, core.AgentArgs{Action: core.AgentFound, Template: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	var shut bool
	if err := db.QueryRow(`SELECT gate_closed FROM teams WHERE id=?`, f.TeamID).Scan(&shut); err != nil || !shut {
		t.Fatalf("a founded team takes the founder's closed gate: %v, %v", shut, err)
	}
	if sees(b, f.TeamName) || rule(send(b, f.TeamName)) != "visibility/visibility.unknown_target" {
		t.Fatal("the closed team is hidden from the others")
	}
	// reopen: the team goes down, its session is a solo again with the team's state, the reopener's state is the team's.
	if _, err := e.Agent(ctx, a, core.AgentArgs{Action: core.AgentClose}); err != nil {
		t.Fatal(err)
	}
	j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: t.TempDir(), Harness: "pi", Mode: "rpc", HarnessRef: "aa"})
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := e.Authenticate(ctx, j.ID, j.Token)
	if who, _ := e.Who(ctx, a2); sees(b, "aa") || len(who) == 0 || !who[0].Closed {
		t.Fatalf("a session made solo by close takes the team's closed gate: %+v", who)
	}
	tfr, err := e.Agent(ctx, a2, core.AgentArgs{Action: core.AgentSpawn, Template: "tf", Task: "review"})
	if err != nil {
		t.Fatal(err)
	}
	chair, _ := e.Authenticate(ctx, rt.starts[len(rt.starts)-1].ParticipantID, rt.starts[len(rt.starts)-1].Token)
	var aName string
	if err := db.QueryRow(`SELECT name FROM participants WHERE id=?`, a2.ParticipantID).Scan(&aName); err != nil {
		t.Fatal(err)
	}
	if send(a2, tfr.TeamName) != nil || send(chair, aName) != nil || !sees(a2, tfr.TeamName) || !sees(chair, aName) {
		t.Fatal("a closed caller and its taskforce reach each other both ways")
	}
	if sees(c, tfr.TeamName) || rule(send(c, tfr.TeamName)) != "visibility/visibility.unknown_target" || rule(send(chair, "cc")) != "visibility/gate.closed_self" {
		t.Fatal("the taskforce of a closed caller is closed to everyone else")
	}
	// Closing holds mail from outside, not from the caller's own taskforce.
	gate(a2, core.AgentGateOpen)
	if err := send(chair, aName); err != nil {
		t.Fatal(err)
	}
	if err := send(c, "aa"); err != nil {
		t.Fatal(err)
	}
	gate(a2, core.AgentGateClose)
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE held_reason='gate_closed' AND to_id=?`, a2.ParticipantID).Scan(&held); err != nil || held != 1 {
		t.Fatalf("held on close = %d (only the other solo's), %v", held, err)
	}
}
