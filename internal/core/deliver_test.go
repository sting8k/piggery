package core_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// A driver that delivers mail itself (CapDeliver) gets each batch from core, and the ack rule is
// unchanged: a completion of the batch while its run is current acks exactly its mail; a batch cancelled
// by a piggery abort acks nothing and its mail is given again with the next delivery (not by itself: an
// abort does not wake the worker); a completion reported for a run that is no longer current acks
// nothing. Mail in an open batch is never delivered twice.
func TestDeliveringDriverAckRule(t *testing.T) {
	f := newAgentFixture(t)
	f.rt.delivers = true
	w := f.spawn(t, f.lead, "w1") // its task is delivered once it runs
	acked := func(body string) bool {
		t.Helper()
		var n int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE body=? AND acked_at IS NOT NULL`, body).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	send := func(body string) {
		t.Helper()
		if _, err := f.e.Send(ctx, f.lead, core.SendArgs{To: "w1", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	last := func() core.Delivery {
		t.Helper()
		got := f.rt.deliveredCopy()
		if len(got) == 0 {
			t.Fatal("nothing delivered")
		}
		return got[len(got)-1]
	}
	ended := func(run string, batch int64, outcome string) {
		t.Helper()
		if err := f.e.DeliveryEnded(ctx, w.ParticipantID, run, batch, outcome); err != nil {
			t.Fatal(err)
		}
	}

	if d := last(); len(f.rt.delivered) != 1 || d.Batch != 1 || d.RunID != w.RunID || !strings.Contains(d.Text, "do w1") {
		t.Fatalf("task delivery: %+v", f.rt.delivered)
	}
	ended(w.RunID, 1, core.DeliveryCompleted)
	if !acked("do w1") {
		t.Fatal("completion did not ack the task")
	}

	send("m1") // idle: batch 2
	if _, err := f.e.Abort(ctx, core.AdminTarget{Target: "w1"}); err != nil {
		t.Fatal(err)
	}
	ended(w.RunID, 2, core.DeliveryCancelled) // the harness's report of the abort
	time.Sleep(50 * time.Millisecond)         // a redelivery would run in its own goroutine
	if n := len(f.rt.deliveredCopy()); acked("m1") || n != 2 {
		t.Fatalf("cancelled: acked %v, %d deliveries; want unacked and no redelivery", acked("m1"), n)
	}
	send("m2") // batch 3 gives m1 again, with m2
	if d := last(); d.Batch != 3 || !strings.Contains(d.Text, "m1") || !strings.Contains(d.Text, "m2") {
		t.Fatalf("after cancel: %+v; want batch 3 with m1 and m2", d)
	}
	send("m3") // busy: batch 4 joins the turn with only what batch 3 does not hold
	if d := last(); d.Batch != 4 || strings.Contains(d.Text, "m1") || !strings.Contains(d.Text, "m3") {
		t.Fatalf("while batch 3 is open: %+v; want batch 4 with m3 only", d)
	}

	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"}); err != nil {
		t.Fatal(err)
	}
	ended(w.RunID, 3, core.DeliveryCompleted) // the old run's completion arrives late
	if acked("m1") || acked("m2") {
		t.Fatal("a completion of a run no longer current acked its mail")
	}
	if d := last(); d.RunID == w.RunID || d.Batch != 1 || !strings.Contains(d.Text, "m1") || !strings.Contains(d.Text, "m3") {
		t.Fatalf("resumed run: %+v; want its batch 1 with the unacked mail", d)
	}
}

// A batch the harness cancels on its own (no piggery abort) would leave its mail unread with
// nothing to wake the worker: it is delivered again, once until a batch completes, and each
// such cancel is recorded.
func TestSelfCancelledDeliveryComesAgainOnce(t *testing.T) {
	f := newAgentFixture(t)
	f.rt.delivers = true
	w := f.spawn(t, f.lead, "w1")
	for _, b := range []int64{1, 2} { // the task, then its redelivery
		if err := f.e.DeliveryEnded(ctx, w.ParticipantID, w.RunID, b, core.DeliveryCancelled); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return len(f.rt.deliveredCopy()) >= 2 })
	}
	time.Sleep(50 * time.Millisecond) // a second redelivery would run in its own goroutine
	got := f.rt.deliveredCopy()
	if len(got) != 2 || got[1].Batch != 2 || !strings.Contains(got[1].Text, "do w1") {
		t.Fatalf("deliveries %+v; want the task once more, as batch 2, and no third", got)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='delivery_cancelled' AND run_id=?`, w.RunID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("delivery_cancelled events %d, %v; want 2", n, err)
	}
}

