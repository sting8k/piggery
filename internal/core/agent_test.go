package core_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// fakeRuntime records starts and plays back canned results.
type fakeRuntime struct {
	starts   []core.Spec
	startErr error
	stopped  []string

	harness, profileModel, profileThinking string // Defaults

	aborted    []string // Abort calls
	wakes      bool     // Wake starts turns itself (Codex); else ErrNoWake
	harnessRef string   // Start names the session itself (Codex's thread id)
	woken      []string // Wake calls
	models     []string // SetModel calls, "id=model"
	modelErr   error    // SetModel result
	modelList  []string // Models result; non-nil: the driver declares CapListModels
	thinkErr   error    // SetThinking result (its calls go to models as "id~level")
	killErr    error    // Kill result (nil: exit -1 SIGKILL)

	delivers    bool                  // Deliver gives mail itself (CapDeliver)
	deliverGate func(d core.Delivery) // called first in Deliver (may block)
	deliverMu   sync.Mutex
	delivered   []core.Delivery // Deliver calls, in the order they returned
	deliverErr  error

	inspect    map[int]core.ProcState // by pid; missing -> inspectErr
	inspectErr error
	killed     []int // KillVerified calls, by pid
}

func (f *fakeRuntime) ToolPrefix() string { return "piggery_" }
func (f *fakeRuntime) Wake(id string) error {
	if !f.wakes {
		return core.ErrNoWake
	}
	f.woken = append(f.woken, id)
	return nil
}
func (f *fakeRuntime) Capabilities() []string {
	caps := []string{core.CapAbort, core.CapSetModel, core.CapWake, core.CapSteer, core.CapSystemPrompt, core.CapUsage}
	if f.delivers {
		caps = append(caps, core.CapDeliver)
	}
	if f.modelList != nil {
		caps = append(caps, core.CapListModels)
	}
	return caps
}

func (f *fakeRuntime) Models(context.Context, string) ([]string, error) { return f.modelList, nil }

func (f *fakeRuntime) deliveredCopy() []core.Delivery {
	f.deliverMu.Lock()
	defer f.deliverMu.Unlock()
	return append([]core.Delivery(nil), f.delivered...)
}

func (f *fakeRuntime) Deliver(_ string, d core.Delivery) error {
	if f.deliverGate != nil {
		f.deliverGate(d)
	}
	f.deliverMu.Lock()
	defer f.deliverMu.Unlock()
	f.delivered = append(f.delivered, d)
	return f.deliverErr
}

func (f *fakeRuntime) Defaults() (string, string, string) {
	return f.harness, f.profileModel, f.profileThinking
}

func (f *fakeRuntime) Abort(id string) error { f.aborted = append(f.aborted, id); return nil }

func (f *fakeRuntime) SetModel(_ context.Context, id, model string) error {
	f.models = append(f.models, id+"="+model)
	return f.modelErr
}

func (f *fakeRuntime) SetThinking(_ context.Context, id, level string) error {
	f.models = append(f.models, id+"~"+level)
	return f.thinkErr
}

func (f *fakeRuntime) Kill(_ context.Context, id string) (core.Exit, error) {
	return core.Exit{Code: -1, Signal: "SIGKILL"}, f.killErr
}

func (f *fakeRuntime) Inspect(_ context.Context, p core.Proc) (core.ProcState, error) {
	if st, ok := f.inspect[p.PID]; ok {
		return st, nil
	}
	return "", f.inspectErr
}

// KillVerified records every call (core must only ask for ours), then refuses non-ours.
func (f *fakeRuntime) KillVerified(ctx context.Context, p core.Proc) (core.Exit, error) {
	f.killed = append(f.killed, p.PID)
	if st, err := f.Inspect(ctx, p); err != nil || st != core.ProcOurs {
		return core.Exit{}, errors.New("not ours")
	}
	return core.Exit{Code: -1, Signal: "SIGTERM", At: 9}, nil
}

func (f *fakeRuntime) StopAll(context.Context) {}

func (f *fakeRuntime) Start(_ context.Context, s core.Spec) (core.Proc, error) {
	f.starts = append(f.starts, s)
	if f.startErr != nil {
		return core.Proc{}, f.startErr
	}
	return core.Proc{PID: 100 + len(f.starts), PGID: 100 + len(f.starts), StartTime: 1, Cmdline: []string{"pi", "--mode", "rpc"},
		HarnessRef: f.harnessRef}, nil
}

func (f *fakeRuntime) Stop(_ context.Context, id string) (core.Exit, error) {
	f.stopped = append(f.stopped, id)
	return core.Exit{Code: -1, Signal: "SIGTERM", At: 5}, nil
}

func (f *fakeRuntime) Tail(string, int) ([]json.RawMessage, error) {
	return []json.RawMessage{json.RawMessage(`{"type":"agent_start"}`)}, nil
}

const leadWorker = `
template: lw
roles:
  lead:   {can_spawn: [worker], tools: [send, inbox, who, agent]}
  worker: {can_spawn: [worker], tools: [send, inbox, who, agent]}
routing:
  - {from: lead, to: worker, allow: true}
  - {from: worker, to: worker, allow: true}
  - {from: worker, to: lead, allow: true}
limits: {depth: 2, concurrency: 2}
`

type agentFixture struct {
	db     *sql.DB
	e      *core.Engine
	rt     *fakeRuntime
	lead   core.Caller
	lead2  core.Caller
	teamID string
}

