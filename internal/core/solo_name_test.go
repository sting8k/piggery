package core_test

import (
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
	"github.com/sting8k/piggery/manifests"
)

// A session that was a member of a team that closed joins again as a solo under the name it had.
// With a live solo already holding that name, it is name-2.
func TestSoloKeepsItsNameAfterTeamDown(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	home := t.TempDir()
	if err := manifests.Unpack(home); err != nil {
		t.Fatal(err)
	}
	e := core.New(db, core.WithTemplates(func(name, _ string) (string, error) { return manifests.Resolve(name, home) }))
	dir := t.TempDir()
	join := func(ref, name string) core.JoinResult {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: dir, Harness: "pi", Mode: "rpc", HarnessRef: ref, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	nameOf := func(id string) (name string) {
		t.Helper()
		if err := db.QueryRow(`SELECT name FROM participants WHERE id=?`, id).Scan(&name); err != nil {
			t.Fatal(err)
		}
		return name
	}
	for _, c := range []struct {
		ref, name, holder, want string
	}{{"keep-1", "keeper", "", "keeper"}, {"keep-2", "ranger", "ranger", "ranger-2"}} {
		first := join(c.ref, c.name)
		fc, _ := e.Authenticate(ctx, first.ID, first.Token)
		r, err := e.Agent(ctx, fc, core.AgentArgs{Action: core.AgentFound, Template: "lead-peer"})
		if err != nil {
			t.Fatal(err)
		}
		if c.holder != "" { // a live solo takes the name while the team is open
			join("holder-"+c.ref, c.holder)
		}
		if _, err := e.TeamDown(ctx, core.TeamDownArgs{Team: r.TeamID}); err != nil {
			t.Fatal(err)
		}
		again := join(c.ref, "")
		if again.ID == first.ID || nameOf(again.ID) != c.want || nameOf(first.ID) != c.name {
			t.Fatalf("%s: solo %s is %q (member was %q); want %q", c.ref, again.ID, nameOf(again.ID), nameOf(first.ID), c.want)
		}
		// a send by that name reaches the solo: the closed team's gone member is not a second match
		sj := join("sender-"+c.ref, "sender-"+c.ref)
		sc, _ := e.Authenticate(ctx, sj.ID, sj.Token)
		if _, err := e.Send(ctx, sc, core.SendArgs{To: c.want, Body: "hi"}); err != nil {
			t.Fatalf("%s: send to %q: %v", c.ref, c.want, err)
		}
	}
}
