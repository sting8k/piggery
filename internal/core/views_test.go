package core_test

import (
	"slices"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

func (f fixture) view(t *testing.T, c core.Caller, a core.InboxArgs) []string {
	t.Helper()
	before := f.deliveries(t)
	got, err := f.e.Inbox(ctx, c, a)
	if err != nil {
		t.Fatalf("view %+v: %v", a, err)
	}
	if after := f.deliveries(t); after != before {
		t.Fatalf("view %s recorded %d deliveries; views are read-only", a.View, after-before)
	}
	ids := []string{}
	for _, d := range got {
		ids = append(ids, d.ID)
	}
	return ids
}

func (f fixture) deliveries(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBoardViewIsTheLivePins(t *testing.T) {
	f := newFixture(t, nil)
	p1 := f.send(t, f.alice, core.SendArgs{To: "board", Body: "p1"})
	f.send(t, f.alice, core.SendArgs{To: "board", Body: "p2"})
	f.send(t, f.bob, core.SendArgs{To: "board", Body: "p1 v2", Op: "replace", Target: p1.ID})
	pins, err := f.e.Board(ctx, f.bob)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, p := range pins {
		want = append(want, p.ID)
	}
	if got := f.view(t, f.bob, core.InboxArgs{View: core.ViewBoard}); !slices.Equal(got, want) || len(got) != 2 {
		t.Fatalf("board view = %v, board = %v", got, want)
	}
}
