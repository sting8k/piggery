package core_test

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// Leave (a member founds elsewhere): its old row is gone with a dead run, its unacked mail and
// its workers' reports_to move to the team's gate (never acked on the way: the old run's
// completion is refused, the gate acks with its own), with one left and one rerouted event.
// With no live member left, the mail is held and notify is told; when a member comes back the
// engine moves it to that gate.
func TestLeave(t *testing.T) {
	man, err := os.ReadFile("../../manifests/p2p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sunk []string
	// Template v: a boss can admit but has no send, so it is never the gate.
	const v = "template: v\nauto_join_role: lead\nroles:\n" +
		"  lead: {tools: [send, inbox, who, agent], can_spawn: [boss, peer]}\n" +
		"  boss: {tools: [inbox, who, agent], can_spawn: [peer]}\n" +
		"  peer: {tools: [send, inbox, who]}\nlimits: {depth: 2, concurrency: 2}\n"
	// Template w: a lead and a dev it takes in.
	const w = "template: w\nauto_join_role: lead\nroles:\n" +
		"  lead: {tools: [send, inbox, who, agent], can_spawn: [dev]}\n" +
		"  dev: {tools: [send, inbox, who]}\nlimits: {depth: 2, concurrency: 2}\n"
	e := core.New(db, core.WithTemplates(func(name, _ string) (string, error) {
		switch name {
		case "v":
			return v, nil
		case "w":
			return w, nil
		}
		return string(man), nil
	}), core.WithRuntime(&fakeRuntime{}),
		core.WithNotifySink(func(id string) { sunk = append(sunk, id) }))
	solo := func(ref, cwd string) core.Caller {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: cwd, Harness: "pi", Mode: "rpc", HarnessRef: ref})
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
	name := func(c core.Caller) string {
		var n string
		if err := db.QueryRow(`SELECT name FROM participants WHERE id=?`, c.ParticipantID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Team at root: lead, then b and c admitted by it (b joins first: b is the next gate).
	root := t.TempDir()
	lead := solo("lead-1", root)
	agent(lead, core.AgentArgs{Action: core.AgentFound})
	b, c := solo("bee-2", root), solo("cee-3", root)
	agent(lead, core.AgentArgs{Action: core.AgentAdmit, Target: name(b), Role: "peer"})
	agent(lead, core.AgentArgs{Action: core.AgentAdmit, Target: name(c), Role: "peer"})
	ask, err := e.Send(ctx, c, core.SendArgs{To: name(lead), Body: "review please"})
	if err != nil {
		t.Fatal(err)
	}
	if got := pull(t, e, lead, 1); !slices.Equal(got, []string{ask.ID}) { // delivered, not acked
		t.Fatalf("lead batch 1 = %v", got)
	}

	nt := agent(lead, core.AgentArgs{Action: core.AgentFound}) // lead leaves for a new team
	if nt.Token == "" || nt.ParticipantID == lead.ParticipantID {
		t.Fatalf("found from a team = %+v; want a new participant", nt)
	}
	if _, err := e.Completion(ctx, lead, core.CompletionArgs{Batch: 1}); code(err) != core.CodeUnauthorized {
		t.Fatalf("the leaver's old run completes: %v", err)
	}
	var to, from, reportsTo string
	if err := db.QueryRow(`SELECT to_id, rerouted_from FROM messages WHERE id=?`, ask.ID).Scan(&to, &from); err != nil ||
		to != b.ParticipantID || from != lead.ParticipantID {
		t.Fatalf("mail to the leaver: to %s from %s, %v; want b, rerouted from lead", to, from, err)
	}
	if err := db.QueryRow(`SELECT reports_to FROM participants WHERE id=?`, c.ParticipantID).Scan(&reportsTo); err != nil ||
		reportsTo != b.ParticipantID {
		t.Fatalf("c reports to %s, %v; want the gate b", reportsTo, err)
	}
	if got := pull(t, e, b, 1); !slices.Equal(got, []string{ask.ID}) {
		t.Fatalf("gate b batch 1 = %v", got)
	}
	if _, err := e.Completion(ctx, b, core.CompletionArgs{Batch: 1}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM messages WHERE id=? AND acked_at IS NOT NULL`, ask.ID); n != 1 {
		t.Fatal("the gate's completion did not ack the moved mail")
	}
	if l, r := count(`SELECT COUNT(*) FROM events WHERE type='left'`), count(`SELECT COUNT(*) FROM events WHERE type='rerouted'`); l != 1 || r != 1 {
		t.Fatalf("left %d, rerouted %d; want 1, 1", l, r)
	}
	if n := count(`SELECT COUNT(*) FROM events WHERE type='gate_moved' AND json_extract(payload,'$.from')=? AND json_extract(payload,'$.to')=?`,
		lead.ParticipantID, b.ParticipantID); n != 1 {
		t.Fatalf("gate_moved lead -> b events = %d; want 1", n)
	}
	var ce *core.Error
	if _, err := e.Send(ctx, c, core.SendArgs{To: name(lead), Body: "still there?"}); !errors.As(err, &ce) ||
		ce.RuleID != "team.left" || ce.Details.(map[string]any)["gate"] != name(b) {
		t.Fatalf("mail to the leaver while the team has a gate: %v", err)
	}

	// Team u: lead2 and x (gone). A solo writes to lead2 (its gate); lead2 leaves: x, gone, is the
	// gate now (the next by join order): the mail waits in its inbox, not held, and notify is told;
	// x comes back and has it.
	other, far := t.TempDir(), t.TempDir()
	lead2 := solo("led-4", other)
	agent(lead2, core.AgentArgs{Action: core.AgentFound})
	x := solo("xxx-5", other)
	agent(lead2, core.AgentArgs{Action: core.AgentAdmit, Target: name(x), Role: "peer"})
	if err := e.Presence(ctx, x, core.PresenceArgs{Event: core.PresenceShutdown}); err != nil {
		t.Fatal(err)
	}
	s := solo("sol-6", far)
	late, err := e.Send(ctx, s, core.SendArgs{To: name(lead2), Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	sunk = nil
	agent(lead2, core.AgentArgs{Action: core.AgentFound})
	var held string
	if err := db.QueryRow(`SELECT to_id, COALESCE(held_reason,'') FROM messages WHERE id=?`, late.ID).Scan(&to, &held); err != nil ||
		to != x.ParticipantID || held != "" || len(sunk) != 1 {
		t.Fatalf("gate gone: to %s held %q, notify %v, %v; want x, not held, one notice", to, held, sunk, err)
	}
	back, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: other, Harness: "pi", Mode: "rpc", HarnessRef: "xxx-5"})
	if err != nil || back.ID != x.ParticipantID {
		t.Fatalf("x resumes = %+v, %v", back, err)
	}
	if err := db.QueryRow(`SELECT to_id, COALESCE(held_reason,'') FROM messages WHERE id=?`, late.ID).Scan(&to, &held); err != nil ||
		to != x.ParticipantID || held != "" {
		t.Fatalf("after x is back: to %s held %q, %v; want x, not held", to, held, err)
	}

	// Team v: the lead leaves; the boss is live but has no send, so no gate: mail is held and
	// the boss keeps reporting to the leaver. The boss admits a peer (send): it is the gate,
	// gets the held mail, and the boss reports to it.
	vroot := t.TempDir()
	vlead := solo("vld-7", vroot)
	agent(vlead, core.AgentArgs{Action: core.AgentFound, Template: "v"})
	boss := solo("bos-8", vroot)
	agent(vlead, core.AgentArgs{Action: core.AgentAdmit, Target: name(boss), Role: "boss"})
	vmail, err := e.Send(ctx, s, core.SendArgs{To: name(vlead), Body: "for v"})
	if err != nil {
		t.Fatal(err)
	}
	agent(vlead, core.AgentArgs{Action: core.AgentFound})
	if err := db.QueryRow(`SELECT COALESCE(held_reason,'') FROM messages WHERE id=?`, vmail.ID).Scan(&held); err != nil || held != "team.no_gate" {
		t.Fatalf("v without a gate: held %q, %v", held, err)
	}
	peer := solo("per-9", vroot)
	agent(boss, core.AgentArgs{Action: core.AgentAdmit, Target: name(peer), Role: "peer"})
	if err := db.QueryRow(`SELECT to_id, COALESCE(held_reason,'') FROM messages WHERE id=?`, vmail.ID).Scan(&to, &held); err != nil ||
		to != peer.ParticipantID || held != "" {
		t.Fatalf("after a peer is admitted: to %s held %q, %v; want the peer, not held", to, held, err)
	}
	if err := db.QueryRow(`SELECT reports_to FROM participants WHERE id=?`, boss.ParticipantID).Scan(&reportsTo); err != nil ||
		reportsTo != peer.ParticipantID {
		t.Fatalf("boss reports to %s, %v; want the new gate", reportsTo, err)
	}

	// Sessions come before headless workers as the gate: the lead spawns a
	// worker, then admits a session; when the lead leaves the session is the gate. It stays the gate
	// when it only goes gone: the worker does not take over, the mail waits for the session.
	hroot := t.TempDir()
	hlead := solo("hld-c", hroot)
	agent(hlead, core.AgentArgs{Action: core.AgentFound})
	worker := agent(hlead, core.AgentArgs{Action: core.AgentSpawn, Role: "peer", Name: "w1", Task: "t"})
	sess := solo("ses-d", hroot)
	agent(hlead, core.AgentArgs{Action: core.AgentAdmit, Target: name(sess), Role: "peer"})
	// a mail woke the session as a worker (run headless now): it is still a person's session, not a worker
	if _, err := db.Exec(`UPDATE participants SET mode='headless' WHERE id=?`, sess.ParticipantID); err != nil {
		t.Fatal(err)
	}
	agent(hlead, core.AgentArgs{Action: core.AgentFound})
	var hteam string
	if err := db.QueryRow(`SELECT name FROM teams WHERE id=(SELECT team_id FROM participants WHERE id=?)`,
		sess.ParticipantID).Scan(&hteam); err != nil {
		t.Fatal(err)
	}
	gateOf := func() string {
		t.Helper()
		m, err := e.Send(ctx, s, core.SendArgs{To: hteam, Body: "to the team"})
		if err != nil {
			t.Fatal(err)
		}
		var to string
		if err := db.QueryRow(`SELECT to_id FROM messages WHERE id=?`, m.ID).Scan(&to); err != nil {
			t.Fatal(err)
		}
		return to
	}
	if g := gateOf(); g != sess.ParticipantID {
		t.Fatalf("gate = %s; want the admitted session, not the earlier worker %s", g, worker.ParticipantID)
	}
	if err := e.Presence(ctx, sess, core.PresenceArgs{Event: core.PresenceShutdown}); err != nil {
		t.Fatal(err)
	}
	if g := gateOf(); g != sess.ParticipantID {
		t.Fatalf("gate = %s; want the session still (gone, not left), not the worker %s", g, worker.ParticipantID)
	}

	// Regression (live run): the lead leaves and the dev it took in becomes the gate. The
	// gate reports to no one: its links to the leaver are cleared, never pointed at itself.
	wroot := t.TempDir()
	wlead := solo("wld-a", wroot)
	agent(wlead, core.AgentArgs{Action: core.AgentFound, Template: "w"})
	dev := solo("dev-b", wroot)
	agent(wlead, core.AgentArgs{Action: core.AgentAdmit, Target: name(dev), Role: "dev"})
	agent(wlead, core.AgentArgs{Action: core.AgentFound})
	if n := count(`SELECT COUNT(*) FROM participants WHERE id=? AND reports_to IS NULL AND spawned_by IS NULL`,
		dev.ParticipantID); n != 1 {
		t.Fatal("the dev that became the gate still reports to the leaver")
	}
	if _, err := e.Send(ctx, dev, core.SendArgs{To: name(wlead), Body: "hi"}); !errors.As(err, &ce) ||
		ce.RuleID != "team.left" || ce.Details.(map[string]any)["gate"] != name(dev) || strings.Contains(ce.Message, "send to its gate") {
		t.Fatalf("mail from the gate to the leaver: %v", err)
	}
}

// pull opens c's batch n and returns its message ids (nothing is acked).
func pull(t *testing.T, e *core.Engine, c core.Caller, n int64) []string {
	t.Helper()
	in, err := e.Inbox(ctx, c, core.InboxArgs{Batch: &n})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range in {
		ids = append(ids, m.ID)
	}
	return ids
}
