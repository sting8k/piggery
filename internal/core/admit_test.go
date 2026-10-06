package core_test

import (
	"slices"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// Admit: a role that can spawn R takes a solo at the team root into R in the same session
// (same participant and run, reports_to the admitter, one admitted event, the harness told to
// identify again). Refused: a target that is not a solo, a solo elsewhere, a role that cannot
// spawn R, a headless worker as the admitter.
func TestAdmit(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const man = "template: m\nauto_join_role: lead\nroles:\n  lead: {tools: [send, inbox, who, agent], can_spawn: [peer, lead]}\n" +
		"  peer: {tools: [send, inbox, who]}\n  guest: {tools: [inbox]}\nlimits: {depth: 2, concurrency: 2}\nrouting: [{from: lead, to: lead, allow: true}]\n"
	var pushed []string
	rt := &fakeRuntime{}
	e := core.New(db, core.WithTemplates(func(string, string) (string, error) { return man, nil }), core.WithRuntime(rt),
		core.WithRoleChanged(func(id string) { pushed = append(pushed, id) }))
	root, elsewhere := t.TempDir(), t.TempDir()
	solo := func(ref, cwd string) core.Caller {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: cwd, Harness: "pi", Mode: "rpc", HarnessRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	lead := solo("lead-1", root)
	if _, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentFound, Template: "m"}); err != nil {
		t.Fatal(err)
	}
	b, far := solo("bee-2", root), solo("far-3", elsewhere)
	admit := func(target, role string) error {
		_, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentAdmit, Target: target, Role: role})
		return err
	}
	name := func(c core.Caller) string {
		var n string
		if err := db.QueryRow(`SELECT name FROM participants WHERE id=?`, c.ParticipantID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := admit(name(far), "peer"); rule(err) != "target/admit.not_at_root" {
		t.Fatalf("solo elsewhere: %v", err)
	}
	if _, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentSpawn, Role: "lead", Name: "w1", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	w1, _ := e.Authenticate(ctx, rt.starts[0].ParticipantID, rt.starts[0].Token)
	if _, err := e.Agent(ctx, w1, core.AgentArgs{Action: core.AgentAdmit, Target: name(b), Role: "peer"}); rule(err) != "permission/admit.headless" {
		t.Fatalf("admit by a headless worker: %v", err)
	}
	if err := admit(name(b), "guest"); rule(err) != "permission/can_spawn" {
		t.Fatalf("role the lead cannot spawn: %v", err)
	}
	if err := admit(name(b), "peer"); err != nil {
		t.Fatal(err)
	}
	if err := admit(name(b), "peer"); rule(err) != "target/admit.not_solo" {
		t.Fatalf("admit a member: %v", err)
	}
	id, err := e.Identify(ctx, b, core.IdentifyArgs{RunID: b.RunID})
	if err != nil || id.Role != "peer" || id.TeamID == "" || !slices.Equal(id.Tools, []string{"send", "inbox", "who"}) {
		t.Fatalf("identify after admit (same run) = %+v, %v", id, err)
	}
	var reportsTo string
	var admitted int
	if err := db.QueryRow(`SELECT reports_to FROM participants WHERE id=?`, b.ParticipantID).Scan(&reportsTo); err != nil ||
		reportsTo != lead.ParticipantID {
		t.Fatalf("reports_to = %q, %v", reportsTo, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='admitted'`).Scan(&admitted); err != nil || admitted != 1 {
		t.Fatalf("admitted events = %d, %v", admitted, err)
	}
	if !slices.Equal(pushed, []string{b.ParticipantID}) {
		t.Fatalf("role pushes = %v", pushed)
	}
}

// The gate is the member that joined the team first, not the participant made first: a solo made
// before the founder and admitted after the team was founded is not the gate, and the member list
// is in that order too (regression: the order was participants.created_at).
func TestGateIsTheFirstToJoin(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const man = "template: m\nauto_join_role: lead\nroles:\n  lead: {tools: [send, inbox, who, agent], can_spawn: [peer]}\n" +
		"  peer: {tools: [send, inbox, who]}\nlimits: {depth: 2, concurrency: 2}\nrouting: [{from: lead, to: peer, allow: true}]\n"
	now := time.Now()
	e := core.New(db, core.WithClock(func() time.Time { now = now.Add(time.Second); return now }),
		core.WithTemplates(func(string, string) (string, error) { return man, nil }))
	root := t.TempDir()
	solo := func(name string) core.Caller {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: root, Harness: "pi", Mode: "rpc", HarnessRef: name, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	early, founder := solo("early"), solo("founder")
	if _, err := e.Agent(ctx, founder, core.AgentArgs{Action: core.AgentFound, Template: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agent(ctx, founder, core.AgentArgs{Action: core.AgentAdmit, Target: "early", Role: "peer"}); err != nil {
		t.Fatal(err)
	}
	s, err := e.State(ctx, core.StateArgs{})
	if err != nil || len(s.Teams) != 1 || len(s.Teams[0].Members) != 2 {
		t.Fatalf("state = %+v, %v", s, err)
	}
	if g := s.Teams[0].Gate; g != "founder" {
		t.Fatalf("gate = %q; want founder (joined first), not %s (made first)", g, early.ParticipantID)
	}
	if m := s.Teams[0].Members; m[0].Name != "founder" || m[1].Name != "early" {
		t.Fatalf("members = %s, %s; want them in the order they joined", m[0].Name, m[1].Name)
	}
}

// After a reopen the session's old member row has left, so the solo row the session went on with
// is admitted as the team's one row of that session (F5: never two entries for one person).
func TestAdmitAfterReopenLeavesOneRowOfTheSession(t *testing.T) {
	f := newStaleRow(t)
	var bName string
	f.db.QueryRow(`SELECT name FROM participants WHERE id=?`, f.b.ParticipantID).Scan(&bName)
	if _, err := f.e.Agent(ctx, f.gate, core.AgentArgs{Action: core.AgentAdmit, Target: bName, Role: "guest"}); err != nil {
		t.Fatalf("admit of the session's solo row: %v", err)
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM participants WHERE harness_ref='x-2' AND left_at IS NULL AND team_id=(SELECT id FROM teams WHERE name=?)`, f.team).Scan(&n)
	if n != 1 {
		t.Fatalf("%d live rows of the session in the team; want 1", n)
	}
}
