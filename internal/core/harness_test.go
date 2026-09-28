package core_test

import (
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

// The daemon's batch and ack rule for adapter events: a turn's mail is acked only by an ok turn_end
// with the turn's own key on the current run. A failed turn, another turn's key, and a turn left
// without an end (a new turn_start) ack nothing and do not wake; mail that arrives during a turn
// does not wake either, it is given at the next tool call or by blocking the end. The model's inbox
// during a turn reads into the turn.
func TestAdapterTurnAcksOnlyItsOwnEnd(t *testing.T) {
	var woken []string
	f := newFixture(t, nil, core.WithNotify(func(id string) { woken = append(woken, id) }),
		core.WithAbortPush(func(string) int { return 1 }))
	ev := func(a core.HarnessEventArgs) core.HarnessEventResult {
		t.Helper()
		r, err := f.e.HarnessEvent(ctx, f.bob, a)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	turn := func(id string) core.HarnessEventResult {
		return ev(core.HarnessEventArgs{Event: core.HarnessTurnStart, PromptID: id})
	}
	end := func(id, outcome string) core.HarnessEventResult {
		return ev(core.HarnessEventArgs{Event: core.HarnessTurnEnd, PromptID: id, Outcome: outcome})
	}
	pending := func() int {
		t.Helper()
		var n int
		f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NULL`, f.bob.ParticipantID).Scan(&n)
		return n
	}

	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "one"})
	if r := turn("p1"); !strings.Contains(r.Text, "one") {
		t.Fatalf("turn_start text = %q; want the mail", r.Text)
	}
	if r := end("p1", core.HarnessOutcomeOK); r.Block || pending() != 0 {
		t.Fatalf("ok end = %+v, pending %d; want acked", r, pending())
	}

	// A failed turn acks nothing and does not wake; the mail comes again.
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "two"})
	woken = nil
	turn("p2")
	end("p2", core.HarnessOutcomeFailed)
	if pending() != 1 || len(woken) != 0 {
		t.Fatalf("after a failed turn: pending %d, woken %v", pending(), woken)
	}

	// Mail during a turn does not wake; the model's inbox and the next tool call give it.
	if r := turn("p3"); !strings.Contains(r.Text, "two") {
		t.Fatalf("turn p3 text = %q", r.Text)
	}
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "three"})
	if len(woken) != 0 {
		t.Fatalf("woken during a turn: %v", woken)
	}
	if d := f.inbox(t, f.bob, nil); len(d) != 2 {
		t.Fatalf("inbox during the turn = %d messages", len(d))
	}
	if r := ev(core.HarnessEventArgs{Event: core.HarnessToolBoundary, PromptID: "p3"}); r.Text != "" {
		t.Fatalf("tool boundary gave mail the inbox already gave: %q", r.Text)
	}
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "four"})
	if r := ev(core.HarnessEventArgs{Event: core.HarnessToolBoundary, PromptID: "p3"}); !strings.Contains(r.Text, "four") {
		t.Fatalf("tool boundary = %q; want the new mail", r.Text)
	}
	// Another turn's key ends nothing of this one's mail.
	end("other", core.HarnessOutcomeOK)
	if pending() != 3 {
		t.Fatalf("an end with another key acked: pending %d", pending())
	}

	// A turn with no end (Esc), then a new turn: the old one acks nothing. Mail arriving
	// before the end blocks it once, then the ok end acks everything the turn saw.
	turn("p4")
	turn("p5")
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "five"})
	if r := end("p5", core.HarnessOutcomeOK); !r.Block || !strings.Contains(r.Text, "five") {
		t.Fatalf("end with unseen mail = %+v; want a block with it", r)
	}
	woken = nil
	if r := end("p5", core.HarnessOutcomeOK); r.Block || pending() != 0 || len(woken) != 0 {
		t.Fatalf("second end = %+v, pending %d, woken %v", r, pending(), woken)
	}

	// Mail left after an ok end wakes the idle participant.
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "six"})
	if len(woken) != 1 {
		t.Fatalf("idle participant not woken: %v", woken)
	}

	// The daemon restarted: the adapter reconnects with the same run and the turn ends it could not
	// send. A queued ok end acks its turn; a turn with no end sent is closed unacked, a late end of
	// it acks nothing, and new mail wakes the participant again.
	turn("p5a") // gets six; its end is queued
	if _, err := f.e.Identify(ctx, f.bob, core.IdentifyArgs{RunID: f.bob.RunID,
		Ended: []core.EndedTurn{{Key: "p5a", Outcome: core.HarnessOutcomeOK}}}); err != nil {
		t.Fatal(err)
	}
	if pending() != 0 {
		t.Fatalf("queued ok end: pending %d; want six acked", pending())
	}
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "seven"})
	turn("p5b") // gets seven; its end is lost
	if _, err := f.e.Identify(ctx, f.bob, core.IdentifyArgs{RunID: f.bob.RunID, Capabilities: []string{core.CapAbort}}); err != nil {
		t.Fatal(err)
	}
	end("p5b", core.HarnessOutcomeOK)
	woken = nil
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "eight"})
	if pending() != 2 || len(woken) != 1 {
		t.Fatalf("lost end: pending %d, woken %v; want seven and eight pending, a wake", pending(), woken)
	}

	// Regression (live test): an aborted turn that sends no end held back every later wake. Abort
	// closes it: its mail stays unacked and does not wake, new mail wakes.
	turn("p5c") // gets seven and eight
	woken = nil
	if _, err := f.e.Abort(ctx, core.AdminTarget{Target: "bob"}); err != nil {
		t.Fatal(err)
	}
	if pending() != 2 || len(woken) != 0 {
		t.Fatalf("abort: pending %d, woken %v; want seven and eight unacked, no wake", pending(), woken)
	}
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "nine"})
	if len(woken) != 1 {
		t.Fatalf("mail after an abort: woken %v; want a wake", woken)
	}

	// The harness restarted (new run): the old run's turn end is refused and acks nothing.
	turn("p6")
	if _, err := f.db.Exec(`UPDATE participants SET run_id='run-2' WHERE id=?`, f.bob.ParticipantID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.HarnessEvent(ctx, f.bob, core.HarnessEventArgs{Event: core.HarnessTurnEnd, PromptID: "p6",
		Outcome: core.HarnessOutcomeOK}); code(err) != core.CodeUnauthorized || pending() != 3 {
		t.Fatalf("old run's end: %v, pending %d", err, pending())
	}
}

// Delivery policy: while a participant waits on a permission prompt its mail and wakes are held,
// and go out when the prompt ends (the next tool result, or the end of a pi ui prompt). An Esc
// leaves a turn with no end: idle closes it unacked, without a wake, and new mail wakes again.
func TestAdapterPermissionHoldAndIdle(t *testing.T) {
	var woken []string
	f := newFixture(t, nil, core.WithNotify(func(id string) { woken = append(woken, id) }))
	ev := func(a core.HarnessEventArgs) core.HarnessEventResult {
		t.Helper()
		r, err := f.e.HarnessEvent(ctx, f.bob, a)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	pending := func() int {
		var n int
		f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NULL`, f.bob.ParticipantID).Scan(&n)
		return n
	}

	// A permission prompt mid-turn: mail waits; the tool result that ends the prompt gives it.
	ev(core.HarnessEventArgs{Event: core.HarnessTurnStart, PromptID: "p1"})
	ev(core.HarnessEventArgs{Event: core.HarnessPermission})
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "one"})
	if len(woken) != 0 {
		t.Fatalf("woken during a permission prompt: %v", woken)
	}
	if r := ev(core.HarnessEventArgs{Event: core.HarnessToolBoundary, PromptID: "p1"}); !strings.Contains(r.Text, "one") {
		t.Fatalf("tool result after the prompt: %q; want the held mail", r.Text)
	}
	ev(core.HarnessEventArgs{Event: core.HarnessTurnEnd, PromptID: "p1", Outcome: core.HarnessOutcomeOK})

	// Esc: the turn gets no end; idle closes it unacked and does not wake; new mail wakes.
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "two"})
	woken = nil
	ev(core.HarnessEventArgs{Event: core.HarnessTurnStart, PromptID: "p2"})
	ev(core.HarnessEventArgs{Event: core.HarnessIdle})
	if pending() != 1 || len(woken) != 0 {
		t.Fatalf("idle after Esc: pending %d, woken %v; want two unacked, no wake", pending(), woken)
	}
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "three"})
	if len(woken) != 1 {
		t.Fatalf("mail after idle: woken %v; want a wake", woken)
	}

	// pi: a ui prompt while idle holds the wake; its end releases it.
	woken = nil
	if err := f.e.Presence(ctx, f.bob, core.PresenceArgs{Event: core.PresenceUIPromptStart}); err != nil {
		t.Fatal(err)
	}
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "four"})
	if len(woken) != 0 {
		t.Fatalf("woken during a ui prompt: %v", woken)
	}
	if err := f.e.Presence(ctx, f.bob, core.PresenceArgs{Event: core.PresenceUIPromptEnd}); err != nil || len(woken) != 1 {
		t.Fatalf("ui prompt end: %v, woken %v; want the held wake", err, woken)
	}
}
