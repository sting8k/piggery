package core_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// A sender's cc: each name is checked as a send to it would be, and one that would be refused (no
// routing rule, another team, no such name) is left out and told to the sender without failing
// the send. The rest get a copy, once even when routing cc names them too; the original's reader
// sees every copy's recipient. To another team's gate no cc goes at all. One string works as a list.
func TestSenderCC(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	const man = `
template: cc
roles: {a: {tools: [send, inbox, who]}, b: {tools: [send, inbox, who]}, c: {tools: [send, inbox, who]}, d: {tools: [send, inbox, who]}, e: {tools: [send, inbox, who]}}
routing:
  - {from: a, to: b, allow: true, cc: [e]}
  - {from: a, to: c, allow: true}
  - {from: a, to: e, allow: true}
`
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	join := func(name, role string) core.Caller {
		j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: role, Name: name, Cwd: team.RootCwd})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	a1, b1, c1, d1, e1 := join("a1", "a"), join("b1", "b"), join("c1", "c"), join("d1", "d"), join("e1", "e")
	sj, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: t.TempDir(), Harness: "pi", Mode: "rpc", HarnessRef: "lone", Name: "lone"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := func(c core.Caller) []core.Delivered {
		t.Helper()
		in, err := e.Inbox(ctx, c, core.InboxArgs{})
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	refused := func(r core.SendResult) map[string]string {
		got := map[string]string{}
		for _, c := range r.CcRefused {
			got[c.To] = c.RuleID
		}
		return got
	}

	// d1: no rule a -> d. lone: another team. ghost: no one. a1 and b1 have the mail already.
	r, err := e.Send(ctx, a1, core.SendArgs{To: "b1", Body: "plan", CC: core.Names{"c1", "e1", "d1", "lone", "ghost", "a1", "b1"}})
	if err != nil {
		t.Fatalf("a refused cc name failed the send: %v", err)
	}
	if got := refused(r); len(got) != 3 || got["d1"] != "routing.no_rule" || got["lone"] != "cc.cross_team" || got["ghost"] != "visibility.unknown_target" {
		t.Fatalf("refused = %+v", r.CcRefused)
	}
	for _, c := range []core.Caller{c1, e1} {
		if in := inbox(c); len(in) != 1 || in[0].CcOf != r.ID || len(in[0].Cc) != 1 { // the other copy, not itself
			t.Fatalf("copy = %+v", in)
		}
	}
	if in := inbox(d1); len(in) != 0 {
		t.Fatalf("a refused name got a copy: %+v", in)
	}
	if in := inbox(b1); len(in) != 1 || in[0].ID != r.ID || !slices.Equal(in[0].Cc, []string{"c1 (c)", "e1 (e)"}) {
		t.Fatalf("recipient sees %+v", in)
	}

	// One string, as a harness may hand it over.
	var a core.SendArgs
	if err := json.Unmarshal([]byte(`{"to":"b1","body":"again","cc":"c1"}`), &a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, a1, a); err != nil {
		t.Fatal(err)
	}
	if in := inbox(c1); len(in) != 2 { // the first copy is still unacked
		t.Fatalf("c1 after a string cc: %+v", in)
	}

	// To another team's gate (a1 is its team's gate; lone a solo): the send goes, nothing is copied.
	solo, _ := e.Authenticate(ctx, sj.ID, sj.Token)
	r, err = e.Send(ctx, a1, core.SendArgs{To: "lone", Body: "hi", CC: core.Names{"c1"}})
	if err != nil || refused(r)["c1"] != "cc.cross_team" || len(inbox(c1)) != 2 || len(inbox(solo)) != 1 {
		t.Fatalf("cross-team send: %+v, %v", r, err)
	}
}