func newAgentFixture(t *testing.T) agentFixture {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rt := &fakeRuntime{}
	e := core.New(db, core.WithRuntime(rt))
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: leadWorker, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	join := func(name string) core.Caller {
		j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: name, Cwd: team.RootCwd})
		if err != nil {
			t.Fatal(err)
		}
		c, err := e.Authenticate(ctx, j.ID, j.Token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return agentFixture{db: db, e: e, rt: rt, lead: join("lead"), lead2: join("lead2"), teamID: team.ID}
}

// spawn spawns and returns the worker as a caller (with the token the driver was given).
func (f agentFixture) spawn(t *testing.T, by core.Caller, name string) core.Caller {
	t.Helper()
	r, err := f.e.Agent(ctx, by, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: name, Task: "do " + name})
	if err != nil {
		t.Fatal(err)
	}
	s := f.rt.starts[len(f.rt.starts)-1]
	c, err := f.e.Authenticate(ctx, s.ParticipantID, s.Token)
	if err != nil || r.RunID != c.RunID {
		t.Fatalf("worker auth: %v, run %s vs %s", err, r.RunID, c.RunID)
	}
	return c
}

func rule(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Layer + "/" + ce.RuleID
	}
	return ""
}

// Spawn gate: can_spawn (permission), depth and concurrency (limit), each a denied event.
func TestSpawnGate(t *testing.T) {
	f := newAgentFixture(t)
	noLimits := strings.Replace(leadWorker, "limits: {depth: 2, concurrency: 2}", "limits: {depth: 2}", 1)
	if _, err := f.e.TeamUp(ctx, core.TeamUpArgs{Name: "nolimits", Manifest: noLimits, Cwd: t.TempDir()}); code(err) != core.CodeInvalid {
		t.Fatalf("team up with can_spawn but no concurrency limit: %v", err)
	}
	_, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "lead", Name: "x", Task: "t"})
	if rule(err) != "permission/can_spawn" {
		t.Fatalf("spawn of a role not in can_spawn: %v", err)
	}
	w1 := f.spawn(t, f.lead, "w1") // depth 1
	w2 := f.spawn(t, w1, "w2")     // depth 2
	if _, err := f.e.Agent(ctx, w2, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w3", Task: "t"}); rule(err) != "limit/limits.depth" {
		t.Fatalf("depth 3: %v", err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w4", Task: "t"}); rule(err) != "limit/limits.concurrency" {
		t.Fatalf("third live worker: %v", err)
	}
	evs, _ := f.e.Log(ctx, core.LogArgs{})
	denied := 0
	for _, ev := range evs {
		if ev.Type == "denied" {
			denied++
		}
	}
	if denied != 3 || len(f.rt.starts) != 2 {
		t.Fatalf("denied events %d, starts %d; want 3 and 2", denied, len(f.rt.starts))
	}
}

// Only the spawner (or the worker's lead) can stop or tail it; stop records the exit and ends gone.
func TestStopAndTailOwnership(t *testing.T) {
	f := newAgentFixture(t)
	w := f.spawn(t, f.lead, "w1")
	for _, action := range []string{core.AgentStop, core.AgentTail} {
		if _, err := f.e.Agent(ctx, f.lead2, core.AgentArgs{Action: action, Target: "w1"}); rule(err) != "target/agent.not_owner" {
			t.Fatalf("%s by a non-owner: %v", action, err)
		}
	}
	if r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentTail, Target: "w1", Lines: 5}); err != nil || len(r.Records) != 1 {
		t.Fatalf("tail by owner = %+v, %v", r, err)
	}
	r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"})
	if err != nil || r.Exit == nil || r.Exit.Signal != "SIGTERM" || len(f.rt.stopped) != 1 {
		t.Fatalf("stop by owner = %+v, %v", r, err)
	}
	who, _ := f.e.Who(ctx, f.lead)
	for _, p := range who {
		if p.ID == w.ParticipantID && p.State != "gone" {
			t.Fatalf("stopped worker is %s", p.State)
		}
	}
}

// Spawn writes worker + task in one tx; a failed start leaves the worker gone with its task
// pending, and resume starts the same harness session on a new run that still gets the task. Resume
// reuses the model chosen at spawn, even after the chain would choose another. Regression (live
// test): resume with a task dropped the task and answered ok, so the supervisor waited for a
// handback that could not come. The task is given as at spawn.
func TestResumeWithTaskGivesTheTask(t *testing.T) {
	f := newAgentFixture(t)
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"}); err != nil {
		t.Fatal(err)
	}
	r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1", Task: "second"})
	if err != nil || r.TaskSeq == 0 || r.TaskID == "" {
		t.Fatalf("resume with a task = %+v, %v; want its task seq", r, err)
	}
	s := f.rt.starts[len(f.rt.starts)-1]
	w, err := f.e.Authenticate(ctx, s.ParticipantID, s.Token)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.e.Inbox(ctx, w, core.InboxArgs{})
	var task *core.Delivered
	for i := range d {
		if d[i].ID == r.TaskID {
			task = &d[i]
		}
	}
	if err != nil || task == nil || task.Seq != r.TaskSeq || task.Body != "second" {
		t.Fatalf("worker inbox = %+v, %v; want the unacked task #%d", d, err, r.TaskSeq)
	}
}

