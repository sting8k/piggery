package core_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

var ctx = context.Background()

type fixture struct {
	e          *core.Engine
	db         *sql.DB
	alice, bob core.Caller
}

// newFixture brings up the p2p team with alice and bob on db (in-memory when nil).
func newFixture(t *testing.T, db *sql.DB, opts ...core.Option) fixture {
	t.Helper()
	if db == nil {
		var err error
		if db, err = store.OpenMemory(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
	}
	e := core.New(db, opts...)
	man, err := os.ReadFile("../../manifests/p2p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: string(man), Name: "p2p", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	join := func(name string) core.Caller {
		j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "peer", Name: name, Cwd: team.RootCwd})
		if err != nil {
			t.Fatal(err)
		}
		c, err := e.Authenticate(ctx, j.ID, j.Token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return fixture{e: e, db: db, alice: join("alice"), bob: join("bob")}
}

func (f fixture) send(t *testing.T, from core.Caller, a core.SendArgs) core.SendResult {
	t.Helper()
	r, err := f.e.Send(ctx, from, a)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f fixture) inbox(t *testing.T, c core.Caller, batch *int64) []core.Delivered {
	t.Helper()
	d, err := f.e.Inbox(ctx, c, core.InboxArgs{Batch: batch})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func batch(n int64) *int64 { return &n }

func code(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

func ids(ds []core.Delivered) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// #2: the engine stamps the sender header; who lists both with state.
func TestSendStampsSenderAndWhoListsState(t *testing.T) {
	f := newFixture(t, nil)
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "hi"})
	got := f.inbox(t, f.bob, nil)
	if len(got) != 1 || got[0].From != f.alice.ParticipantID || got[0].FromLabel != "alice (peer)" {
		t.Fatalf("inbox = %+v", got)
	}
	who, err := f.e.Who(ctx, f.bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(who) != 2 || who[0].Name != "alice" || who[1].Name != "bob" || who[0].State != "idle" {
		t.Fatalf("who = %+v", who)
	}
}

// #3: a retried client_msg_id returns the original id and stores nothing new.
func TestSendDedupesClientMsgID(t *testing.T) {
	f := newFixture(t, nil)
	a := core.SendArgs{To: "bob", Body: "hi", ClientMsgID: "k1"}
	first := f.send(t, f.alice, a)
	again := f.send(t, f.alice, a)
	if again.ID != first.ID || !again.Duplicate {
		t.Fatalf("retry = %+v, first = %+v", again, first)
	}
	if n := len(f.inbox(t, f.bob, nil)); n != 1 {
		t.Fatalf("bob has %d messages, want 1", n)
	}
}

// #4: only Completion(current run, batch) acks, and only that batch's deliveries.
func TestCompletionAcksOnlyItsBatchOfCurrentRun(t *testing.T) {
	f := newFixture(t, nil)
	m1 := f.send(t, f.alice, core.SendArgs{To: "bob", Body: "one"}).ID

	// A pull delivery is never ackable: completing batch 1 acks nothing, mail stays.
	f.inbox(t, f.bob, nil)
	if r, err := f.e.Completion(ctx, f.bob, core.CompletionArgs{Batch: 1}); err != nil || len(r.Acked) != 0 {
		t.Fatalf("completion 1 = %+v, %v", r, err)
	}
	if _, err := f.e.Inbox(ctx, f.bob, core.InboxArgs{Batch: batch(1)}); code(err) != core.CodeBatchClosed {
		t.Fatalf("inbox into closed batch: %v", err)
	}
	if evs, _ := f.e.Log(ctx, core.LogArgs{}); payloadStr(evs[len(evs)-1].Payload, "rule_id") != "batch.closed" {
		t.Fatalf("batch_closed refusal has no denied event: %+v", evs[len(evs)-1])
	}

	// Batch 2 carries m1; repeating the inbox does not duplicate the delivery.
	d := f.inbox(t, f.bob, batch(2))
	if again := f.inbox(t, f.bob, batch(2)); !eq(ids(d), []string{m1}) || again[0].DeliveryID != d[0].DeliveryID {
		t.Fatalf("batch 2 = %+v, again = %+v", d, again)
	}
	m2 := f.send(t, f.alice, core.SendArgs{To: "bob", Body: "two"}).ID
	r, err := f.e.Completion(ctx, f.bob, core.CompletionArgs{Batch: 2})
	if err != nil || !eq(r.Acked, []string{m1}) {
		t.Fatalf("completion 2 = %+v, %v (m2 was never delivered in batch 2)", r, err)
	}
	if d := f.inbox(t, f.bob, batch(3)); !eq(ids(d), []string{m2}) {
		t.Fatalf("batch 3 = %v, want only m2", ids(d))
	}
	if _, err := f.e.Inbox(ctx, f.bob, core.InboxArgs{Batch: batch(5)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Inbox(ctx, f.bob, core.InboxArgs{Batch: batch(4)}); code(err) != core.CodeBatchOrder {
		t.Fatalf("opening 4 after 5: %v", err)
	}

	// The harness restarted: the old run's completion acks nothing.
	if _, err := f.db.Exec(`UPDATE participants SET run_id='run-2' WHERE id=?`, f.bob.ParticipantID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Completion(ctx, f.bob, core.CompletionArgs{Batch: 3}); code(err) != core.CodeUnauthorized {
		t.Fatalf("stale completion: %v", err)
	}
	bob2 := f.bob
	bob2.RunID = "run-2"
	if d := f.inbox(t, bob2, nil); !eq(ids(d), []string{m2}) {
		t.Fatalf("after stale completion inbox = %v, want m2 still pending", ids(d))
	}

	// m2 delivered in batches 1 and 2 of the new run: completing 2 then 1 acks it once.
	f.inbox(t, bob2, batch(1))
	f.inbox(t, bob2, batch(2))
	if r, err := f.e.Completion(ctx, bob2, core.CompletionArgs{Batch: 2}); err != nil || !eq(r.Acked, []string{m2}) {
		t.Fatalf("completion 2 = %+v, %v", r, err)
	}
	if r, err := f.e.Completion(ctx, bob2, core.CompletionArgs{Batch: 1}); err != nil || len(r.Acked) != 0 {
		t.Fatalf("completion 1 re-acked: %+v, %v", r, err)
	}
}

// #5: a refusal commits a denied event with rule_id and layer, and stores no message.
func TestDeniedSendCommitsEvent(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.e.Send(ctx, f.alice, core.SendArgs{To: "nobody", Body: "hi"})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeDenied || ce.Layer != "visibility" || ce.RuleID == "" {
		t.Fatalf("err = %v", err)
	}
	evs, err := f.e.Log(ctx, core.LogArgs{})
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Type != "denied" || last.Participant != f.alice.ParticipantID ||
		payloadStr(last.Payload, "rule_id") != ce.RuleID || payloadStr(last.Payload, "layer") != "visibility" {
		t.Fatalf("last event = %+v", last)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("messages = %d, %v", n, err)
	}

	_, err = f.e.Send(ctx, f.alice, core.SendArgs{To: "bob", Body: "re", ReplyTo: "no-such-id"})
	if !errors.As(err, &ce) || ce.Code != core.CodeDenied || ce.Layer != "visibility" || ce.RuleID != "reply_to.not_in_view" {
		t.Fatalf("reply_to outside view: %v", err)
	}
	if evs, _ := f.e.Log(ctx, core.LogArgs{}); payloadStr(evs[len(evs)-1].Payload, "rule_id") != "reply_to.not_in_view" {
		t.Fatalf("reply_to refusal has no denied event: %+v", evs[len(evs)-1])
	}
}

func payloadStr(raw json.RawMessage, key string) string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	s, _ := m[key].(string)
	return s
}

// #7: pin, replace, remove; a full board refuses with the live pins.
func TestBoardPinReplaceRemoveAndLimit(t *testing.T) {
	f := newFixture(t, nil)
	board := func() []string {
		t.Helper()
		pins, err := f.e.Board(ctx, f.bob)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, p := range pins {
			out = append(out, p.ID)
		}
		return out
	}
	p1 := f.send(t, f.alice, core.SendArgs{To: "board", Body: "v1"}).ID
	p2 := f.send(t, f.alice, core.SendArgs{To: "board", Op: "replace", Target: p1, Body: "v2"}).ID
	if got := board(); !eq(got, []string{p2}) {
		t.Fatalf("after replace board = %v, want [%s]", got, p2)
	}
	f.send(t, f.alice, core.SendArgs{To: "board", Op: "remove", Target: p2})
	if got := board(); len(got) != 0 {
		t.Fatalf("after remove board = %v", got)
	}
	if _, err := f.e.Send(ctx, f.alice, core.SendArgs{To: "board", Op: "replace", Target: p2, Body: "v3"}); code(err) != core.CodeDenied {
		t.Fatalf("replace of removed pin: %v", err)
	}

	for i := 0; i < core.BoardLimit; i++ {
		f.send(t, f.bob, core.SendArgs{To: "board", Body: "pin"})
	}
	_, err := f.e.Send(ctx, f.alice, core.SendArgs{To: "board", Body: "one too many"})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeDenied || ce.Layer != "limit" {
		t.Fatalf("21st pin: %v", err)
	}
	if live, ok := ce.Details.([]core.Message); !ok || len(live) != core.BoardLimit {
		t.Fatalf("denial details = %#v, want the %d live pins", ce.Details, core.BoardLimit)
	}
}

// #6: unacked mail with its open delivery, a pin and a timer survive closing and reopening the DB file.
func TestReopenDBFileKeepsMailPinAndTimer(t *testing.T) {
	path := t.TempDir() + "/piggery.db"
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, db)
	msg := f.send(t, f.alice, core.SendArgs{To: "bob", Body: "hi"}).ID
	before := f.inbox(t, f.bob, batch(1))
	pin := f.send(t, f.alice, core.SendArgs{To: "board", Body: "pinned"}).ID
	if _, err := f.e.WatchAdd(ctx, f.alice, core.TimerArgs{To: "bob", EveryMs: 1000, Body: "storm"}); code(err) != core.CodeInvalid {
		t.Fatalf("repeating timer under a minute: %v", err)
	}
	tm, err := f.e.WatchAdd(ctx, f.alice, core.TimerArgs{To: "bob", InMs: 1000, Body: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db, core.WithClock(func() time.Time { return time.Now().Add(2 * time.Second) }))

	if d, err := e.Inbox(ctx, f.bob, core.InboxArgs{Batch: batch(1)}); err != nil || len(d) != 1 ||
		d[0].ID != msg || d[0].DeliveryID != before[0].DeliveryID {
		t.Fatalf("reopened inbox = %+v, %v", d, err)
	}
	if pins, err := e.Board(ctx, f.bob); err != nil || len(pins) != 1 || pins[0].ID != pin {
		t.Fatalf("reopened board = %+v, %v", pins, err)
	}
	if ts, err := e.WatchList(ctx, f.alice); err != nil || len(ts) != 1 || ts[0].ID != tm.ID {
		t.Fatalf("reopened timers = %+v, %v", ts, err)
	}
	if n, err := e.FireDue(ctx); err != nil || n != 1 {
		t.Fatalf("FireDue = %d, %v", n, err)
	}
	r, err := e.Completion(ctx, f.bob, core.CompletionArgs{Batch: 1})
	if err != nil || !eq(r.Acked, []string{msg}) {
		t.Fatalf("completion after reopen = %+v, %v", r, err)
	}
	d, err := e.Inbox(ctx, f.bob, core.InboxArgs{})
	if err != nil || len(d) != 1 || d[0].From != core.AddrEngine || d[0].Body != "ping" {
		t.Fatalf("timer message = %+v, %v", d, err)
	}
}

// A timer fires as engine, so WatchAdd must pass the same routing gate as Send.
func TestWatchAddEnforcesRouting(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	const man = `
template: lw
roles: {lead: {tools: [send, inbox, who, agent]}, worker: {tools: [send, inbox, who, agent]}}
routing:
  - {from: lead, to: worker, allow: true}
  - {from: worker, to: lead, allow: false}
`
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var workerID, workerTok string
	for _, j := range []core.JoinArgs{{Role: "lead", Name: "lee"}, {Role: "worker", Name: "wes"}} {
		j.Team, j.Cwd = team.ID, team.RootCwd
		r, err := e.Join(ctx, j)
		if err != nil {
			t.Fatal(err)
		}
		workerID, workerTok = r.ID, r.Token
	}
	worker, err := e.Authenticate(ctx, workerID, workerTok)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.WatchAdd(ctx, worker, core.TimerArgs{To: "lee", Body: "ping"})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeDenied || ce.Layer != "routing" {
		t.Fatalf("worker timer to lead: %v", err)
	}
	evs, _ := e.Log(ctx, core.LogArgs{})
	if last := evs[len(evs)-1]; last.Type != "denied" || payloadStr(last.Payload, "layer") != "routing" {
		t.Fatalf("last event = %+v", last)
	}
}

// A new process gets a new run: the old run's open batch can never ack, mail stays pending.
func TestIdentifyNewRunOrphansOldRun(t *testing.T) {
	f := newFixture(t, nil)
	msg := f.send(t, f.alice, core.SendArgs{To: "bob", Body: "hi"}).ID
	f.inbox(t, f.bob, batch(1))

	same, err := f.e.Identify(ctx, f.bob, core.IdentifyArgs{RunID: f.bob.RunID, Harness: "pi", HarnessRef: "session-1"})
	if err != nil || same.RunID != f.bob.RunID {
		t.Fatalf("reconnect identify = %+v, %v; want run kept", same, err)
	}
	fresh, err := f.e.Identify(ctx, f.bob, core.IdentifyArgs{NewRun: true, Harness: "pi", HarnessRef: "session-2", ToolPrefix: "piggery_"})
	var ref string
	if err := f.db.QueryRow(`SELECT harness_ref FROM participants WHERE id=?`, f.bob.ParticipantID).Scan(&ref); err != nil || ref != "session-1" {
		t.Fatalf("harness_ref = %q, %v; want the first session kept", ref, err)
	}
	if err != nil || fresh.RunID == f.bob.RunID {
		t.Fatalf("new_run identify = %+v, %v; want a new run", fresh, err)
	}
	if !strings.Contains(fresh.RoleCard, "alice (peer)") || !strings.Contains(fresh.RoleCard, "piggery_send") || !eq(fresh.Tools, []string{"send", "inbox", "who", "agent"}) {
		t.Fatalf("role card/tools = %q %v", fresh.RoleCard, fresh.Tools)
	}
	if _, err := f.e.Completion(ctx, f.bob, core.CompletionArgs{Batch: 1}); code(err) != core.CodeUnauthorized {
		t.Fatalf("old run completion: %v", err)
	}
	bob2 := f.bob
	bob2.RunID = fresh.RunID
	// A superseded process reconnecting with its old run cannot take over the new run.
	var ce *core.Error
	if _, err := f.e.Identify(ctx, bob2, core.IdentifyArgs{RunID: f.bob.RunID}); !errors.As(err, &ce) || ce.RuleID != "run.stale" {
		t.Fatalf("reconnect with old run: %v", err)
	}
	if r, err := f.e.Identify(ctx, bob2, core.IdentifyArgs{RunID: fresh.RunID}); err != nil || r.RunID != fresh.RunID {
		t.Fatalf("reconnect with current run = %+v, %v", r, err)
	}
	if _, err := f.e.Identify(ctx, bob2, core.IdentifyArgs{}); code(err) != core.CodeInvalid {
		t.Fatalf("reconnect without run_id: %v", err)
	}
	if d := f.inbox(t, bob2, batch(1)); !eq(ids(d), []string{msg}) {
		t.Fatalf("new run batch 1 = %v, want the pending mail", ids(d))
	}
}

// Presence moves the participant's state (no event).
func TestPresenceMovesState(t *testing.T) {
	f := newFixture(t, nil)
	state := func() string {
		t.Helper()
		who, err := f.e.Who(ctx, f.alice)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range who {
			if p.ID == f.bob.ParticipantID {
				return p.State
			}
		}
		return ""
	}
	for _, step := range []struct{ event, state string }{
		{core.PresenceAgentStart, "working"},
		{core.PresenceAgentSettled, "idle"},
		{core.PresenceShutdown, "gone"},
	} {
		if err := f.e.Presence(ctx, f.bob, core.PresenceArgs{Event: step.event}); err != nil {
			t.Fatal(err)
		}
		if got := state(); got != step.state {
			t.Fatalf("after %s: state %q", step.event, got)
		}
	}
}

// Notify fires only after the message is committed, and never for a refused send.
func TestNotifyAfterCommitOnly(t *testing.T) {
	var db *sql.DB
	var notified []string
	f := newFixture(t, nil, core.WithNotify(func(id string) {
		// The DB has one connection: this read would block if the send's tx were still open.
		qctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var n int
		if err := db.QueryRowContext(qctx, `SELECT COUNT(*) FROM messages WHERE to_id=?`, id).Scan(&n); err != nil || n == 0 {
			t.Errorf("notify before commit: n=%d err=%v", n, err)
		}
		notified = append(notified, id)
	}))
	db = f.db
	f.send(t, f.alice, core.SendArgs{To: "bob", Body: "hi"})
	if _, err := f.e.Send(ctx, f.alice, core.SendArgs{To: "nobody", Body: "hi"}); code(err) != core.CodeDenied {
		t.Fatal(err)
	}
	if !eq(notified, []string{f.bob.ParticipantID}) {
		t.Fatalf("notified = %v, want only bob once", notified)
	}
}
