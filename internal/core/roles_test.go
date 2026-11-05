package core_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

const planDev = `
template: pd
roles:
  planner:  {tools: [send, who, agent], can_spawn: [dev]}
  dev:      {tools: [], spawn: {model: small, thinking: max}}
  reviewer: {tools: [send]}
routing:
  - {from: planner, to: dev, allow: true}
  - {from: dev, to: planner, allow: true}
  - {from: dev, to: reviewer, allow: true}
limits: {depth: 2, concurrency: 5}
`

// Spawn uses the role's model unless one is given. Model verbs need the tool in the role (send,
// who, inbox views); driver verbs (inbox pull) do not, and why shows the same permission check.
func TestRoleGrantsAndSpawnModel(t *testing.T) {
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
	planner := join("planner", "planner")
	if _, err := e.Agent(ctx, planner, core.AgentArgs{Action: core.AgentSpawn, Role: "dev", Name: "dev1", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	dev, err := e.Authenticate(ctx, rt.Starts()[0].ParticipantID, rt.Starts()[0].Token)
	if err != nil {
		t.Fatal(err)
	}
	if s := rt.Starts()[0]; s.Model != "small" || s.Thinking != "max" {
		t.Fatalf("spawn used %q/%q, want the role's small/max over the profile's", s.Model, s.Thinking)
	}
	if _, err := e.Agent(ctx, planner, core.AgentArgs{Action: core.AgentSpawn, Role: "dev", Name: "dev2", Task: "t"}); err != nil {
		t.Fatal(err)
	}

	id, err := e.Identify(ctx, dev, core.IdentifyArgs{RunID: dev.RunID})
	// The role card names only tools the role has: a dev without send must not be told to use it.
	if err != nil || strings.Contains(id.RoleCard, "send tool") {
		t.Fatalf("dev role card = %q, %v", id.RoleCard, err)
	}

	// dev has no tools: no send (nor watches), who or agent; the pull inbox is the driver's.
	for name, err := range map[string]error{
		"send": func() error { _, err := e.Send(ctx, dev, core.SendArgs{To: "planner", Body: "x"}); return err }(),
		"who":  func() error { _, err := e.Who(ctx, dev); return err }(),
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
	if w, err := e.Why(ctx, core.WhyArgs{From: "dev1", To: "planner"}); err != nil || w.Verdict != "deny" || w.RuleID != "tools.not_granted" {
		t.Fatalf("why dev1 planner = %+v, %v", w, err)
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
		{"{from: dev, to: reviewer, allow: true}", "{from: dev, to: reviewer, allow: true, cc: [ghost]}"},
		{"limits: {depth: 2, concurrency: 5}", "limits: {depth: 2, concurrency: 5, budget_tokens: 100}"}, // out of scope
		{"template: pd", "template: pd\nauto_join_role: ghost"},                                          // unknown role
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

// Declarative tools were removed: a template that still declares some is refused with a message
// that says so, never brought up without them; the empty `tools: {}` older setups wrote is fine,
// and a team an older daemon brought up with tools keeps working (its manifest is re-read per verb).
func TestDeclaredToolsRefused(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	old := planDev + "tools:\n  done: {description: Hand back, params: {summary: string}, send: {kind: handback, to: spawned_by}}\n"
	if _, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: old, Cwd: t.TempDir()}); code(err) != core.CodeInvalid ||
		!strings.Contains(err.Error(), "declarative tools were removed") {
		t.Fatalf("team up with tools: %v; want refused", err)
	}
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: planDev + "tools: {}\n", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("team up with tools: {}: %v", err)
	}
	if _, err := db.Exec(`UPDATE teams SET manifest=? WHERE id=?`, old, team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "planner", Name: "p", Cwd: team.RootCwd}); err != nil {
		t.Fatalf("join a team stored with tools: %v", err)
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
template: cc
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
		r, err := e.Send(ctx, a1, core.SendArgs{To: "b1", Body: "plan", Kind: "plan"})
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
	if len(cp) != 1 || cp[0].CcOf != orig.ID ||
		cp[0].Kind != "plan" || cp[0].FromLabel != "a1 (a)" || cp[0].CcTo != "b1 (b)" {
		t.Fatalf("c1 copy = %+v", cp)
	}
	if in := inbox(b2); len(in) != 1 || in[0].CcTo != "b1 (b)" {
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
// none = short); team up refuses a placeholder that names no built-in tool.
func TestToolPlaceholders(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	man := strings.Replace(planDev, "planner:  {tools:", "planner:  {instructions: 'Use {tool:send}, then wait for {tool:who}.', tools:", 1)
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for prefix, want := range map[string]string{"piggery_": "Use piggery_send, then wait for piggery_who.", "": "Use send, then wait for who."} {
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
	bad := strings.Replace(man, "{tool:who}", "{tool:finish}", 1)
	if _, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: bad, Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "{tool:finish}") {
		t.Fatalf("team up with {tool:finish}: %v; want refused", err)
	}
}

// The Human's shared prompts go into the card right after the role's instructions, asked by the
// team's template (the manifest's template:) and role, and by ("", "solo") for a solo.
func TestSharedPromptsInTheCard(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var asked []string
	e := core.New(db, core.WithConfig(func() (core.Config, error) {
		return core.Config{SharedPrompts: func(template, role string) string {
			asked = append(asked, template+"/"+role)
			return "\nSHARED " + template + "/" + role + "\n"
		}}, nil
	}))
	man := strings.Replace(leadWorker, "lead:   {", "lead:   {instructions: 'DO THIS', ", 1)
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: man, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: "l", Cwd: team.RootCwd})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := e.Authenticate(ctx, j.ID, j.Token)
	id, err := e.Identify(ctx, c, core.IdentifyArgs{RunID: c.RunID})
	card := id.RoleCard
	i, k, o := strings.Index(card, "DO THIS"), strings.Index(card, "SHARED lw/lead"), strings.Index(card, "Other participants")
	if err != nil || !(0 <= i && i < k && k < o) {
		t.Fatalf("card %q, %v: want the shared text after the instructions and before the participants", card, err)
	}
	sj, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: t.TempDir(), Harness: "pi", Mode: "rpc", HarnessRef: "solo-1"})
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := e.Authenticate(ctx, sj.ID, sj.Token)
	if id, err := e.Identify(ctx, sc, core.IdentifyArgs{RunID: sc.RunID}); err != nil || !strings.Contains(id.RoleCard, "SHARED /solo") {
		t.Fatalf("solo card %q, %v", id.RoleCard, err)
	}
}

