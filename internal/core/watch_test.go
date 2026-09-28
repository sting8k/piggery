package core_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

type watchFixture struct {
	e       *core.Engine
	db      *sql.DB
	lead, w core.Caller
	now     *time.Time
}

// newWatchFixture: a lead and a worker "w" (both joined) under one watch rule.
func newWatchFixture(t *testing.T, rule string, opts ...core.Option) watchFixture {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Unix(1_800_000_000, 0)
	e := core.New(db, append(opts, core.WithClock(func() time.Time { return now }))...)
	man := "model: wr\nroles: {lead: {tools: [send, inbox, who, agent]}, worker: {tools: [send, inbox, who, agent]}}\nrouting:\n" +
		"  - {from: lead, to: worker, allow: true}\n  - {from: worker, to: lead, allow: true}\n" +
		"timers:\n  - " + rule + "\n"
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
	return watchFixture{e: e, db: db, lead: join("lead", "lead"), w: join("w", "worker"), now: &now}
}

func (f watchFixture) presence(t *testing.T, event string) {
	t.Helper()
	if err := f.e.Presence(ctx, f.w, core.PresenceArgs{Event: event}); err != nil {
		t.Fatal(err)
	}
}

func (f watchFixture) advance(d time.Duration) { *f.now = f.now.Add(d) }

// fires runs one watch tick and checks how many incidents fired.
func (f watchFixture) fires(t *testing.T, want int) {
	t.Helper()
	n, err := f.e.Watch(ctx)
	if err != nil || n != want {
		t.Fatalf("Watch fired %d (%v), want %d", n, err, want)
	}
}

// leadNotices returns the engine notices in the lead's inbox.
func (f watchFixture) leadNotices(t *testing.T) []string {
	t.Helper()
	in, err := f.e.Inbox(ctx, f.lead, core.InboxArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range in {
		if m.From == core.AddrEngine {
			out = append(out, m.Body)
		}
	}
	return out
}

func TestWatchSilentFor(t *testing.T) {
	f := newWatchFixture(t, "{on: worker, notify: lead, silent_for: 10m}")
	f.presence(t, core.PresenceAgentStart)
	f.advance(9 * time.Minute)
	f.fires(t, 0)
	f.presence(t, core.PresenceTurnEnd) // a turn end restarts the silence
	f.advance(9 * time.Minute)
	f.fires(t, 0)
	f.advance(2 * time.Minute)
	f.fires(t, 1)
	f.fires(t, 0) // same incident
	if n := f.leadNotices(t); len(n) != 1 || !strings.HasPrefix(n[0], "w (worker) has been working for 11m") {
		t.Fatalf("notices = %q", n)
	}
	f.presence(t, core.PresenceAgentSettled) // a new working stretch is a new incident
	f.presence(t, core.PresenceAgentStart)
	f.advance(11 * time.Minute)
	f.fires(t, 1)
}

// A notice to notify has no inbox to wait in: it must reach the notify hook.
func TestWatchNoticeToNotifyRunsHook(t *testing.T) {
	var sunk []string
	f := newWatchFixture(t, "{on: worker, notify: notify, silent_for: 10m}",
		core.WithNotifySink(func(id string) { sunk = append(sunk, id) }))
	f.presence(t, core.PresenceAgentStart)
	f.advance(11 * time.Minute)
	f.fires(t, 1)
	if len(sunk) != 1 {
		t.Fatalf("sink got %v, want one notice", sunk)
	}
}

func TestWatchRulesValidated(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	for _, rule := range []string{
		"{on: worker, notify: lead, silent_for: 1m, colour: red}", // unknown key
		"{on: worker, notify: lead}",                              // no condition
		"{on: ghost, notify: lead, silent_for: 1m}",               // unknown role
		"{on: worker, notify: nobody, silent_for: 1m}",            // unknown target
	} {
		man := "model: wr\nroles: {lead: {tools: [send, inbox, who, agent]}, worker: {tools: [send, inbox, who, agent]}}\ntimers:\n  - " + rule + "\n"
		if _, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()}); code(err) != core.CodeInvalid {
			t.Fatalf("rule %s: %v", rule, err)
		}
	}
}
