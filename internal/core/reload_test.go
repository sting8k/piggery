package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// A reload starts the worker again in its session with what the config gives now, except a field
// set by hand, which is kept; a config that moves the role to another harness is refused.
func TestReloadTakesTheConfigNowAndKeepsWhatWasSetByHand(t *testing.T) {
	f := newAgentFixture(t)
	f.rt.harness = "pi"
	core.WithRuntime(&fakeRuntime{harness: "claude"})(f.e)
	pin := core.SpawnPin{Model: "m1", Thinking: "low"}
	core.WithConfig(func() (core.Config, error) {
		return core.Config{RoleSpawn: func(string, string) core.SpawnPin { return pin }}, nil
	})(f.e)
	f.spawn(t, f.lead, "w1")
	if _, err := f.e.SetModel(ctx, core.ModelArgs{AdminTarget: core.AdminTarget{Target: "w1"}, Thinking: "high"}); err != nil {
		t.Fatal(err)
	}
	pin = core.SpawnPin{Model: "m2", Thinking: "max"}
	r, err := f.e.Reload(ctx, core.ReloadArgs{AdminTarget: core.AdminTarget{Target: "w1"}, Now: true})
	if err != nil || r.Pending {
		t.Fatalf("reload: %+v, %v", r, err)
	}
	st := f.rt.Starts()
	if s := st[len(st)-1]; len(st) != 2 || !s.Resume || s.Model != "m2" || s.Thinking != "high" {
		t.Fatalf("starts %+v; want the same session again on the config's model and the thinking set by hand", st)
	}
	pin.Harness = "claude"
	if _, err := f.e.Reload(ctx, core.ReloadArgs{AdminTarget: core.AdminTarget{Target: "w1"}, Now: true}); err == nil ||
		!strings.Contains(err.Error(), "another harness") || len(f.rt.Starts()) != 2 {
		t.Fatalf("reload into another harness: %v, %d starts; want refused before any stop", err, len(f.rt.Starts()))
	}
}

// The spawner's reload of a worker in a turn waits for the turn's end, then starts it again.
func TestReloadWaitsForTheTurnToEnd(t *testing.T) {
	f := newAgentFixture(t)
	w := f.spawn(t, f.lead, "w1") // requested: its first turn is to come
	r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentReload, Target: "w1"})
	if err != nil || !r.Pending {
		t.Fatalf("reload in a turn: %+v, %v; want pending", r, err)
	}
	time.Sleep(700 * time.Millisecond) // more than one poll: nothing happens while the turn runs
	if len(f.rt.Starts()) != 1 || len(f.rt.stopped) != 0 {
		t.Fatalf("reloaded during the turn: %d starts, stopped %v", len(f.rt.Starts()), f.rt.stopped)
	}
	if _, err := f.e.Identify(ctx, w, core.IdentifyArgs{RunID: w.RunID}); err != nil { // idle
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); len(f.rt.Starts()) < 2; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("not reloaded after the turn ended")
		}
	}
}
