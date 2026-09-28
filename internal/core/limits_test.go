package core_test

import (
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

type limitFixture struct {
	e          *core.Engine
	alice, bob core.Caller
	now        *time.Time
	notified   *[]string
}

func newLimitFixture(t *testing.T, limits string) limitFixture {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Unix(1_800_000_000, 0)
	var notified []string
	e := core.New(db, core.WithClock(func() time.Time { return now }),
		core.WithNotify(func(id string) { notified = append(notified, id) }))
	man := "model: lim\nroles: {peer: {can_pin: true, tools: [send, inbox, who, agent]}}\nrouting:\n  - {from: peer, to: peer, allow: true}\nlimits: " + limits + "\n"
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	join := func(name string) core.Caller {
		j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "peer", Name: name, Cwd: team.RootCwd})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	return limitFixture{e: e, alice: join("alice"), bob: join("bob"), now: &now, notified: &notified}
}

func (f limitFixture) send(t *testing.T, from core.Caller, to, replyTo string) core.SendResult {
	t.Helper()
	r, err := f.e.Send(ctx, from, core.SendArgs{To: to, Body: "x", ReplyTo: replyTo})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// heldTwice sends twice over the limit and checks: both held by rule, absent from the
// recipient's inbox, the recipient not woken, and exactly one notice from engine to the sender.
func (f limitFixture) heldTwice(t *testing.T, sender, recipient core.Caller, rule string, send func() core.SendResult) {
	t.Helper()
	*f.notified = nil
	over := []core.SendResult{send(), send()}
	for _, r := range over {
		if !r.Held || r.RuleID != rule {
			t.Fatalf("over-limit send = %+v, want held by %s", r, rule)
		}
	}
	for _, id := range *f.notified {
		if id == recipient.ParticipantID {
			t.Fatal("recipient woken for held mail")
		}
	}
	in, err := f.e.Inbox(ctx, recipient, core.InboxArgs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range in {
		if m.ID == over[0].ID || m.ID == over[1].ID {
			t.Fatalf("held message %s delivered", m.ID)
		}
	}
	mine, err := f.e.Inbox(ctx, sender, core.InboxArgs{})
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, m := range mine {
		if m.From == core.AddrEngine {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("sender got %d notices, want 1", notices)
	}
}

func TestMaxHopsHolds(t *testing.T) {
	f := newLimitFixture(t, "{max_hops: 2}")
	m1 := f.send(t, f.alice, "bob", "")
	m2 := f.send(t, f.bob, "alice", m1.ID)
	m3 := f.send(t, f.alice, "bob", m2.ID) // chain of 2: allowed
	if m3.Held {
		t.Fatalf("hop 2 held: %+v", m3)
	}
	f.heldTwice(t, f.bob, f.alice, core.RuleMaxHops, func() core.SendResult { return f.send(t, f.bob, "alice", m3.ID) })
}

func TestRatePerMinuteHolds(t *testing.T) {
	f := newLimitFixture(t, "{messages_per_participant_per_minute: 2}")
	f.send(t, f.alice, "bob", "")
	f.send(t, f.alice, "board", "") // a pin is never held but counts toward the rate
	f.heldTwice(t, f.alice, f.bob, core.RuleRatePerMinute, func() core.SendResult { return f.send(t, f.alice, "bob", "") })
	*f.now = f.now.Add(61 * time.Second)
	if r := f.send(t, f.alice, "bob", ""); r.Held {
		t.Fatalf("next minute still held: %+v", r)
	}
}

func TestThreadCapHolds(t *testing.T) {
	f := newLimitFixture(t, "{messages_per_thread: 2}")
	m1 := f.send(t, f.alice, "bob", "")
	m2 := f.send(t, f.bob, "alice", m1.ID)
	f.heldTwice(t, f.alice, f.bob, core.RuleThreadCap, func() core.SendResult { return f.send(t, f.alice, "bob", m2.ID) })
	if r := f.send(t, f.alice, "bob", ""); r.Held { // a new thread is fine
		t.Fatalf("new thread held: %+v", r)
	}
}

// Release delivers a held message and wakes its recipient; engine mail is never held.
func TestReleaseDeliversAndEngineIsExempt(t *testing.T) {
	f := newLimitFixture(t, "{messages_per_participant_per_minute: 1}")
	f.send(t, f.alice, "bob", "")
	held := f.send(t, f.alice, "bob", "")
	if !held.Held {
		t.Fatalf("not held: %+v", held)
	}
	*f.notified = nil
	if err := f.e.Release(ctx, core.ReleaseArgs{ID: held.ID}); err != nil {
		t.Fatal(err)
	}
	if len(*f.notified) != 1 || (*f.notified)[0] != f.bob.ParticipantID {
		t.Fatalf("notified %v, want bob", *f.notified)
	}
	in, _ := f.e.Inbox(ctx, f.bob, core.InboxArgs{})
	if len(in) != 2 || in[1].ID != held.ID {
		t.Fatalf("bob inbox after release = %+v", in)
	}
	if err := f.e.Release(ctx, core.ReleaseArgs{ID: held.ID}); code(err) != core.CodeInvalid {
		t.Fatalf("release of a message that is not held: %v", err)
	}
	if err := f.e.Release(ctx, core.ReleaseArgs{ID: "nope"}); code(err) != core.CodeNotFound {
		t.Fatalf("release of an unknown message: %v", err)
	}

	// Timers fire as engine: exempt from alice's exhausted rate, and no notice follows.
	for i := 0; i < 2; i++ {
		if _, err := f.e.WatchAdd(ctx, f.alice, core.TimerArgs{To: "bob", Body: "tick"}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := f.e.FireDue(ctx); err != nil || n != 2 {
		t.Fatalf("FireDue = %d, %v", n, err)
	}
	if in, _ := f.e.Inbox(ctx, f.bob, core.InboxArgs{}); len(in) != 4 {
		t.Fatalf("bob inbox with engine mail = %d messages, want 4", len(in))
	}
	mine, _ := f.e.Inbox(ctx, f.alice, core.InboxArgs{})
	if len(mine) != 1 { // the one rate notice, nothing for the timers
		t.Fatalf("alice notices = %+v", mine)
	}
}
