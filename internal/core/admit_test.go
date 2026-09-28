package core_test

import (
	"slices"
	"testing"

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
	const man = "model: m\nauto_join_role: lead\nroles:\n  lead: {tools: [send, inbox, who, agent], can_spawn: [peer, lead]}\n" +
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