// A team whose stored manifest snapshot still has the old `model:` key runs: its members get their
// card naming the template.
func TestLegacyModelKeyInAStoredManifest(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := core.New(db)
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: "template: old\nroles: {peer: {tools: [send]}}\n", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE teams SET manifest='model: old' || char(10) || 'roles: {peer: {tools: [send]}}' WHERE id=?`, team.ID); err != nil {
		t.Fatal(err)
	}
	j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "peer", Name: "p", Cwd: team.RootCwd})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := e.Authenticate(ctx, j.ID, j.Token)
	if id, err := e.Identify(ctx, c, core.IdentifyArgs{RunID: c.RunID}); err != nil || !strings.Contains(id.RoleCard, "(template old)") {
		t.Fatalf("card %q, %v", id.RoleCard, err)
	}
}

// The old form (a role's spawn.model|thinking in the manifest) still works but is deprecated: team
// up says which config.yaml line replaces it, and a field the config sets (roles) wins over it
// while a field it leaves out falls to the manifest's, then the profile's. Never a refusal.
func TestRoleSpawnConfigOverDeprecatedManifest(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt := &fakeRuntime{profileModel: "profile", profileThinking: "low"}
	e := core.New(db, core.WithRuntime(rt), core.WithConfig(func() (core.Config, error) {
		return core.Config{RoleSpawn: func(template, role string) core.SpawnPin {
			if template == "pd" && role == "dev" {
				return core.SpawnPin{Model: "cfg"}
			}
			return core.SpawnPin{}
		}}, nil
	}))
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: planDev, Cwd: t.TempDir()})
	if err != nil || len(team.Warnings) != 1 || !strings.Contains(team.Warnings[0], "model choices for dev belong in config.yaml roles:") {
		t.Fatalf("warnings %q, %v; want one short line naming the role", team.Warnings, err)
	}
	// piggery check gives the entry to write.
	if w, err := core.CheckManifest(planDev); err != nil || len(w) != 1 || !strings.Contains(w[0], "roles.dev.spawn.model/thinking is deprecated") ||
		!strings.Contains(w[0], "roles: {pd/dev: {model: small, thinking: max}}") {
		t.Fatalf("check warnings %q, %v; want the line with the config entry", w, err)
	}
	j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "planner", Name: "planner", Cwd: team.RootCwd})
	if err != nil {
		t.Fatal(err)
	}
	planner, _ := e.Authenticate(ctx, j.ID, j.Token)
	if _, err := e.Agent(ctx, planner, core.AgentArgs{Action: core.AgentSpawn, Role: "dev", Name: "dev1", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	if s := rt.Starts()[0]; s.Model != "cfg" || s.Thinking != "max" {
		t.Fatalf("spawn used %q/%q, want the config's model and the manifest's thinking", s.Model, s.Thinking)
	}
}

// A resumed worker goes on with what it was spawned with, never the config as it is now: here a
// model it left to the harness ("") that a pin names since. A new spawn takes the config as it is
// at that spawn, and a config that cannot be read fails it with the reason.
func TestResumeKeepsWhatSpawnChose(t *testing.T) {
	f := newAgentFixture(t)
	var pin core.SpawnPin
	var cfgErr error
	core.WithConfig(func() (core.Config, error) {
		return core.Config{RoleSpawn: func(string, string) core.SpawnPin { return pin }}, cfgErr
	})(f.e)
	agent := func(action, name string) error {
		_, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: action, Role: "worker", Name: name, Target: name, Task: "t"})
		return err
	}
	if err := agent(core.AgentSpawn, "w1"); err != nil {
		t.Fatal(err)
	}
	pin = core.SpawnPin{Model: "new", Thinking: "high"}
	cfgErr = errors.New("config.yaml: line 3: bad")
	if err := agent(core.AgentSpawn, "w2"); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("spawn with a config that cannot be read: %v; want its reason", err)
	}
	cfgErr = nil
	for _, step := range [][2]string{{core.AgentSpawn, "w2"}, {core.AgentStop, "w1"}, {core.AgentResume, "w1"}} {
		if err := agent(step[0], step[1]); err != nil {
			t.Fatal(step, err)
		}
	}
	st := f.rt.Starts()
	if len(st) != 3 || st[1].Model != "new" || st[1].Thinking != "high" || st[2].Model != "" || st[2].Thinking != "" {
		t.Fatalf("starts %+v; want w2 on the pin, w1 resumed on the harness default it was spawned with", st)
	}
}
