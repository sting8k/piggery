package core_test

import (
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

const planDev = `
model: pd
roles:
  planner:  {tools: [send, who, agent], can_spawn: [dev]}
  dev:      {tools: [done, ask], spawn: {model: small, thinking: max}}
  reviewer: {tools: [send]}
routing:
  - {from: planner, to: dev, allow: true}
  - {from: dev, to: planner, allow: true}
  - {from: dev, to: reviewer, allow: true}
tools:
  done: {description: Hand the work back, params: {summary: string}, send: {kind: handback, to: spawned_by}}
  ask:  {description: Ask the reviewer, params: {question: string}, send: {kind: ask, to: reviewer}}
limits: {depth: 2, concurrency: 5}
`

// Declarative tools send through Send with their kind and target; only granted roles may call
// them, with exactly their string params. Spawn uses the role's model unless one is given.
// Model verbs need the tool in the role (send, who, inbox views); driver verbs (inbox pull)
// do not, and why shows the same permission check.
func TestDeclarativeToolsAndSpawnModel(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt := &fakeRuntime{profileModel: "profile", profileThinking: "low"}
	e := core.New(db, core.WithRuntime(rt))
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: planDev, Cwd: t.TempDir()})
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
	planner, reviewer := join("planner", "planner"), join("reviewer", "reviewer")
	if _, err := e.Agent(ctx, planner, core.AgentArgs{Action: core.AgentSpawn, Role: "dev", Name: "dev1", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	dev, err := e.Authenticate(ctx, rt.starts[0].ParticipantID, rt.starts[0].Token)
	if err != nil {
		t.Fatal(err)
	}
	if s := rt.starts[0]; s.Model != "small" || s.Thinking != "max" {
		t.Fatalf("spawn used %q/%q, want the role's small/max over the profile's", s.Model, s.Thinking)
	}
	if _, err := e.Agent(ctx, planner, core.AgentArgs{Action: core.AgentSpawn, Role: "dev", Name: "dev2", Task: "t"}); err != nil {
		t.Fatal(err)
	}

	id, err := e.Identify(ctx, dev, core.IdentifyArgs{RunID: dev.RunID})
	if err != nil || len(id.ToolSpecs) != 2 || id.ToolSpecs[0].Name != "ask" || id.ToolSpecs[1].Params["summary"] != "string" {
		t.Fatalf("tool specs = %+v, %v", id.ToolSpecs, err)
	}
	// The role card names only tools the role has: a dev without send must not be told to use it.
	// No tool_prefix declared: the short names.
	if strings.Contains(id.RoleCard, "send tool") || !strings.Contains(id.RoleCard, "through your tools: done, ask.") {
		t.Fatalf("dev role card = %q", id.RoleCard)
	}

	tool := func(c core.Caller, name string, args map[string]any) error {
		_, err := e.Tool(ctx, c, core.ToolArgs{Name: name, Args: args})
		return err
	}
	if err := tool(dev, "done", map[string]any{"summary": "shipped"}); err != nil {
		t.Fatal(err)
	}
	if err := tool(dev, "ask", map[string]any{"question": "ok?"}); err != nil {
		t.Fatal(err)
	}
	got := func(c core.Caller, kind string) core.Message {
		t.Helper()
		in, err := e.Inbox(ctx, c, core.InboxArgs{})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range in {
			if m.Kind == kind {
				return m.Message
			}
		}
		t.Fatalf("no %s mail in %+v", kind, in)
		return core.Message{}
	}
	if m := got(planner, "handback"); m.From != dev.ParticipantID || m.Body != `{"summary":"shipped"}` {
		t.Fatalf("handback = %+v", m)
	}
	if m := got(reviewer, "ask"); m.Body != `{"question":"ok?"}` {
		t.Fatalf("ask = %+v", m)
	}

	if err := tool(planner, "done", map[string]any{"summary": "x"}); rule(err) != "permission/tools.not_granted" {
		t.Fatalf("tool not granted to the role: %v", err)
	}
	for _, bad := range []map[string]any{{}, {"summary": 3}, {"summary": "x", "extra": "y"}} {
		if err := tool(dev, "done", bad); code(err) != core.CodeInvalid {
			t.Fatalf("bad args %v: %v", bad, err)
		}
	}

	// dev has [done, ask]: no send (nor board pins or watches), who, agent, or inbox views and
	// board reads; the pull inbox is the driver's.
	for name, err := range map[string]error{
		"send":       func() error { _, err := e.Send(ctx, dev, core.SendArgs{To: "planner", Body: "x"}); return err }(),
		"board":      func() error { _, err := e.Send(ctx, dev, core.SendArgs{To: core.AddrBoard, Body: "x"}); return err }(),
		"who":        func() error { _, err := e.Who(ctx, dev); return err }(),
		"view":       func() error { _, err := e.Inbox(ctx, dev, core.InboxArgs{View: core.ViewBoard}); return err }(),
		"board read": func() error { _, err := e.Board(ctx, dev); return err }(),
		"watch add": func() error {
			_, err := e.WatchAdd(ctx, dev, core.TimerArgs{To: "planner", InMs: 60_000, Body: "x"})
			return err
		}(),
		"watch list": func() error { _, err := e.WatchList(ctx, dev); return err }(),
		"agent": func() error {
			_, err := e.Agent(ctx, dev, core.AgentArgs{Action: core.AgentSpawn, Role: "dev", Name: "dev3", Task: "t"})
			return err
		}(),
	} {
		if rule(err) != "permission/tools.not_granted" {
			t.Fatalf("dev %s: %v, want permission/tools.not_granted", name, err)
		}
	}
	if _, err := e.Inbox(ctx, dev, core.InboxArgs{Batch: batch(1)}); err != nil {
		t.Fatalf("dev pull inbox: %v", err)
	}
	// why: dev1 cannot send to the planner, but its done tool can (as the tool call above did);
	// ask goes to the reviewer, so it is listed there and not here.
	if w, err := e.Why(ctx, core.WhyArgs{From: "dev1", To: "planner"}); err != nil || w.Verdict != "deny" || w.RuleID != "tools.not_granted" ||
		len(w.Tools) != 1 || w.Tools[0].Tool != "done" || w.Tools[0].Kind != "handback" || w.Tools[0].Verdict != "allow" {
		t.Fatalf("why dev1 planner = %+v, %v", w, err)
	}
	if w, err := e.Why(ctx, core.WhyArgs{From: "dev1", To: "reviewer"}); err != nil || len(w.Tools) != 1 || w.Tools[0].Tool != "ask" || w.Tools[0].Verdict != "allow" {
		t.Fatalf("why dev1 reviewer = %+v, %v", w, err)
	}
}

func TestRolesV1Validation(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	for _, c := range []struct{ from, to string }{
		{"planner:  {tools: [send, who, agent], can_spawn: [dev]}", "planner: {instructions_file: planner.md}"},
		{"reviewer: {tools: [send]}", "reviewer: {tools: [send, review]}"},
		{"to: reviewer}}", "to: nobody}}"},
		{"{from: dev, to: reviewer, allow: true}", "{from: dev, to: reviewer, allow: true, cc: [ghost]}"},
		{"limits: {depth: 2, concurrency: 5}", "limits: {depth: 2, concurrency: 5, budget_tokens: 100}"}, // out of scope
		{"model: pd", "model: pd\nauto_join_role: ghost"},                                                // unknown role
	} {
		man := strings.Replace(planDev, c.from, c.to, 1)
		if man == planDev {
			t.Fatalf("case %q did not apply", c.to)
		}
		if _, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()}); code(err) != core.CodeInvalid {
			t.Fatalf("%s: %v", c.to, err)
		}
	}
}

