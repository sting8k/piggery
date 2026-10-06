package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// Mail that is given again after an earlier delivery that was not acked is marked redelivered in
// its header, however young it is; the first delivery is not marked.
func TestRedeliveredMailIsMarked(t *testing.T) {
	f := newFixture(t, nil)
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "do the thing"})
	first, err := f.e.Inbox(ctx, f.bob, core.InboxArgs{})
	if err != nil || len(first) != 1 || first[0].Redelivered {
		t.Fatalf("first delivery: %+v, %v", first, err)
	}
	again, err := f.e.Inbox(ctx, f.bob, core.InboxArgs{})
	if err != nil || len(again) != 1 || !again[0].Redelivered {
		t.Fatalf("second delivery: %+v, %v", again, err)
	}
	now := time.UnixMilli(again[0].CreatedAt).Add(time.Second)
	if got := core.RenderMail(first, "", "", now); strings.Contains(got, "redelivered") {
		t.Fatalf("first delivery rendered as redelivered: %s", got)
	}
	if got := core.RenderMail(again, "", "", now); !strings.Contains(got, ` redelivered="true"`) || strings.Contains(got, "age=") {
		t.Fatalf("redelivery rendered: %s", got)
	}
}
