package core_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// A solo calls a taskforce with spawn template=: it gets a team with a headless chair that reports to
// it, ps links the team to its caller, a template with no taskforce: block and a chair calling
// another taskforce are refused, only the caller closes it from outside, the caller gets one notice
// per stretch of idleness, and the daemon tick closes one whose chair is gone (stopping its lens
// and telling the caller once) or whose caller is gone. An unknown key in the
// taskforce block loads with a warning; a bad idle_for is refused.
func TestTaskforce(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const tf = "template: tf\nsummary: a reviewer\ntaskforce: {idle_for: 20m}\nauto_join_role: chair\n" +
		"roles:\n  chair: {tools: [send, inbox, who, agent], can_spawn: [lens]}\n  lens: {tools: [send, inbox]}\nlimits: {depth: 2, concurrency: 2}\nrouting: [{from: chair, to: lens, allow: true}]\n"
	const plain = "template: plain\nauto_join_role: chair\nroles:\n  chair: {tools: [send, inbox, agent]}\n"
	rt := &fakeRuntime{}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	e := core.New(db, core.WithRuntime(rt), core.WithClock(func() time.Time { return now }), core.WithTemplates(func(name, _ string) (string, error) {
		if name == "plain" {
			return plain, nil
		}
		return tf, nil
	}))
	home := t.TempDir()
	j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: home, Harness: "pi", Mode: "rpc", HarnessRef: "solo-1"})
	if err != nil {
		t.Fatal(err)
	}
	solo, _ := e.Authenticate(ctx, j.ID, j.Token)
	spawn := func(c core.Caller, tpl string) (core.AgentResult, error) {
		return e.Agent(ctx, c, core.AgentArgs{Action: core.AgentSpawn, Template: tpl, Task: "review it"})
	}
	if _, err := spawn(solo, "plain"); rule(err) != "permission/taskforce.not_a_taskforce" {
		t.Fatalf("a template with no taskforce block: %v", err)
	}
	r, err := spawn(solo, "tf")
	if err != nil || r.TeamName != "tf" || r.TaskSeq == 0 || len(rt.starts) != 1 {
		t.Fatalf("spawn = %+v, %v, starts %d", r, err, len(rt.starts))
	}
	st, err := e.State(ctx, core.StateArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tm := range st.Teams {
		if tm.Name == "tf" {
			found = tm.ParentID == solo.ParticipantID && tm.Parent != "" && tm.Gate == "chair"
		}
	}
	if !found {
		t.Fatalf("ps does not link the taskforce to its caller: %+v", st.Teams)
	}
	chair, _ := e.Authenticate(ctx, rt.starts[0].ParticipantID, rt.starts[0].Token)
	if _, err := spawn(chair, "tf"); rule(err) != "permission/taskforce.nested" {
		t.Fatalf("a chair calls a taskforce: %v", err)
	}
	if _, err := e.Agent(ctx, chair, core.AgentArgs{Action: core.AgentClose, Team: "tf"}); rule(err) != "permission/taskforce.not_yours" {
		t.Fatalf("a chair closes its taskforce from outside: %v", err)
	}
	var callerName string
	if err := db.QueryRow(`SELECT name FROM participants WHERE id=?`, solo.ParticipantID).Scan(&callerName); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agent(ctx, chair, core.AgentArgs{Action: core.AgentClose}); rule(err) != "permission/taskforce.close_by_caller" || !strings.Contains(err.Error(), "only "+callerName+" closes") {
		t.Fatalf("a chair closes its own taskforce: %v", err)
	}
	card, err := e.Identify(ctx, chair, core.IdentifyArgs{RunID: chair.RunID})
	if err != nil || !strings.Contains(card.RoleCard, "taskforce called by") {
		t.Fatalf("chair card = %q, %v", card.RoleCard, err)
	}
	// idle_for 20m: one notice once every member has been idle that long, none on the next tick, one
	// more after it worked and went idle again.
	notices := func() (n int) {
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE from_id='engine' AND to_id=? AND body LIKE '%has been idle%'`,
			solo.ParticipantID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	setChair := func(state string) {
		if _, err := db.Exec(`UPDATE participants SET state=?, state_since=? WHERE id=?`, state, now.UnixMilli(), chair.ParticipantID); err != nil {
			t.Fatal(err)
		}
	}
	tick := func(after time.Duration) {
		now = now.Add(after)
		if _, err := e.Taskforces(ctx); err != nil {
			t.Fatal(err)
		}
	}
	setChair("idle")
	tick(10 * time.Minute)
	if notices() != 0 {
		t.Fatal("a notice before idle_for")
	}
	tick(11 * time.Minute)
	tick(time.Minute)
	if notices() != 1 {
		t.Fatalf("notices after 22m idle and a second tick = %d, want 1", notices())
	}
	setChair("working")
	tick(time.Minute)
	setChair("idle")
	tick(21 * time.Minute)
	if notices() != 2 {
		t.Fatalf("notices after working and idle again = %d, want 2", notices())
	}
	if _, err := e.Agent(ctx, solo, core.AgentArgs{Action: core.AgentClose, Team: "tf"}); err != nil {
		t.Fatalf("the caller closes it: %v", err)
	}
	// cwd: a directory inside the caller's root is where the taskforce works; one outside is refused.
	sub := filepath.Join(home, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Agent(ctx, solo, core.AgentArgs{Action: core.AgentSpawn, Template: "tf", Task: "t", Cwd: "sub"}); err != nil {
		t.Fatal(err)
	}
	var root string
	want, _ := filepath.EvalSymlinks(sub)
	if err := db.QueryRow(`SELECT root_cwd FROM teams WHERE name='tf' AND closed_at IS NULL`).Scan(&root); err != nil || root != want || rt.starts[len(rt.starts)-1].Cwd != want {
		t.Fatalf("taskforce root %q (%v), chair cwd %q, want %q", root, err, rt.starts[len(rt.starts)-1].Cwd, want)
	}
	if _, err := e.Agent(ctx, solo, core.AgentArgs{Action: core.AgentSpawn, Template: "tf", Task: "t", Cwd: t.TempDir()}); rule(err) != "bounds/cwd.bounds" {
		t.Fatalf("a cwd outside the caller's root: %v", err)
	}
	if _, err := e.Agent(ctx, solo, core.AgentArgs{Action: core.AgentClose, Team: "tf"}); err != nil {
		t.Fatal(err)
	}
	// A second one whose chair goes, with a lens under it: the tick closes the team, stops the lens
	// and tells the caller once.
	if _, err := spawn(solo, "tf"); err != nil {
		t.Fatal(err)
	}
	chair2, _ := e.Authenticate(ctx, rt.starts[len(rt.starts)-1].ParticipantID, rt.starts[len(rt.starts)-1].Token)
	if _, err := e.Agent(ctx, chair2, core.AgentArgs{Action: core.AgentSpawn, Role: "lens", Name: "l1", Task: "look"}); err != nil {
		t.Fatal(err)
	}
	lens := rt.starts[len(rt.starts)-1].ParticipantID
	if _, err := db.Exec(`UPDATE participants SET state='gone' WHERE id=?`, chair2.ParticipantID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := e.Taskforces(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var closedNotices int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE from_id='engine' AND to_id=? AND body LIKE 'Taskforce tf closed: its chair chair stopped%'`,
		solo.ParticipantID).Scan(&closedNotices); err != nil || closedNotices != 1 {
		t.Fatalf("chair-gone notices = %d, %v", closedNotices, err)
	}
	var open int
	if err := db.QueryRow(`SELECT COUNT(*) FROM teams WHERE closed_at IS NULL AND parent_id IS NOT NULL`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("open taskforces after the chair went: %d, %v", open, err)
	}
	if !slices.Contains(rt.stopped, lens) {
		t.Fatalf("the lens was not stopped: %v", rt.stopped)
	}
	// A third, whose caller then goes: the tick closes it.
	if _, err := spawn(solo, "tf"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE participants SET state='gone' WHERE id=?`, solo.ParticipantID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.Taskforces(ctx); err != nil || n != 1 {
		t.Fatalf("tick closed %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM teams WHERE closed_at IS NULL AND parent_id IS NOT NULL`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("open taskforces after the tick: %d, %v", open, err)
	}
	if w, err := core.CheckManifest(strings.Replace(tf, "idle_for: 20m", "idle_for: 20m, later: 1", 1)); err != nil || len(w) != 1 || !strings.Contains(w[0], "taskforce.later") {
		t.Fatalf("an unknown taskforce key: warnings %v, err %v", w, err)
	}
	if _, err := core.CheckManifest(strings.Replace(tf, "idle_for: 20m", "idle_for: soon", 1)); err == nil {
		t.Fatal("a bad idle_for loaded")
	}
}