func TestSpawnStartFailureKeepsTaskForResume(t *testing.T) {
	f := newAgentFixture(t)
	f.rt.startErr, f.rt.profileModel = errors.New("no profile"), "X"
	_, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "build it"})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeStartFailed {
		t.Fatalf("spawn with failing start: %v", err)
	}
	spawned, ok := ce.Details.(core.AgentResult)
	if !ok || spawned.TaskID == "" {
		t.Fatalf("details = %#v", ce.Details)
	}
	who, _ := f.e.Who(ctx, f.lead)
	if w := who[len(who)-1]; w.Name != "w1" || w.State != "gone" {
		t.Fatalf("worker after failed start: %+v", w)
	}

	f.rt.startErr, f.rt.profileModel = nil, "Z" // the profile changed since the spawn
	r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"})
	if err != nil || r.RunID == spawned.RunID {
		t.Fatalf("resume = %+v, %v; want a new run", r, err)
	}
	first, again := f.rt.starts[0], f.rt.starts[1]
	if first.Model != "X" || again.Model != "X" {
		t.Fatalf("models spawn %q, resume %q; want X stored at spawn and reused", first.Model, again.Model)
	}
	if again.HarnessRef != first.HarnessRef || again.RunID != r.RunID || again.Token == first.Token {
		t.Fatalf("resume spec %+v vs first %+v", again, first)
	}
	w, err := f.e.Authenticate(ctx, again.ParticipantID, again.Token)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.e.Inbox(ctx, w, core.InboxArgs{})
	if err != nil || len(d) != 1 || d[0].ID != spawned.TaskID || d[0].FromLabel != "lead (lead, you report to them)" {
		t.Fatalf("worker inbox = %+v, %v", d, err)
	}
	// The worker's extension identifies with the run it was started with (PIGGERY_RUN_ID): ready.
	if _, err := f.e.Identify(ctx, w, core.IdentifyArgs{RunID: r.RunID}); err != nil {
		t.Fatal(err)
	}
	who, _ = f.e.Who(ctx, f.lead)
	if st := who[len(who)-1].State; st != "idle" {
		t.Fatalf("worker after identify is %s, want idle", st)
	}
}

