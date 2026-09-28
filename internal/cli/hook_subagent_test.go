package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

// Regression (Claude Code 2.1.283, testdata/fixtures/claude-2.1.283/hooks-subagent.jsonl): hooks
// also fire inside a subagent (Agent tool), with the parent's session_id and prompt_id and an
// agent_id. The subagent is not the participant: mail must not go into its context at its tool
// calls; it goes at the parent's next boundary (the Agent call's PostToolUse). A permission prompt
// inside the subagent still waits on the Human: wakes are held.
func TestClaudeSubagentHooksAreNotTheParticipant(t *testing.T) {
	ctx := context.Background()
	f, err := os.Open("../../testdata/fixtures/claude-2.1.283/hooks-subagent.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type rec struct {
		Run, Event string
		Input      json.RawMessage
	}
	var recs []rec
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r rec
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}

	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var woken []string
	e := core.New(db, core.WithNotify(func(id string) { woken = append(woken, id) }))
	dir := t.TempDir()
	join := func(ref string) core.Caller {
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: dir, Harness: "claude", Mode: "interactive", HarnessRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	sender := join("sender")

	for _, run := range []string{"bypass", "default"} {
		var me core.Caller
		for _, r := range recs {
			if r.Run != run {
				continue
			}
			var in claudeHookInput
			json.Unmarshal(r.Input, &in)
			if r.Event == "SessionStart" {
				me = join(in.SessionID)
				continue
			}
			a, ok := claudeHookEvent(r.Event, in)
			if in.AgentID != "" && ok != (r.Event == "PermissionRequest") {
				t.Fatalf("%s %s in a subagent: reported %v", run, r.Event, ok)
			}
			if !ok {
				continue
			}
			res, err := e.HarnessEvent(ctx, me, a)
			if err != nil {
				t.Fatalf("%s %s: %v", run, r.Event, err)
			}
			switch {
			case r.Event == "UserPromptSubmit":
				woken = nil
				mustSend(t, e, sender, me.ParticipantID, run+" mail") // arrives while the turn runs
			case r.Event == "PermissionRequest" && len(woken) != 0:
				t.Fatalf("%s: woken while the subagent waits on a permission prompt: %v", run, woken)
			case r.Event == "PostToolUse" && !strings.Contains(res.Text, run+" mail"):
				t.Fatalf("%s: the parent's tool boundary after the subagent gave %q; want the mail", run, res.Text)
			}
		}
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NULL`, me.ParticipantID).Scan(&n)
		if n != 0 {
			t.Fatalf("%s: %d unacked after Stop", run, n)
		}
	}
}
