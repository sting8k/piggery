package core_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

// The harness contract, one test for every adapter:
// replaying a harness's captured turns through the calls its adapter makes, a turn that
// completes acks exactly the mail it was given, an aborted turn acks nothing and wakes nobody,
// and its mail is given and acked by the next completed turn.
//
// Every adapter reports standard events through harness.event, with its own
// mapping: pi's extension (agent_start opens a turn under a key it makes, agent_before_settle
// completed|aborted|error ends it ok|interrupted|failed, agent_settled with the turn still open
// (an abort with no before_settle) ends it interrupted) and Claude's hooks (`piggery hook claude`).

// turnReplay plays one captured turn of a harness against the engine as caller c.
type turnReplay func(t *testing.T, e *core.Engine, c core.Caller)

type harnessContract struct {
	name               string
	completed, aborted turnReplay
	completedAgain     turnReplay
}

func readFixture(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// piTurns replays pi 0.87.1 extension events of one scenario (the capture's `scenario` field).
func piTurns(t *testing.T) func(scenario string) turnReplay {
	recs := readFixture(t, "../../testdata/fixtures/pi-0.87.1-ext-events-glm-5.3-flash.jsonl")
	turns := 0
	return func(scenario string) turnReplay {
		return func(t *testing.T, e *core.Engine, c core.Caller) {
			t.Helper()
			key, seen := "", false
			event := func(a core.HarnessEventArgs) {
				a.PromptID = key
				if _, err := e.HarnessEvent(ctx, c, a); err != nil {
					t.Fatal(err)
				}
			}
			for _, r := range recs {
				if r["scenario"] != scenario {
					continue
				}
				seen = true
				switch r["type"] {
				case "agent_start":
					if key == "" {
						turns++
						key = fmt.Sprintf("pi-turn-%d", turns)
					}
					event(core.HarnessEventArgs{Event: core.HarnessTurnStart})
				case "agent_before_settle":
					outcome := map[string]string{"completed": core.HarnessOutcomeOK, "aborted": core.HarnessOutcomeIntr,
						"error": core.HarnessOutcomeFailed}[r["outcome"].(string)]
					event(core.HarnessEventArgs{Event: core.HarnessTurnEnd, Outcome: outcome})
					key = ""
				case "agent_settled":
					if key != "" {
						event(core.HarnessEventArgs{Event: core.HarnessTurnEnd, Outcome: core.HarnessOutcomeIntr})
					}
					key = ""
				}
			}
			if !seen {
				t.Fatalf("pi fixture has no scenario %q", scenario)
			}
		}
	}
}

// claudeTurns replays the Claude Code 2.1.283 hooks of one captured turn (its prompt_id), as
// `piggery hook claude` maps them.
func claudeTurns(t *testing.T) func(promptID string) turnReplay {
	recs := readFixture(t, "../../testdata/fixtures/claude-2.1.283/hooks-c7m.jsonl")
	return func(promptID string) turnReplay {
		return func(t *testing.T, e *core.Engine, c core.Caller) {
			t.Helper()
			seen := false
			for _, r := range recs {
				in, _ := r["input"].(map[string]any)
				if in["prompt_id"] != promptID {
					continue
				}
				seen = true
				a := core.HarnessEventArgs{PromptID: promptID}
				switch in["hook_event_name"] {
				case "UserPromptSubmit":
					a.Event = core.HarnessTurnStart
				case "Stop":
					a.Event, a.Outcome = core.HarnessTurnEnd, core.HarnessOutcomeOK
				case "StopFailure":
					a.Event, a.Outcome = core.HarnessTurnEnd, core.HarnessOutcomeFailed
				default:
					continue
				}
				if _, err := e.HarnessEvent(ctx, c, a); err != nil {
					t.Fatal(err)
				}
			}
			if !seen {
				t.Fatalf("claude fixture has no turn %q", promptID)
			}
		}
	}
}

func TestHarnessContract(t *testing.T) {
	pi, cc := piTurns(t), claudeTurns(t)
	for _, h := range []harnessContract{
		// pi: "tool" settles completed; "abort" (rpc abort mid tool) settles with no before_settle.
		// Through the adapter events, as Claude.
		{name: "pi-0.87.1", completed: pi("tool"), aborted: pi("abort"), completedAgain: pi("tool")},
		// Claude: A ends with Stop; the sleep turn was cut by a priority-now message and has
		// no Stop (the Esc/abort shape); B ends with Stop.
		{name: "claude-2.1.283", completed: cc("21fb33e6-f890-4f8a-b523-40ca724d5809"),
			aborted: cc("de171aa8-86b8-481c-900e-f8804b1d9512"), completedAgain: cc("2498ee53-770c-49d4-b0ea-fa8df079235c")},
	} {
		t.Run(h.name, func(t *testing.T) {
			var woken []string
			f := newFixture(t, nil, core.WithNotify(func(id string) { woken = append(woken, id) }),
				core.WithAbortPush(func(string) int { return 1 }))
			if _, err := f.e.Identify(ctx, f.bob, core.IdentifyArgs{RunID: f.bob.RunID, Capabilities: []string{core.CapAbort}}); err != nil {
				t.Fatal(err)
			}
			acked := func(id string) bool {
				var at *int64
				f.db.QueryRow(`SELECT acked_at FROM messages WHERE id=?`, id).Scan(&at)
				return at != nil
			}
			m1 := f.send(t, f.alice, core.SendArgs{To: "bob", Body: "one"}).ID
			h.completed(t, f.e, f.bob)
			if !acked(m1) {
				t.Fatal("a completed turn did not ack the mail it was given")
			}
			m2 := f.send(t, f.alice, core.SendArgs{To: "bob", Body: "two"}).ID
			woken = nil
			h.aborted(t, f.e, f.bob)
			if acked(m2) || len(woken) != 0 {
				t.Fatalf("aborted turn: acked %v, woken %v; want nothing acked, no self-wake", acked(m2), woken)
			}
			// The abort (sent while the turn ran) leaves no turn open: new mail wakes.
			if _, err := f.e.Abort(ctx, core.AdminTarget{Target: "bob"}); err != nil || len(woken) != 0 {
				t.Fatalf("abort: %v, woken %v", err, woken)
			}
			f.send(t, f.alice, core.SendArgs{To: "bob", Body: "three"})
			if len(woken) != 1 {
				t.Fatalf("mail after the abort: woken %v; want a wake", woken)
			}
			h.completedAgain(t, f.e, f.bob)
			if !acked(m2) {
				t.Fatal("the next completed turn did not give and ack the aborted turn's mail")
			}
		})
	}
}
