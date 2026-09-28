package core_test

import (
	"slices"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

// Mail to notify runs the sink and is done: stored acked, never delivered to anyone.
func TestNotifyHasNoInbox(t *testing.T) {
	var sunk []string
	f := newFixture(t, nil, core.WithNotifySink(func(id string) { sunk = append(sunk, id) }))
	m := f.send(t, f.alice, core.SendArgs{To: core.AddrNotify, Body: "build is red"})
	if !slices.Equal(sunk, []string{m.ID}) {
		t.Fatalf("sink got %v, want [%s]", sunk, m.ID)
	}
	var pending int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=? AND acked_at IS NULL`, m.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("notify mail left pending: %d, %v", pending, err)
	}
	for _, c := range []core.Caller{f.alice, f.bob} {
		if got := f.inbox(t, c, nil); len(got) != 0 {
			t.Fatalf("inbox = %+v, want nothing", got)
		}
	}
}
