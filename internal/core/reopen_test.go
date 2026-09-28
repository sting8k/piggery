package core_test

import (
	"path/filepath"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// Reopen: a different solo at the root reopens a closed team and replaces its old gate: the worker
// and its unacked mail to the old gate move to it, and it resumes the worker. Refused off the root
// and for an open team. The old gate's own session later gets its old participant back (a new
// token), and the solo row it used never comes back.
func TestReopen(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt := &fakeRuntime{}
	e := core.New(db, core.WithRuntime(rt), core.WithTemplates(func(string, string) (string, error) {
		return "auto_join_role: lead\n" + leadWorker, nil
	}))
	root := t.TempDir()
	team := filepath.Base(root)
	solo := func(ref, cwd string) core.Caller {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: cwd, Harness: "pi", Mode: "rpc", HarnessRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	agent := func(c core.Caller, a core.AgentArgs) core.AgentResult {
		t.Helper()
		r, err := e.Agent(ctx, c, a)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	lead := solo("lead-1", root)
	agent(lead, core.AgentArgs{Action: core.AgentFound})
	agent(lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "build"})
	w, _ := e.Authenticate(ctx, rt.starts[0].ParticipantID, rt.starts[0].Token)
	var leadName string
	if err := db.QueryRow(`SELECT name FROM participants WHERE id=?`, lead.ParticipantID).Scan(&leadName); err != nil {
		t.Fatal(err)
	}
	report, err := e.Send(ctx, w, core.SendArgs{To: leadName, Body: "half done"})
	if err != nil {
		t.Fatal(err)
	}
	agent(lead, core.AgentArgs{Action: core.AgentClose})

	reopen := core.AgentArgs{Action: core.AgentReopen, Team: team}
	if _, err := e.Agent(ctx, solo("far-2", t.TempDir()), reopen); rule(err) != "target/reopen.not_at_root" {
		t.Fatalf("reopen off the root: %v", err)
	}
	other := solo("oth-3", root)
	r := agent(other, reopen)
	if r.ParticipantID != other.ParticipantID || r.Token != "" || r.Text == "" {
		t.Fatalf("reopen by another session = %+v; want its own row, same token, a summary", r)
	}
	var to, reportsTo, spawnedBy string
	if err := db.QueryRow(`SELECT to_id FROM messages WHERE id=?`, report.ID).Scan(&to); err != nil || to != other.ParticipantID {
		t.Fatalf("the worker's report goes to %s, %v; want the new gate", to, err)
	}
	if err := db.QueryRow(`SELECT reports_to, spawned_by FROM participants WHERE id=?`, w.ParticipantID).
		Scan(&reportsTo, &spawnedBy); err != nil || reportsTo != other.ParticipantID || spawnedBy != other.ParticipantID {
		t.Fatalf("w1 reports to %s, spawned by %s, %v; want the new gate", reportsTo, spawnedBy, err)
	}
	var reopened int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='team_up' AND json_extract(payload,'$.reopened')`).
		Scan(&reopened); err != nil || reopened != 1 {
		t.Fatalf("team_up reopened events = %d, %v", reopened, err)
	}
	if _, err := e.Agent(ctx, solo("thr-4", root), reopen); code(err) != core.CodeInvalid {
		t.Fatalf("reopen of an open team: %v", err)
	}
	agent(other, core.AgentArgs{Action: core.AgentResume, Target: "w1"})

	// The gate's own session: after close it is a solo, and reopen gives it its old participant.
	agent(other, core.AgentArgs{Action: core.AgentClose})
	again := solo("oth-3", root)
	r = agent(again, reopen)
	if r.ParticipantID != other.ParticipantID || r.Token == "" {
		t.Fatalf("reopen by the old gate's session = %+v; want its old participant, a new token", r)
	}
	if _, err := e.Authenticate(ctx, r.ParticipantID, r.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: root, Harness: "pi", Mode: "rpc", HarnessRef: "oth-3"}); code(err) != core.CodeInvalid {
		t.Fatalf("join.auto of that session: %v; want its live gate participant (live), not the solo row", err)
	}
}