// join.auto: a new session is a solo (no team) even at a team's root; the same session resumes
// its participant (a solo, or a member of an open team) with a new run and token, only when
// gone; a closed team's member comes back as a new solo; a headless worker's session is never
// taken over.
func TestJoinAuto(t *testing.T) {
	dir := t.TempDir()
	man, err := os.ReadFile("../../manifests/p2p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db, core.WithTemplates(func(string, string) (string, error) { return string(man), nil }))
	if _, err := e.TeamUp(ctx, core.TeamUpArgs{Name: "here", Manifest: string(man), Cwd: dir}); err != nil {
		t.Fatal(err)
	}
	args := core.JoinAutoArgs{Cwd: dir, Harness: "pi", Mode: "interactive", HarnessRef: "abcdef-123456",
		Transcript: &core.Transcript{Path: "/s/a.jsonl", Format: "pi"}}
	// transcript is the session's transcript path as ps reports it ("" none).
	transcript := func(id string) string {
		t.Helper()
		st, err := e.State(ctx, core.StateArgs{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range st.Solos {
			if s.ID == id && s.Transcript != nil {
				return s.Transcript.Path
			}
		}
		for _, tm := range st.Teams {
			for _, m := range tm.Members {
				if m.ID == id && m.Transcript != nil {
					return m.Transcript.Path
				}
			}
		}
		return ""
	}
	first, err := e.JoinAuto(ctx, args)
	if err != nil || first.TeamID != "" {
		t.Fatalf("new session at a team root = %+v, %v; want a solo", first, err)
	}
	if _, err := e.JoinAuto(ctx, args); code(err) != core.CodeInvalid {
		t.Fatalf("rejoin while the session is live: %v", err)
	}
	c, _ := e.Authenticate(ctx, first.ID, first.Token)
	if who, _ := e.Who(ctx, c); len(who) == 0 || !who[0].Gate {
		t.Fatalf("who = %+v", who)
	}
	shutdown := func(c core.Caller) {
		t.Helper()
		if err := e.Presence(ctx, c, core.PresenceArgs{Event: core.PresenceShutdown}); err != nil {
			t.Fatal(err)
		}
	}
	shutdown(c)
	args.Transcript = nil // an adapter that reports none keeps the stored transcript
	again, err := e.JoinAuto(ctx, args)
	if err != nil || again.ID != first.ID || again.RunID == first.RunID || again.Token == first.Token || again.TeamID != "" {
		t.Fatalf("solo resume = %+v, first %+v, %v", again, first, err)
	}
	if p := transcript(again.ID); p != "/s/a.jsonl" {
		t.Fatalf("transcript after a resume that reported none = %q", p)
	}
	if _, err := e.Authenticate(ctx, first.ID, first.Token); err == nil {
		t.Fatal("old token still valid after resume")
	}
	c, _ = e.Authenticate(ctx, again.ID, again.Token)
	f, err := e.Agent(ctx, c, core.AgentArgs{Action: core.AgentFound})
	if err != nil {
		t.Fatal(err)
	}
	shutdown(c)
	args.Transcript = &core.Transcript{Path: "/s/b.jsonl", Format: "pi"} // e.g. the harness's new session file
	member, err := e.JoinAuto(ctx, args)
	if err != nil || member.ID != first.ID || member.TeamID != f.TeamID {
		t.Fatalf("member resume = %+v (team %s), %v", member, f.TeamID, err)
	}
	if p := transcript(member.ID); p != "/s/b.jsonl" {
		t.Fatalf("transcript after a resume that reported a new one = %q", p)
	}
	c, _ = e.Authenticate(ctx, member.ID, member.Token)
	shutdown(c)
	if _, err := e.TeamDown(ctx, core.TeamDownArgs{Team: f.TeamID}); err != nil {
		t.Fatal(err)
	}
	solo, err := e.JoinAuto(ctx, args)
	if err != nil || solo.ID == first.ID || solo.TeamID != "" {
		t.Fatalf("after the team closed = %+v, %v; want a new solo", solo, err)
	}
	// A headless worker's session is never taken over, even when the worker is gone.
	if _, err := db.Exec(`UPDATE participants SET mode='headless', person=0, state='gone' WHERE id=?`, solo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.JoinAuto(ctx, args); code(err) != core.CodeNotFound {
		t.Fatalf("join.auto with a headless worker's session: %v", err)
	}

	// A member that founds another team goes on as a new participant: its transcript goes with it.
	s, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: dir, Harness: "pi", Mode: "interactive", HarnessRef: "fedcba-654321",
		Transcript: &core.Transcript{Path: "/s/c.jsonl", Format: "pi"}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = e.Authenticate(ctx, s.ID, s.Token)
	if _, err := e.Agent(ctx, c, core.AgentArgs{Action: core.AgentFound}); err != nil { // the solo becomes a member
		t.Fatal(err)
	}
	f2, err := e.Agent(ctx, c, core.AgentArgs{Action: core.AgentFound}) // the member leaves for a new team
	if err != nil || f2.ParticipantID == s.ID {
		t.Fatalf("found by a member = %+v, %v; want a new participant", f2, err)
	}
	if p := transcript(f2.ParticipantID); p != "/s/c.jsonl" {
		t.Fatalf("transcript after found by a member = %q; want the session's, moved with it", p)
	}
}

// A session's default name is one word derived from its harness_ref: the same ref gives the
// same word. Two refs that start at the same word get two different words, not a -2 suffix.
func TestJoinAutoWordName(t *testing.T) {
	join := func(e *core.Engine, ref string) string {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: t.TempDir(), Harness: "pi", Mode: "rpc", HarnessRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		who, err := e.Who(ctx, c)
		if err != nil || len(who) == 0 {
			t.Fatalf("who = %+v, %v", who, err)
		}
		for _, w := range who {
			if w.ID == j.ID {
				return w.Name
			}
		}
		t.Fatalf("%s not in who %+v", j.ID, who)
		return ""
	}
	engine := func() *core.Engine {
		t.Helper()
		db, err := store.OpenMemory()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return core.New(db)
	}
	const ref = "019a3c7e-5b1d-7f00-8e2a-1c9d4b6a7e31"
	if a, b := join(engine(), ref), join(engine(), ref); a != b || !regexp.MustCompile(`^[a-z]+$`).MatchString(a) {
		t.Fatalf("names %q, %q; want the same single word", a, b)
	}
	other := ""
	for i := 0; other == ""; i++ {
		if r := fmt.Sprintf("ref-%d", i); r != ref && core.NameWord(r) == core.NameWord(ref) {
			other = r
		}
	}
	e := engine()
	a, b := join(e, ref), join(e, other)
	if a != core.NameWord(ref) || b == a || !regexp.MustCompile(`^[a-z]+$`).MatchString(b) {
		t.Fatalf("same start word: names %q, %q; want %q and another single word", a, b, core.NameWord(ref))
	}
}

// The driver's exit hook: a crash of the current run ends gone with one `exited` event; a
// repeated hook, or the hook after Stop recorded the exit, adds nothing.
func TestProcessExitedOnce(t *testing.T) {
	f := newAgentFixture(t)
	crashed := f.spawn(t, f.lead, "w1")
	stopped := f.spawn(t, f.lead, "w2")
	exit := core.Exit{Code: 1, At: 7}
	for i := 0; i < 2; i++ {
		if err := f.e.ProcessExited(ctx, crashed.ParticipantID, crashed.RunID, exit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w2"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.ProcessExited(ctx, stopped.ParticipantID, stopped.RunID, exit); err != nil {
		t.Fatal(err)
	}
	evs, _ := f.e.Log(ctx, core.LogArgs{})
	count := map[string]int{}
	for _, ev := range evs {
		if ev.Type == "exited" {
			count[ev.Participant]++
		}
	}
	// gone via stop: the later hook adds no exited event for w2
	if len(count) != 1 || count[crashed.ParticipantID] != 1 {
		t.Fatalf("exited events = %v, want one for w1", count)
	}
	who, err := f.e.Who(ctx, f.lead)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range who {
		if (p.Name == "w1" || p.Name == "w2") && p.State != "gone" {
			t.Fatalf("%s is %s, want gone", p.Name, p.State)
		}
	}
}

// exitsDuringStart is a runtime whose process dies during startup: the exit hook runs before
// Start returns, i.e. before core wrote the process row.
type exitsDuringStart struct {
	fakeRuntime
	e *core.Engine
}

func (f *exitsDuringStart) Start(ctx context.Context, s core.Spec) (core.Proc, error) {
	p, _ := f.fakeRuntime.Start(ctx, s)
	if err := f.e.ProcessExited(ctx, s.ParticipantID, s.RunID, core.Exit{Code: 1}); err != nil {
		return p, err
	}
	return p, nil
}

// A worker that dies before its process row exists still ends gone (once), with the row exited.
func TestExitBeforeStartReturns(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt := &exitsDuringStart{}
	e := core.New(db, core.WithRuntime(rt))
	rt.e = e
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: leadWorker, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: "lead", Cwd: team.RootCwd})
	if err != nil {
		t.Fatal(err)
	}
	lead, _ := e.Authenticate(ctx, j.ID, j.Token)
	r, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// Right after spawn returns, with no further hook: already gone, row already exited.
	who, _ := e.Who(ctx, lead)
	var exitedAt sql.NullInt64
	if err := db.QueryRow(`SELECT exited_at FROM processes WHERE participant_id=?`, r.ParticipantID).Scan(&exitedAt); err != nil {
		t.Fatal(err)
	}
	if err := e.ProcessExited(ctx, r.ParticipantID, r.RunID, core.Exit{Code: 1}); err != nil { // late duplicate
		t.Fatal(err)
	}
	evs, _ := e.Log(ctx, core.LogArgs{})
	exited := 0
	for _, ev := range evs {
		if ev.Type == "exited" {
			exited++
		}
	}
	if who[1].State != "gone" || !exitedAt.Valid || exited != 1 {
		t.Fatalf("state %s, exited_at %v, exited events %d; want gone, set, 1", who[1].State, exitedAt, exited)
	}
}

// With no role or profile model a worker runs the current model of its nearest non-headless
// ancestor (through headless workers, whatever model they report), only on the same harness;
// the profile's model comes first; a "model" in the args is ignored (agents do not choose).
// The thinking level has the same chain, resolved on its own: a profile model does not stop
// the thinking level from being inherited.
func TestWorkerModelChain(t *testing.T) {
	spawn := func(f agentFixture, by core.Caller, name string) core.Spec {
		t.Helper()
		var a core.AgentArgs
		if err := json.Unmarshal([]byte(`{"action":"spawn","role":"worker","name":"`+name+`","task":"t","model":"agent-pick"}`), &a); err != nil {
			t.Fatal(err)
		}
		if _, err := f.e.Agent(ctx, by, a); err != nil {
			t.Fatal(err)
		}
		return f.rt.starts[len(f.rt.starts)-1]
	}
	f := newAgentFixture(t)
	f.rt.harness = "pi"
	if _, err := f.e.Identify(ctx, f.lead, core.IdentifyArgs{RunID: f.lead.RunID, Harness: "pi", Model: "M"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Presence(ctx, f.lead, core.PresenceArgs{Event: core.PresenceModel, Model: "N", Thinking: "high"}); err != nil {
		t.Fatal(err)
	}
	w1 := spawn(f, f.lead, "w1")
	c1, _ := f.e.Authenticate(ctx, w1.ParticipantID, w1.Token)
	if _, err := f.e.Identify(ctx, c1, core.IdentifyArgs{RunID: w1.RunID, Harness: "pi", Model: "worker-own"}); err != nil {
		t.Fatal(err)
	}
	if w2 := spawn(f, c1, "w2"); w1.Model != "N" || w2.Model != "N" || w1.Thinking != "high" || w2.Thinking != "high" {
		t.Fatalf("w1 %q/%q, w2 (by w1) %q/%q; want the lead's current N/high for both", w1.Model, w1.Thinking,
			w2.Model, w2.Thinking)
	}

	f = newAgentFixture(t)
	f.rt.harness, f.rt.profileModel = "pi", "P"
	f.e.Identify(ctx, f.lead, core.IdentifyArgs{RunID: f.lead.RunID, Harness: "pi", Model: "M", Thinking: "medium"})
	if w := spawn(f, f.lead, "w1"); w.Model != "P" || w.Thinking != "medium" {
		t.Fatalf("profile model: got %q/%q, want P and the lead's medium", w.Model, w.Thinking)
	}

	f = newAgentFixture(t)
	f.rt.harness = "other"
	f.e.Identify(ctx, f.lead, core.IdentifyArgs{RunID: f.lead.RunID, Harness: "pi", Model: "M"})
	if w := spawn(f, f.lead, "w1"); w.Model != "" {
		t.Fatalf("other harness: got %q, want none", w.Model)
	}
}

// An admin abort cancels a turn without acking its batch: the mail comes again in the next
// batch. A headless worker is told by the driver; a session through the push, and one that is
// not connected cannot be aborted. An admin kill ends only a headless worker.
func TestAbortLeavesBatchUnacked(t *testing.T) {
	f := newAgentFixture(t)
	var pushed []string
	reached := 1
	core.WithAbortPush(func(id string) int { pushed = append(pushed, id); return reached })(f.e)
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "build it"}); err != nil {
		t.Fatal(err)
	}
	s := f.rt.starts[0]
	w, _ := f.e.Authenticate(ctx, s.ParticipantID, s.Token)
	if _, err := f.e.Identify(ctx, w, core.IdentifyArgs{RunID: s.RunID}); err != nil {
		t.Fatal(err)
	}
	one, two := int64(1), int64(2)
	first, err := f.e.Inbox(ctx, w, core.InboxArgs{Batch: &one})
	if err != nil || len(first) != 1 {
		t.Fatalf("batch 1 = %+v, %v", first, err)
	}
	if r, err := f.e.Abort(ctx, core.AdminTarget{Target: "w1"}); err != nil || r.How != "rpc" || !eq(f.rt.aborted, []string{s.ParticipantID}) {
		t.Fatalf("abort w1 = %+v, %v; driver aborted %v", r, err, f.rt.aborted)
	}
	again, err := f.e.Inbox(ctx, w, core.InboxArgs{Batch: &two})
	if err != nil || len(again) != 1 || again[0].ID != first[0].ID {
		t.Fatalf("batch 2 after abort = %+v, %v; want the task again (not acked)", again, err)
	}

	// A session's adapter declares what its harness supports. One that declares nothing (an older
	// adapter) is refused nothing; a declared list refuses what it leaves out.
	if _, err := f.e.Abort(ctx, core.AdminTarget{Target: "lead"}); err != nil || len(pushed) != 1 {
		t.Fatalf("abort of a session that declared nothing: %v, pushed %v; want it pushed", err, pushed)
	}
	pushed = nil
	if _, err := f.e.Identify(ctx, f.lead, core.IdentifyArgs{RunID: f.lead.RunID, Capabilities: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Abort(ctx, core.AdminTarget{Target: "lead"}); code(err) != core.CodeUnsupported || len(pushed) != 0 {
		t.Fatalf("abort of a session that declared no abort: %v, pushed %v", err, pushed)
	}
	if _, err := f.e.SetModel(ctx, core.ModelArgs{AdminTarget: core.AdminTarget{Target: "lead"}, Model: "m"}); code(err) != core.CodeUnsupported {
		t.Fatalf("model of a session that declared no set_model: %v", err)
	}
	if _, err := f.e.Identify(ctx, f.lead, core.IdentifyArgs{RunID: f.lead.RunID, Capabilities: []string{core.CapAbort}}); err != nil {
		t.Fatal(err)
	}
	if r, err := f.e.Abort(ctx, core.AdminTarget{Target: "lead"}); err != nil || r.How != "push" || !eq(pushed, []string{f.lead.ParticipantID}) {
		t.Fatalf("abort lead = %+v, %v; pushed %v", r, err, pushed)
	}
	reached = 0
	if _, err := f.e.Abort(ctx, core.AdminTarget{Target: "lead"}); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("abort of an unconnected session: %v", err)
	}

	// kill: only a headless worker; it ends gone with its exit recorded.
	if _, err := f.e.Kill(ctx, core.AdminTarget{Target: "lead"}); err == nil || !strings.Contains(err.Error(), "not a headless worker") {
		t.Fatalf("kill of a session: %v", err)
	}
	if r, err := f.e.Kill(ctx, core.AdminTarget{Target: "w1"}); err != nil || r.Exit == nil || r.Exit.Signal != "SIGKILL" {
		t.Fatalf("kill w1 = %+v, %v", r, err)
	}
	if who, _ := f.e.Who(ctx, f.lead); who[len(who)-1].Name != "w1" || who[len(who)-1].State != "gone" {
		t.Fatalf("w1 after kill: %+v", who[len(who)-1])
	}
}

// An admin model change on a running worker goes through the driver and is stored only if the
// harness accepts it; a stopped worker gets it at resume; sessions are refused.
func TestAdminSetModel(t *testing.T) {
	f := newAgentFixture(t)
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	id := f.rt.starts[0].ParticipantID
	w1 := f.rt.starts[0]
	wc, _ := f.e.Authenticate(ctx, w1.ParticipantID, w1.Token)
	if _, err := f.e.Identify(ctx, wc, core.IdentifyArgs{RunID: w1.RunID, Harness: "pi", Model: "HP/start", Thinking: "low"}); err != nil {
		t.Fatal(err)
	}
	// ps shows the session's report over the stored model: a live switch must replace it.
	shown := func() (model, thinking string) {
		st, err := f.e.State(ctx, core.StateArgs{})
		if err != nil {
			t.Fatal(err)
		}
		for _, ts := range st.Teams {
			for _, m := range ts.Members {
				if m.Name == "w1" {
					return m.Model, m.Thinking
				}
			}
		}
		t.Fatal("w1 not in ps")
		return "", ""
	}
	set := func(model string) (core.ModelResult, error) {
		return f.e.SetModel(ctx, core.ModelArgs{AdminTarget: core.AdminTarget{Target: "w1"}, Model: model})
	}
	if r, err := set("HP/a"); err != nil || !r.Live || !eq(f.rt.models, []string{id + "=HP/a"}) {
		t.Fatalf("live set = %+v, %v; driver %v", r, err, f.rt.models)
	}
	if m, th := shown(); m != "HP/a" || th != "low" {
		t.Fatalf("ps after a live set shows %q/%q; want HP/a, thinking kept low", m, th)
	}
	f.rt.modelErr = errors.New("Model not found")
	if _, err := set("HP/bad"); err == nil || !strings.Contains(err.Error(), "Model not found") {
		t.Fatalf("refused set: %v", err)
	}
	f.rt.modelErr = nil
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"}); err != nil {
		t.Fatal(err)
	}
	if r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"}); err != nil || f.rt.starts[1].Model != "HP/a" {
		t.Fatalf("resume after a refused set = %+v, %v, model %q; want HP/a kept", r, err, f.rt.starts[1].Model)
	}
	f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"})
	if r, err := set("HP/b"); err != nil || r.Live {
		t.Fatalf("set on a stopped worker = %+v, %v; want stored for resume", r, err)
	}
	f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"})
	if m := f.rt.starts[2].Model; m != "HP/b" {
		t.Fatalf("resume after set on a stopped worker ran %q; want HP/b", m)
	}
	if _, err := f.e.SetModel(ctx, core.ModelArgs{AdminTarget: core.AdminTarget{Target: "lead"}, Model: "HP/x"}); err == nil {
		t.Fatal("model of a session: want refused")
	}

	// --thinking alone keeps the model; a level the harness does not run stores nothing.
	id = f.rt.starts[2].ParticipantID
	think := func(level string) (core.ModelResult, error) {
		return f.e.SetModel(ctx, core.ModelArgs{AdminTarget: core.AdminTarget{Target: "w1"}, Thinking: level})
	}
	f.rt.models = nil
	if r, err := think("high"); err != nil || !r.Live || !eq(f.rt.models, []string{id + "~high"}) {
		t.Fatalf("live thinking = %+v, %v; driver %v", r, err, f.rt.models)
	}
	if _, th := shown(); th != "high" {
		t.Fatalf("ps after a live thinking set shows %q; want high", th)
	}
	f.rt.thinkErr = errors.New(`thinking level: pi ran "high", not "bogus"`)
	if _, err := think("bogus"); err == nil || !strings.Contains(err.Error(), "not \"bogus\"") {
		t.Fatalf("refused thinking: %v", err)
	}
	f.rt.thinkErr = nil
	f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"})
	f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"})
	if s := f.rt.starts[3]; s.Model != "HP/b" || s.Thinking != "high" {
		t.Fatalf("resume ran %q/%q; want HP/b kept and high", s.Model, s.Thinking)
	}
}

// The model list is the driver's, passed through; it is refused for a session, for a worker whose
// harness declared no list at spawn, and for a worker that is no longer running.
func TestAdminModelsList(t *testing.T) {
	f := newAgentFixture(t)
	models := func(who string) ([]string, error) {
		r, err := f.e.Models(ctx, core.AdminTarget{Target: who})
		return r.Models, err
	}
	spawn := func(name string) {
		t.Helper()
		if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: name, Task: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	spawn("w1") // no list_models declared
	if _, err := models("w1"); code(err) != core.CodeInvalid || !strings.Contains(err.Error(), "cannot list") {
		t.Fatalf("worker without the capability: %v; want invalid", err)
	}
	f.rt.modelList = []string{"p/a", "p/b"}
	spawn("w2")
	if got, err := models("w2"); err != nil || !eq(got, []string{"p/a", "p/b"}) {
		t.Fatalf("models = %v, %v", got, err)
	}
	if _, err := models("lead"); code(err) != core.CodeInvalid {
		t.Fatalf("models of a session: %v; want invalid", err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := models("w2"); code(err) != core.CodeInvalid || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("models of a stopped worker: %v; want not running", err)
	}
}

// agent close: only the team's gate, a session, closes its team, the way admin team down does:
// the headless worker is stopped, unacked mail stays unacked, and the gate's session joins
// again as a solo. Others are denied with the gate's name.
func TestGateCloses(t *testing.T) {
	f := newAgentFixture(t)
	if _, err := f.e.Identify(ctx, f.lead, core.IdentifyArgs{RunID: f.lead.RunID, Harness: "pi", Mode: "rpc", HarnessRef: "sess-lead"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "build it"}); err != nil {
		t.Fatal(err)
	}
	w := f.rt.starts[0]
	wc, _ := f.e.Authenticate(ctx, w.ParticipantID, w.Token)
	for who, c := range map[string]core.Caller{"lead2": f.lead2, "w1": wc} {
		_, err := f.e.Agent(ctx, c, core.AgentArgs{Action: core.AgentClose})
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Code != core.CodeDenied {
			t.Fatalf("close by %s: %v; want denied", who, err)
		}
		if who == "lead2" && !strings.Contains(ce.Message, "gate lead") {
			t.Fatalf("close by lead2: %q; want the gate's name", ce.Message)
		}
	}

	r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentClose})
	if err != nil || r.TeamID != f.teamID || !eq(r.Stopped, []string{"w1"}) || !eq(f.rt.stopped, []string{w.ParticipantID}) {
		t.Fatalf("close by the gate = %+v, %v; driver stopped %v", r, err, f.rt.stopped)
	}
	var unacked int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NULL`, w.ParticipantID).Scan(&unacked); err != nil || unacked != 1 {
		t.Fatalf("w1's task after close: %d unacked, %v; want still unacked", unacked, err)
	}
	var by string
	if err := f.db.QueryRow(`SELECT json_extract(payload,'$.by') FROM events WHERE type='team_down'`).Scan(&by); err != nil || by != f.lead.ParticipantID {
		t.Fatalf("team_down by = %q, %v; want the gate", by, err)
	}
	solo, err := f.e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: t.TempDir(), Harness: "pi", Mode: "rpc", HarnessRef: "sess-lead"})
	if err != nil || solo.TeamID != "" || solo.ID == f.lead.ParticipantID {
		t.Fatalf("the gate's session after close = %+v, %v; want a new solo", solo, err)
	}
}

// A worker runs on the harness its role names, else on its main session's, and stays on it:
// stop goes to the same driver. A role naming a harness no driver runs cannot spawn. Model
// names are the harness's own: the claude worker gets the claude profile's model, not the
// pi session's.
func TestWorkerHarnessChoice(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pi := &fakeRuntime{harness: "pi"}
	cc := &fakeRuntime{harness: "claude", profileModel: "sonnet"}
	e := core.New(db, core.WithRuntime(pi), core.WithRuntime(cc), core.WithDefaultHarness("pi"))
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Cwd: t.TempDir(), Manifest: `
template: h
roles:
  lead:   {can_spawn: [worker, claudy, ghost], tools: [agent]}
  worker: {}
  claudy: {spawn: {harness: claude}}
  ghost:  {spawn: {harness: codex}}
routing:
  - {from: lead, to: worker, allow: true}
  - {from: lead, to: claudy, allow: true}
  - {from: lead, to: ghost, allow: true}
limits: {depth: 2, concurrency: 4}
`})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: "lead", Cwd: team.RootCwd})
	if err != nil {
		t.Fatal(err)
	}
	lead, _ := e.Authenticate(ctx, j.ID, j.Token)
	if _, err := e.Identify(ctx, lead, core.IdentifyArgs{RunID: lead.RunID, Harness: "pi", Model: "N"}); err != nil {
		t.Fatal(err)
	}
	spawn := func(role, name string) error {
		_, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentSpawn, Role: role, Name: name, Task: "t"})
		return err
	}
	if err := spawn("claudy", "c1"); err != nil {
		t.Fatal(err)
	}
	if err := spawn("worker", "w1"); err != nil {
		t.Fatal(err)
	}
	if len(cc.starts) != 1 || cc.starts[0].Harness != "claude" || cc.starts[0].Model != "sonnet" ||
		len(pi.starts) != 1 || pi.starts[0].Harness != "pi" || pi.starts[0].Model != "N" {
		t.Fatalf("claude starts %+v, pi starts %+v", cc.starts, pi.starts)
	}
	if err := spawn("ghost", "g1"); err == nil || !strings.Contains(err.Error(), `harness "codex"`) {
		t.Fatalf("spawn on a harness with no driver: %v", err)
	}
	if _, err := e.Agent(ctx, lead, core.AgentArgs{Action: core.AgentStop, Target: "c1"}); err != nil {
		t.Fatal(err)
	}
	if len(cc.stopped) != 1 || len(pi.stopped) != 0 {
		t.Fatalf("stop went to claude %v, pi %v; want claude's driver", cc.stopped, pi.stopped)
	}
}

// A worker whose adapter cannot wake it (Codex) is woken by its runtime driver: new mail while it
// is idle goes to the driver's Wake, not to the notify hook. Its driver names the session (the
// thread id), kept as harness_ref for resume. A turn the driver saw fail with no end from the
// adapter is closed unacked, and new mail wakes it again.
func TestDriverWakesItsWorker(t *testing.T) {
	f := newAgentFixture(t)
	var notified []string
	core.WithNotify(func(id string) { notified = append(notified, id) })(f.e)
	f.rt.wakes, f.rt.harnessRef = true, "thread-1"
	r, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "build it"})
	if err != nil {
		t.Fatal(err)
	}
	var ref string
	f.db.QueryRow(`SELECT harness_ref FROM participants WHERE id=?`, r.ParticipantID).Scan(&ref)
	if ref != "thread-1" {
		t.Fatalf("harness_ref = %q; want the driver's thread id", ref)
	}
	s := f.rt.starts[0]
	if !eq(f.rt.woken, []string{r.ParticipantID}) || slices.Contains(notified, r.ParticipantID) {
		t.Fatalf("task mail: driver woke %v, notify %v; want the driver", f.rt.woken, notified)
	}

	// A turn opens (the nudge), then fails with no Stop: closed, nothing acked; new mail wakes.
	wc, err := f.e.Authenticate(ctx, s.ParticipantID, s.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.HarnessEvent(ctx, wc, core.HarnessEventArgs{Event: core.HarnessTurnStart, PromptID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RuntimeTurnFailed(ctx, s.ParticipantID, s.RunID, "t1"); err != nil {
		t.Fatal(err)
	}
	f.rt.woken = nil
	if _, err := f.e.Send(ctx, f.lead, core.SendArgs{To: "w1", Body: "more"}); err != nil {
		t.Fatal(err)
	}
	var unacked int
	f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NULL`, s.ParticipantID).Scan(&unacked)
	if unacked != 2 || !eq(f.rt.woken, []string{s.ParticipantID}) {
		t.Fatalf("after a failed turn: unacked %d, driver woke %v; want the task and more pending, a wake", unacked, f.rt.woken)
	}

	// A failure reported late, once the next turn's hook opened its batch, leaves that turn open.
	if _, err := f.e.HarnessEvent(ctx, wc, core.HarnessEventArgs{Event: core.HarnessTurnStart, PromptID: "t2"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RuntimeTurnFailed(ctx, s.ParticipantID, s.RunID, "t1"); err != nil {
		t.Fatal(err)
	}
	var open int
	f.db.QueryRow(`SELECT COUNT(*) FROM batches WHERE run_id=? AND ended_at IS NULL`, s.RunID).Scan(&open)
	if open != 1 {
		t.Fatalf("open batches %d after a late failure of another turn; want t2 still open", open)
	}
}

// The admin resumes a stopped worker (piggery resume): a new run of the same participant in the
// same session, no owner check, and the mail sent while it was gone is what it reads. A running
// worker is refused, nothing done.
func TestAdminResume(t *testing.T) {
	f := newAgentFixture(t)
	spawned, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"}); err != nil {
		t.Fatal(err)
	}
	sent, err := f.e.Send(ctx, f.lead, core.SendArgs{To: "w1", Body: "while you were gone"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.e.Resume(ctx, core.AdminTarget{Target: "w1"})
	if err != nil || r.ParticipantID != spawned.ParticipantID || r.RunID == spawned.RunID {
		t.Fatalf("admin resume = %+v, %v; want a new run of %s", r, err, spawned.ParticipantID)
	}
	first, again := f.rt.starts[0], f.rt.starts[1]
	if !again.Resume || again.HarnessRef != first.HarnessRef {
		t.Fatalf("resume spec %+v; want the same session resumed", again)
	}
	w, err := f.e.Authenticate(ctx, again.ParticipantID, again.Token)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.e.Inbox(ctx, w, core.InboxArgs{})
	if err != nil || !slices.ContainsFunc(d, func(m core.Delivered) bool { return m.ID == sent.ID }) {
		t.Fatalf("inbox after resume = %+v, %v; want the mail sent while it was gone", d, err)
	}
	var by string
	f.db.QueryRow(`SELECT json_extract(payload,'$.by') FROM events WHERE type='resumed' AND ref_id=?`, r.ParticipantID).Scan(&by)
	if by != "admin" {
		t.Fatalf("resumed event by %q; want admin", by)
	}
	if _, err := f.e.Resume(ctx, core.AdminTarget{Target: "w1"}); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("resume of a running worker: %v; want an error saying it runs", err)
	}
	if len(f.rt.starts) != 2 {
		t.Fatalf("starts %d; want nothing started for a running worker", len(f.rt.starts))
	}
}
