package core_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// The inbox history views are read-only: reading mail to me through `recent` and `message` records
// no delivery and opens no batch, so the mail is still unacked and the next inbox gives it as new.
// Held mail to me is not in them (it would bypass the hold); my own held mail is, marked. `before`
// pages back.
func TestInboxHistory(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	const man = `
template: h
roles: {a: {tools: [send, inbox]}, b: {tools: [send, inbox]}}
routing:
  - {from: a, to: b, allow: true}
  - {from: b, to: a, allow: true}
limits: {messages_per_participant_per_minute: 12}
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
	a1, b1 := join("a1", "a"), join("b1", "b")
	send := func(from core.Caller, to, body string) core.SendResult {
		t.Helper()
		r, err := e.Send(ctx, from, core.SendArgs{To: to, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var seqs []int64
	for i := 1; i <= 12; i++ {
		seqs = append(seqs, send(a1, "b1", fmt.Sprintf("note %d", i)).Seq)
	}
	held := send(a1, "b1", "held note") // the 13th in the minute
	if !held.Held {
		t.Fatalf("not held: %+v", held)
	}
	reply := send(b1, "a1", "got them") // after the engine's hold notice to a1

	history := func(c core.Caller, a core.HistoryArgs) string {
		t.Helper()
		got, err := e.History(ctx, c, a)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	recent := history(b1, core.HistoryArgs{View: "recent"})
	lines := strings.Split(recent, "\n")
	// the reply I sent, then notes 12..4; the 11th line says how to go on
	if len(lines) != 11 || !strings.HasPrefix(lines[0], fmt.Sprintf("#%d ", reply.Seq)) || !strings.Contains(lines[0], "→ a1") ||
		!strings.Contains(lines[1], "← a1") || !strings.Contains(lines[1], "unread: note 12") || strings.Contains(recent, "held note") ||
		!strings.Contains(lines[10], fmt.Sprintf("before=#%d", seqs[3])) {
		t.Fatalf("recent =\n%s", recent)
	}
	older := strings.Split(history(b1, core.HistoryArgs{View: "recent", Before: fmt.Sprintf("#%d", seqs[3])}), "\n")
	if len(older) != 3 || !strings.HasSuffix(older[0], "note 3") || !strings.HasSuffix(older[2], "note 1") {
		t.Fatalf("older = %q", older)
	}
	one := history(b1, core.HistoryArgs{View: "message", ID: fmt.Sprintf("#%d", seqs[0])})
	if !strings.Contains(one, `unread="true"`) || !strings.Contains(one, "\nnote 1\n") {
		t.Fatalf("message = %s", one)
	}
	var de *core.Error
	if _, err := e.History(ctx, b1, core.HistoryArgs{View: "message", ID: fmt.Sprintf("#%d", held.Seq)}); !errors.As(err, &de) || de.RuleID != "message.not_in_view" {
		t.Fatalf("held mail to me by id: %v", err)
	}
	if mine := history(a1, core.HistoryArgs{View: "recent"}); !strings.Contains(mine, "→ b1 held: held note") {
		t.Fatalf("sender's recent =\n%s", mine)
	}

	var deliveries, batches int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 || batches != 0 {
		t.Fatalf("history wrote %d deliveries, %d batches", deliveries, batches)
	}
	in, err := e.Inbox(ctx, b1, core.InboxArgs{})
	if err != nil || len(in) != 12 || in[0].Redelivered {
		t.Fatalf("inbox after history: %d messages (redelivered %v), %v; want the 12 as new", len(in), len(in) > 0 && in[0].Redelivered, err)
	}
}
