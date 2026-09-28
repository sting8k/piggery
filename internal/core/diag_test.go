package core_test

import (
	"database/sql"
	"sort"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

type diagFixture struct {
	e    *core.Engine
	db   *sql.DB
	now  *time.Time
	join func(name, role string) core.Caller
}

func newDiagFixture(t *testing.T, man string) diagFixture {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Unix(1_800_000_000, 0)
	e := core.New(db, core.WithClock(func() time.Time { return now }))
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
	return diagFixture{e: e, db: db, now: &now, join: join}
}

func (f diagFixture) events(t *testing.T) int {
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// why runs the same gate as Send: for allow, hold (rate) and deny (routing) the verdict and
// rule match what Send then does, and why itself writes nothing, not even a denied event.
func TestWhyAgreesWithSend(t *testing.T) {
	f := newDiagFixture(t, `
model: why
roles: {a: {tools: [send, inbox, who, agent]}, b: {tools: [send, inbox, who, agent]}, c: {tools: [send, inbox, who, agent]}}
routing:
  - {from: a, to: b, allow: true, cc: [c]}
limits: {messages_per_participant_per_minute: 1}
`)
	a1, b1 := f.join("a1", "a"), f.join("b1", "b")
	f.join("c1", "c")
	why := func(from, to string) core.WhyResult {
		t.Helper()
		before := f.events(t)
		r, err := f.e.Why(ctx, core.WhyArgs{From: from, To: to})
		if err != nil {
			t.Fatal(err)
		}
		if after := f.events(t); after != before {
			t.Fatalf("why %s %s wrote %d events", from, to, after-before)
		}
		return r
	}
	checks := func(r core.WhyResult) map[string]core.GateCheck {
		m := map[string]core.GateCheck{}
		for _, c := range r.Checks {
			m[c.Check] = c
		}
		return m
	}

	r := why("a1", "b1")
	c := checks(r)
	if r.Verdict != "allow" || c["routing"].RuleID != "routing[0]" || c["cc"].Detail != "copies to c1" || c["caller"].Result != "pass" {
		t.Fatalf("why a1 b1 = %+v", r)
	}
	if s, err := f.e.Send(ctx, a1, core.SendArgs{To: "b1", Body: "x"}); err != nil || s.Held {
		t.Fatalf("send after allow: %+v %v", s, err)
	}

	r = why("a1", "b1")
	if r.Verdict != "hold" || r.RuleID != core.RuleRatePerMinute || checks(r)["limits"].Result != "hold" {
		t.Fatalf("why a1 b1 over the rate = %+v", r)
	}
	if s, err := f.e.Send(ctx, a1, core.SendArgs{To: "b1", Body: "x"}); err != nil || !s.Held || s.RuleID != r.RuleID {
		t.Fatalf("send after hold verdict: %+v %v", s, err)
	}

	r = why("b1", "a1")
	if r.Verdict != "deny" || r.Layer != "routing" || r.RuleID != "routing.no_rule" {
		t.Fatalf("why b1 a1 = %+v", r)
	}
	if _, err := f.e.Send(ctx, b1, core.SendArgs{To: "a1", Body: "x"}); rule(err) != "routing/routing.no_rule" {
		t.Fatalf("send after deny verdict: %v", err)
	}
}

// doctor: nothing on a healthy team; then one finding of each kind, and only those.
func TestDoctorFindings(t *testing.T) {
	f := newDiagFixture(t, `
model: doc
roles: {peer: {tools: [send, inbox, who, agent]}}
routing:
  - {from: peer, to: peer, allow: true}
limits: {messages_per_participant_per_minute: 1}
`)
	alice, bob, carol := f.join("alice", "peer"), f.join("bob", "peer"), f.join("carol", "peer")
	dave, erin := f.join("dave", "peer"), f.join("erin", "peer")
	var who map[string]string // finding kind -> first id
	doctor := func() []string {
		t.Helper()
		r, err := f.e.Doctor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		who = map[string]string{}
		for _, x := range r.Findings {
			if len(x.IDs) == 0 || x.IDs[0] == "" {
				t.Fatalf("finding without ids: %+v", x)
			}
			kinds = append(kinds, x.Kind)
			who[x.Kind] = x.IDs[0]
		}
		sort.Strings(kinds)
		return kinds
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Healthy: a delivered, completed batch and settled turns.
	must(f.e.Presence(ctx, alice, core.PresenceArgs{Event: core.PresenceAgentStart}))
	must(f.e.Presence(ctx, alice, core.PresenceArgs{Event: core.PresenceAgentSettled}))
	must(f.e.Presence(ctx, bob, core.PresenceArgs{Event: core.PresenceAgentStart}))
	_, err := f.e.Send(ctx, alice, core.SendArgs{To: "bob", Body: "x"})
	must(err)
	_, err = f.e.Inbox(ctx, bob, core.InboxArgs{Batch: batch(1)})
	must(err)
	_, err = f.e.Completion(ctx, bob, core.CompletionArgs{Batch: 1})
	must(err)
	must(f.e.Presence(ctx, bob, core.PresenceArgs{Event: core.PresenceAgentSettled}))
	if k := doctor(); len(k) != 0 {
		t.Fatalf("healthy team: findings %v", k)
	}

	*f.now = f.now.Add(time.Minute)
	_, err = f.e.Send(ctx, bob, core.SendArgs{To: "carol", Body: "x"}) // unacked mail, then carol leaves
	must(err)
	must(f.e.Presence(ctx, carol, core.PresenceArgs{Event: core.PresenceShutdown}))
	s, err := f.e.Send(ctx, bob, core.SendArgs{To: "alice", Body: "x"}) // over the rate: held
	must(err)
	if !s.Held {
		t.Fatal("second send in a minute not held")
	}
	must(f.e.Presence(ctx, alice, core.PresenceArgs{Event: core.PresenceAgentStart})) // never settles
	_, err = f.e.Inbox(ctx, alice, core.InboxArgs{Batch: batch(1)})                   // never completed
	must(err)
	*f.now = f.now.Add(31 * time.Minute)
	for _, q := range []string{
		// Seeded directly: states the engine does not produce on its own.
		`INSERT INTO batches(run_id, batch_seq, opened_at, completed_at) SELECT run_id, 9, 1, 1 FROM participants WHERE name='bob'`,
		// A turn closed unacked (interrupted): ended, never completed, and not an open batch.
		`INSERT INTO batches(run_id, batch_seq, opened_at, ended_at) SELECT run_id, 1, 1, 2 FROM participants WHERE name='dave'`,
		`UPDATE participants SET mode='headless' WHERE name='dave'`,
		`UPDATE participants SET mode='headless', state='parked' WHERE name='erin'`, // parked is not also "no process"
	} {
		if _, err := f.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"batch_order", "gone_unacked", "headless_no_process", "held", "open_batch", "parked", "unsettled_start"}
	if k := doctor(); !eq(k, want) {
		t.Fatalf("findings %v, want %v", k, want)
	}
	for kind, id := range map[string]string{"gone_unacked": carol.ParticipantID, "headless_no_process": dave.ParticipantID,
		"parked": erin.ParticipantID, "unsettled_start": alice.ParticipantID, "open_batch": alice.ParticipantID} {
		if who[kind] != id {
			t.Errorf("%s points at %s, want %s", kind, who[kind], id)
		}
	}
}
