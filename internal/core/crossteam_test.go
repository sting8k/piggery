package core_test

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// Teams anywhere on the machine see each other and talk only gate to gate (a gate: the
// live participant that joined earliest): a non-gate sender is denied with its own gate's
// name, a non-gate recipient with the other gate's; the receiving team's routing is not
// consulted; the gate answers the other gate; a solo is its own gate; when a gate is gone the
// next one takes over. why shows the team-gate step.
func TestTeamGate(t *testing.T) {
	real := func(d string) string { // team roots are stored with symlinks resolved
		r, err := filepath.EvalSymlinks(d)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	parent, child := real(t.TempDir()), real(t.TempDir()) // unrelated directories
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	up := func(name, man, root string, members ...string) []core.Caller {
		t.Helper()
		team, err := e.TeamUp(ctx, core.TeamUpArgs{Name: name, Manifest: man, Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		var cs []core.Caller
		for _, m := range members {
			j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "peer", Name: m, Cwd: root})
			if err != nil {
				t.Fatal(err)
			}
			c, _ := e.Authenticate(ctx, j.ID, j.Token)
			cs = append(cs, c)
		}
		return cs
	}
	// The parent team routes nothing, even inside: cross-team mail must not depend on it.
	x := up("x", "template: closed\nroles: {peer: {tools: [send, inbox, who]}}\n", parent, "x1", "x2")
	y := up("y", "template: open\nroles: {peer: {tools: [send, inbox, who]}}\nrouting:\n  - {from: peer, to: peer, allow: true}\n", child, "y1", "y2")

	// A solo standing at y's root: y sees it as admittable.
	sj, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: child, Harness: "pi", Mode: "rpc", HarnessRef: "solo", Name: "lone"})
	if err != nil {
		t.Fatal(err)
	}
	solo, _ := e.Authenticate(ctx, sj.ID, sj.Token)
	lines := func(c core.Caller) []string {
		t.Helper()
		who, err := e.Who(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range who {
			l := p.Kind + " " + p.Name
			switch {
			case p.Kind == core.WhoTeam:
				l += " gate=" + p.GateName + " cwd=" + p.Cwd
			case p.Gate && p.Kind == core.WhoMember:
				l += " gate"
			case p.Admittable:
				l += " admittable"
			}
			out = append(out, l)
		}
		return out
	}
	if got, want := lines(y[1]), []string{"member y1 gate", "member y2", "team x gate=x1 cwd=" + parent, "solo lone admittable"}; !slices.Equal(got, want) {
		t.Fatalf("who from y2 = %q, want %q", got, want)
	}
	if got, want := lines(solo), []string{"solo lone", "team x gate=x1 cwd=" + parent, "team y gate=y1 cwd=" + child}; !slices.Equal(got, want) {
		t.Fatalf("who from the solo = %q, want %q", got, want)
	}

	var ce *core.Error
	denied := func(from core.Caller, to, rule, gate string) {
		t.Helper()
		_, err := e.Send(ctx, from, core.SendArgs{To: to, Body: "hi"})
		if !errors.As(err, &ce) || ce.RuleID != rule || ce.Details.(map[string]any)["gate"] != gate {
			t.Fatalf("send to %s: %v, want %s naming %s", to, err, rule, gate)
		}
	}
	denied(y[1], "x1", "team_gate.sender_not_gate", "y1")
	denied(y[0], "x2", "team_gate.not_gate", "x1")
	ask, err := e.Send(ctx, y[0], core.SendArgs{To: "x1", Body: "can you review?"})
	if err != nil {
		t.Fatalf("gate to gate: %v", err)
	}
	in, err := e.Inbox(ctx, x[0], core.InboxArgs{})
	if err != nil || len(in) != 1 || in[0].FromLabel != "y1 (peer, team y)" {
		t.Fatalf("gate inbox = %+v, %v", in, err)
	}
	if _, err := e.Send(ctx, x[0], core.SendArgs{To: "y1", Body: "yes", ReplyTo: ask.ID}); err != nil {
		t.Fatalf("gate reply to the other gate: %v", err)
	}
	byName, err := e.Send(ctx, y[0], core.SendArgs{To: "x", Body: "to the team"}) // a team's name is its gate
	var to string
	if err != nil || db.QueryRow(`SELECT to_id FROM messages WHERE id=?`, byName.ID).Scan(&to) != nil || to != x[0].ParticipantID {
		t.Fatalf("send to team name x: to %s, %v; want x1", to, err)
	}

	why, err := e.Why(ctx, core.WhyArgs{From: "y1", To: "x1"})
	if err != nil || why.Verdict != "allow" || why.Checks[2].Check != "team_gate" {
		t.Fatalf("why y1 x1 = %+v, %v", why, err)
	}
	if why, err := e.Why(ctx, core.WhyArgs{From: "y2", To: "x1"}); err != nil || why.RuleID != "team_gate.sender_not_gate" {
		t.Fatalf("why y2 x1 = %+v, %v", why, err)
	}

	if _, err := e.Send(ctx, solo, core.SendArgs{To: "x1", Body: "hello from a solo"}); err != nil {
		t.Fatalf("a solo (its own gate) to a gate: %v", err)
	}

	if err := e.Presence(ctx, x[0], core.PresenceArgs{Event: core.PresenceShutdown}); err != nil {
		t.Fatal(err)
	}
	// A gone gate stays the gate (stored): x2 does not take over, mail to the team
	// is queued for x1.
	denied(y[0], "x2", "team_gate.not_gate", "x1")
	if _, err := e.Send(ctx, y[0], core.SendArgs{To: "x", Body: "hi"}); err != nil {
		t.Fatalf("x1 gone, still the gate: mail to the team is queued: %v", err)
	}

	// Only roles with send can be the gate: in a team whose planner is gone, the gate is still the
	// planner, not the live dev.
	pd, err := e.TeamUp(ctx, core.TeamUpArgs{Name: "pd", Cwd: t.TempDir(), Manifest: "template: pd\n" +
		"roles: {planner: {tools: [send, inbox, who]}, dev: {tools: [inbox, who]}}\n"})
	if err != nil {
		t.Fatal(err)
	}
	pdJoin := func(name, role string) core.Caller {
		t.Helper()
		j, err := e.Join(ctx, core.JoinArgs{Team: pd.ID, Role: role, Name: name, Cwd: pd.RootCwd})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	pdJoin("dev1", "dev") // joined first, but its role has no send: it is not the gate
	planner := pdJoin("planner", "planner")
	if err := e.Presence(ctx, planner, core.PresenceArgs{Event: core.PresenceShutdown}); err != nil {
		t.Fatal(err)
	}
	denied(y[0], "dev1", "team_gate.not_gate", "planner")
}