// Deliveries reach the driver in batch order even when two run at once: batch n is handed to
// the driver before batch n+1 is opened.
func TestDeliveriesKeepBatchOrder(t *testing.T) {
	f := newAgentFixture(t)
	f.rt.delivers = true
	f.spawn(t, f.lead, "w1") // batch 1: the task
	entered, release := make(chan struct{}), make(chan struct{})
	f.rt.deliverGate = func(d core.Delivery) {
		if d.Batch == 2 {
			close(entered)
			<-release
		}
	}
	var wg sync.WaitGroup
	for i, body := range []string{"m1", "m2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.e.Send(ctx, f.lead, core.SendArgs{To: "w1", Body: body}); err != nil {
				t.Error(err)
			}
		}()
		if i == 0 {
			<-entered // batch 2 is in the driver
		}
	}
	time.Sleep(100 * time.Millisecond) // time for m2's delivery to overtake, were it not serialized
	close(release)
	wg.Wait()
	var order []int64
	for _, d := range f.rt.deliveredCopy() {
		order = append(order, d.Batch)
	}
	if len(order) != 3 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("batches reached the driver as %v; want [1 2 3]", order)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200 && !ok(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
}

// A turn the harness runs that no delivered batch started (a background task's notification)
// shows the worker working and ends idle; it acks nothing and opens no batch. It and the
// delivered batches keep each other's work visible: neither ending flips the worker idle
// while the other still runs, and mail that comes during it is delivered as to a busy worker.
func TestUnbatchedTurnKeepsWorkerWorking(t *testing.T) {
	f := newAgentFixture(t)
	f.rt.delivers = true
	w := f.spawn(t, f.lead, "w1")
	state := func() string {
		t.Helper()
		var s string
		if err := f.db.QueryRow(`SELECT state FROM participants WHERE id=?`, w.ParticipantID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	turn := func(event string) {
		t.Helper()
		if err := f.e.UnbatchedTurn(ctx, w.ParticipantID, w.RunID, event); err != nil {
			t.Fatal(err)
		}
	}
	ended := func(batch int64) {
		t.Helper()
		if err := f.e.DeliveryEnded(ctx, w.ParticipantID, w.RunID, batch, core.DeliveryCompleted); err != nil {
			t.Fatal(err)
		}
	}
	ended(1) // the task
	if s := state(); s != "idle" {
		t.Fatalf("after the task: %s", s)
	}

	turn(core.TurnStarted)
	var batches, acked int
	f.db.QueryRow(`SELECT COUNT(*) FROM batches WHERE run_id=?`, w.RunID).Scan(&batches)
	f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NOT NULL`, w.ParticipantID).Scan(&acked)
	if s := state(); s != "working" || batches != 1 || acked != 1 {
		t.Fatalf("turn started: %s, %d batches, %d acked; want working, no new batch, nothing more acked", s, batches, acked)
	}
	if _, err := f.e.Send(ctx, f.lead, core.SendArgs{To: "w1", Body: "m1"}); err != nil {
		t.Fatal(err)
	}
	if d := f.rt.deliveredCopy(); len(d) != 2 || d[1].Batch != 2 {
		t.Fatalf("mail during the turn: %+v; want it delivered as batch 2", d)
	}
	ended(2)
	if s := state(); s != "working" {
		t.Fatalf("batch 2 done while the turn runs: %s; want working", s)
	}
	turn(core.TurnEnded)
	if s := state(); s != "idle" {
		t.Fatalf("turn ended: %s; want idle", s)
	}

	turn(core.TurnStarted)
	if _, err := f.e.Send(ctx, f.lead, core.SendArgs{To: "w1", Body: "m2"}); err != nil { // batch 3
		t.Fatal(err)
	}
	turn(core.TurnEnded)
	if s := state(); s != "working" {
		t.Fatalf("turn ended with batch 3 open: %s; want working", s)
	}
	ended(3)
	if s := state(); s != "idle" {
		t.Fatalf("all done: %s; want idle", s)
	}
}
