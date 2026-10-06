package core_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// The operator snapshot: a team with its gate, a headless worker (with its model), a solo, held
// and unacked mail counted by recipient, the latest events; and reading it writes nothing.
func TestState(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const man = "template: m\nauto_join_role: lead\nroles:\n  lead: {tools: [send, inbox, who, agent], can_spawn: [w]}\n" +
		"  w: {tools: [send, inbox], spawn: {model: m-w}}\nrouting: [{from: lead, to: w, allow: true}]\n" +
		"limits: {depth: 2, concurrency: 2, messages_per_participant_per_minute: 2}\n"
	rt := &fakeRuntime{}
	e := core.New(db, core.WithRuntime(rt), core.WithTemplates(func(string, string) (string, error) { return man, nil }))
	solo := func(ref, cwd string) core.Caller {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: cwd, Harness: "pi", Mode: "rpc", HarnessRef: ref, Name: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	lead := solo("lead-1", t.TempDir())
	if _, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentFound}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentSpawn, Role: "w", Name: "w1", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	solo("sol-2", t.TempDir())
	// The task counts toward the rate (2/min): "a" goes, "b" is held and the lead gets a notice.
	for _, body := range []string{"a", "b"} {
		if _, err := e.Send(ctx, lead, core.SendArgs{To: "w1", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	s, err := e.State(ctx, core.StateArgs{Events: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Teams) != 1 || len(s.Solos) != 1 || s.Solos[0].Name != "sol-2" {
		t.Fatalf("state = %+v", s)
	}
	tm := s.Teams[0]
	if tm.Template != "m" || tm.Gate != "lead-1" || tm.Held != 1 || tm.Unacked != 3 || len(tm.Members) != 2 {
		t.Fatalf("team = %+v; want template m, gate lead-1, 1 held, 3 unacked (task, a, notice), 2 members", tm)
	}
	byName := map[string]core.MemberState{}
	for _, m := range tm.Members {
		byName[m.Name] = m
	}
	if l, w := byName["lead-1"], byName["w1"]; !l.Gate || l.Headless || l.Unacked != 1 || l.Model != "" || !w.Headless || w.Gate || w.Unacked != 2 ||
		w.Model != "m-w" {
		t.Fatalf("members = %+v", tm.Members)
	}
	var bSeq int64
	if err := db.QueryRow(`SELECT seq FROM messages WHERE body='b'`).Scan(&bSeq); err != nil {
		t.Fatal(err)
	}
	held := false
	for _, ev := range s.Events {
		if ev.Participant != "" && s.Names[ev.Participant] == "" {
			t.Fatalf("no name for event actor %s: %v", ev.Participant, s.Names)
		}
		if ev.Type == "held" {
			held = true
			if want := fmt.Sprintf("#%d", bSeq); s.Names[ev.RefID] != want {
				t.Fatalf("held's message ref labelled %q, want %s", s.Names[ev.RefID], want)
			}
		}
	}
	if !held {
		t.Fatalf("no held event in %+v", s.Events)
	}
	if s.Held != 1 || s.Unacked != 3 || len(s.Events) != 3 || s.Events[2].Seq <= s.Events[0].Seq {
		t.Fatalf("totals held %d unacked %d, events %+v", s.Held, s.Unacked, s.Events)
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&after); err != nil || after != before {
		t.Fatalf("reading state wrote events: %d -> %d, %v", before, after, err)
	}
}

// Every participant list a person or a model reads is in the order they came, oldest first, not
// by name: who (team and solos) and ps (members and solos) with zed before amy.
func TestListsAreInJoinOrder(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	e := core.New(db, core.WithClock(func() time.Time { now = now.Add(time.Second); return now }))
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: "template: m\nroles: {peer: {tools: [send, who]}}\n", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var zed core.Caller
	for _, name := range []string{"zed", "amy"} {
		j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "peer", Name: name, Cwd: team.RootCwd})
		if err != nil {
			t.Fatal(err)
		}
		if name == "zed" {
			zed, _ = e.Authenticate(ctx, j.ID, j.Token)
		}
	}
	var me core.Caller
	for _, ref := range []string{"me", "zed-solo", "amy-solo"} {
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: t.TempDir(), Harness: "pi", Mode: "rpc", HarnessRef: ref, Name: ref})
		if err != nil {
			t.Fatal(err)
		}
		if ref == "me" {
			me, _ = e.Authenticate(ctx, j.ID, j.Token)
		}
	}
	names := func(ps []core.Presence) (out []string) {
		for _, p := range ps {
			if p.Kind != core.WhoTeam {
				out = append(out, p.Name)
			}
		}
		return out
	}
	if w, err := e.Who(ctx, zed); err != nil || !slices.Equal(names(w)[:2], []string{"zed", "amy"}) {
		t.Fatalf("who in the team: %v %v", names(w), err)
	}
	if w, err := e.Who(ctx, me); err != nil || !slices.Equal(names(w), []string{"me", "zed-solo", "amy-solo"}) { // yourself, then the others
		t.Fatalf("who as a solo: %v %v", names(w), err)
	}
	s, err := e.State(ctx, core.StateArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var members, solos []string
	for _, m := range s.Teams[0].Members {
		members = append(members, m.Name)
	}
	for _, so := range s.Solos {
		solos = append(solos, so.Name)
	}
	if !slices.Equal(members, []string{"zed", "amy"}) || !slices.Equal(solos, []string{"me", "zed-solo", "amy-solo"}) {
		t.Fatalf("ps: members %v, solos %v", members, solos)
	}
}
