package core_test

import (
	"fmt"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

// Models and humans see messages as #N (their seq): reply_to, target and release take #N like an
// id, and an unknown #N is not_found.
func TestMessageRefs(t *testing.T) {
	f := newLimitFixture(t, "{messages_per_participant_per_minute: 3}")
	ref := func(r core.SendResult) string { return fmt.Sprintf("#%d", r.Seq) }

	first := f.send(t, f.alice, "bob", "")
	if reply := f.send(t, f.bob, "alice", ref(first)); reply.ThreadID != first.ThreadID {
		t.Fatalf("reply_to %s: thread %s, want %s", ref(first), reply.ThreadID, first.ThreadID)
	}
	pin, err := f.e.Send(ctx, f.alice, core.SendArgs{To: core.AddrBoard, Body: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Send(ctx, f.alice, core.SendArgs{To: core.AddrBoard, Body: "v2", Op: "replace", Target: ref(pin)}); err != nil {
		t.Fatalf("board replace target %s: %v", ref(pin), err)
	}
	held := f.send(t, f.alice, "bob", "")
	if !held.Held {
		t.Fatalf("not held: %+v", held)
	}
	if err := f.e.Release(ctx, core.ReleaseArgs{ID: ref(held)}); err != nil {
		t.Fatalf("release %s: %v", ref(held), err)
	}
	if in, _ := f.e.Inbox(ctx, f.bob, core.InboxArgs{}); len(in) != 2 || in[1].ID != held.ID {
		t.Fatalf("bob inbox after release %s = %+v", ref(held), in)
	}

	if _, err := f.e.Send(ctx, f.bob, core.SendArgs{To: "alice", Body: "x", ReplyTo: "#999999"}); code(err) != core.CodeNotFound {
		t.Fatalf("reply_to #999999: %v", err)
	}
	if err := f.e.Release(ctx, core.ReleaseArgs{ID: "#999999"}); code(err) != core.CodeNotFound {
		t.Fatalf("release #999999: %v", err)
	}
}