// Routing cc: one copy per member of the cc roles, never to the sender or the recipient,
// labelled from the copy reader's view, not counted toward the rate, held and released with
// the original.
func TestRoutingCCCopies(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var woken []string
	e := core.New(db, core.WithNotify(func(id string) { woken = append(woken, id) }))
	man := `
model: cc
roles: {a: {tools: [send, inbox, who, agent]}, b: {tools: [send, inbox, who, agent]}, c: {tools: [send, inbox, who, agent]}}
routing:
  - {from: a, to: b, allow: true, cc: [a, b, c]}
limits: {messages_per_participant_per_minute: 2}
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
	a1, b1, b2, c1 := join("a1", "a"), join("b1", "b"), join("b2", "b"), join("c1", "c")
	inbox := func(c core.Caller) []core.Delivered {
		in, err := e.Inbox(ctx, c, core.InboxArgs{})
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	send := func() core.SendResult {
		r, err := e.Send(ctx, a1, core.SendArgs{To: "b1", Body: "plan", Kind: "plan", ExpectsReply: true})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	orig := send()
	if in := inbox(a1); len(in) != 0 {
		t.Fatalf("sender got a copy: %+v", in)
	}
	if in := inbox(b1); len(in) != 1 || in[0].ID != orig.ID || in[0].CcOf != "" {
		t.Fatalf("recipient inbox = %+v", in)
	}
	cp := inbox(c1)
	if len(cp) != 1 || cp[0].CcOf != orig.ID || cp[0].ExpectsReply || cp[0].ThreadID != orig.ThreadID ||
		cp[0].Kind != "plan" || cp[0].FromLabel != "a1 (a)" || cp[0].CcTo != "b1 (b)" {
		t.Fatalf("c1 copy = %+v", cp)
	}
	if in := inbox(b2); len(in) != 1 || in[0].CcTo != "b1 (your peer)" {
		t.Fatalf("b2 copy = %+v", in)
	}

	if r := send(); r.Held { // 2 of 2 this minute: the 2 copies of the first did not count
		t.Fatalf("second send held: copies counted toward the rate")
	}
	held := send()
	if !held.Held {
		t.Fatalf("third send not held: %+v", held)
	}
	if n := len(inbox(c1)); n != 2 {
		t.Fatalf("c1 sees %d copies, want 2 (the held one's copy is held too)", n)
	}
	woken = nil
	if err := e.Release(ctx, core.ReleaseArgs{ID: held.ID}); err != nil {
		t.Fatal(err)
	}
	if n := len(inbox(c1)); n != 3 || len(woken) != 3 { // b1, and the copies to b2 and c1
		t.Fatalf("after release: c1 sees %d copies, woken %v", n, woken)
	}
}

// {tool:<name>} in instructions becomes the reader's real tool name (its driver's tool_prefix,
// none = short); team up refuses a placeholder that names no built-in or declared tool.
func TestToolPlaceholders(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	man := strings.Replace(planDev, "planner:  {tools:", "planner:  {instructions: 'Use {tool:send}, then wait for {tool:done}.', tools:", 1)
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for prefix, want := range map[string]string{"piggery_": "Use piggery_send, then wait for piggery_done.", "": "Use send, then wait for done."} {
		j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "planner", Name: "p" + prefix, Cwd: team.RootCwd})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		id, err := e.Identify(ctx, c, core.IdentifyArgs{RunID: c.RunID, ToolPrefix: prefix})
		if err != nil || !strings.Contains(id.RoleCard, want) {
			t.Fatalf("prefix %q: card %q, %v; want %q", prefix, id.RoleCard, err, want)
		}
	}
	bad := strings.Replace(man, "{tool:done}", "{tool:finish}", 1)
	if _, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: bad, Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "{tool:finish}") {
		t.Fatalf("team up with {tool:finish}: %v; want refused", err)
	}
}
