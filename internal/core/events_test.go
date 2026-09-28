package core_test

import (
	"sort"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

// eventWhitelist is decisions and team/worker lifecycle only. No per-message or per-turn event
// (sent, delivered, acked, batch, state, turn_end, usage,...).
var eventWhitelist = map[string]bool{
	"denied": true, "held": true, "released": true, "reconcile": true, "gc": true, "notice": true,
	"watch_fired": true, "team_up": true, "team_down": true, "spawned": true, "spawn_failed": true,
	"stopped": true, "exited": true, "resumed": true, "respawn_limit": true,
	"admitted": true, "left": true, "rerouted": true,
}

// A full ordinary flow writes only whitelisted events.
func TestEventsAreMinimal(t *testing.T) {
	f := newLiveFixture(t) // leadWorker, lead joined, lead -> notify routed
	f.spawn(t)
	s := f.rt.starts[0]
	worker, err := f.e.Authenticate(ctx, s.ParticipantID, s.Token)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = f.e.Identify(ctx, worker, core.IdentifyArgs{RunID: worker.RunID, Harness: "pi", Mode: "headless"})
	must(err)
	for _, ev := range []string{core.PresenceAgentStart, core.PresenceTurnEnd, core.PresenceAgentSettled} {
		must(f.e.Presence(ctx, worker, core.PresenceArgs{Event: ev}))
	}
	_, err = f.e.Send(ctx, f.lead, core.SendArgs{To: "w1", Body: "status?", ExpectsReply: true})
	must(err)
	_, err = f.e.Inbox(ctx, worker, core.InboxArgs{Batch: batch(1)})
	must(err)
	_, err = f.e.Completion(ctx, worker, core.CompletionArgs{Batch: 1})
	must(err)
	_, err = f.e.Inbox(ctx, f.lead, core.InboxArgs{View: core.ViewBoard})
	must(err)
	_, err = f.e.WatchAdd(ctx, f.lead, core.TimerArgs{To: "w1", InMs: 1, Body: "ping"})
	must(err)
	*f.now = f.now.Add(1e9)
	_, err = f.e.FireDue(ctx)
	must(err)
	_, err = f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentTail, Target: "w1"})
	must(err)
	_, err = f.e.Send(ctx, f.lead, core.SendArgs{To: core.AddrNotify, Body: "ok?"})
	must(err)
	_, err = f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"})
	must(err)

	rows, err := f.db.Query(`SELECT DISTINCT type FROM events`)
	must(err)
	defer rows.Close()
	var types, bad []string
	for rows.Next() {
		var typ string
		must(rows.Scan(&typ))
		types = append(types, typ)
		if !eventWhitelist[typ] {
			bad = append(bad, typ)
		}
	}
	sort.Strings(types)
	if len(bad) > 0 {
		t.Fatalf("events outside the allowed event types: %v (all: %v)", bad, types)
	}
}
