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

// The interactive Claude path end to end over captured hooks of
// Claude Code 2.1.283 (testdata/fixtures/claude-2.1.283/hooks-session.jsonl), mapped by the real
// claudeHookEvent and applied to core as `piggery hook claude` does: SessionStart joins by host,
// every other hook authenticates by host.
//   - identity: startup, /clear (same process, new id), quit, resume in a new process: one
//     participant throughout, back on a new run at resume.
//   - turns: mail given at the prompt; mail during a permission prompt does not wake; Esc leaves
//     no hook, the next prompt closes that turn unacked and gives the mail again; its Stop acks
//     all; idle_prompt after it changes nothing.
func TestClaudeSessionHookContract(t *testing.T) {
	ctx := context.Background()
	f, err := os.Open("../../testdata/fixtures/claude-2.1.283/hooks-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type rec struct {
		Part    string   `json:"part"`
		Event   string   `json:"event"`
		Parents []string `json:"parents"`
		Input   json.RawMessage
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
	other, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: dir, Harness: "pi", Mode: "rpc", HarnessRef: "sender"})
	if err != nil {
		t.Fatal(err)
	}
	sender, _ := e.Authenticate(ctx, other.ID, other.Token)
	live := func() []string {
		var ids []string
		rows, _ := db.Query(`SELECT id FROM participants WHERE harness='claude' AND left_at IS NULL`)
		defer rows.Close()
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		return ids
	}
	pending := func(id string) int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_id=? AND acked_at IS NULL`, id).Scan(&n)
		return n
	}
	// hook applies one captured hook; it returns the text the model would get ("" none).
	hook := func(r rec) string {
		t.Helper()
		var in claudeHookInput
		json.Unmarshal(r.Input, &in)
		a, ok := claudeHookEvent(r.Event, in)
		if !ok {
			return ""
		}
		host := "claude:" + strings.TrimSuffix(r.Parents[0], ":claude") + ":1"
		if r.Event == "SessionStart" {
			if _, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: dir, Harness: "claude", Mode: "interactive",
				HarnessRef: in.SessionID, Source: in.Source, Host: host}); err != nil {
				t.Fatalf("%s join: %v", r.Event, err)
			}
		}
		c, err := e.AuthenticateHost(ctx, host)
		if err != nil {
			return "" // e.g. a SessionEnd of a process never seen: the hook is silent
		}
		res, err := e.HarnessEvent(ctx, c, a)
		if err != nil {
			t.Fatalf("%s: %v", r.Event, err)
		}
		return res.Text
	}

	var ids []string
	for _, r := range recs {
		if r.Part != "identity" {
			continue
		}
		hook(r)
		if got := live(); len(got) != 1 || (ids != nil && got[0] != ids[0]) {
			t.Fatalf("after %s %s: claude participants %v; want the one session throughout", r.Event, r.Parents, got)
		}
		ids = live()
	}

	var me string
	for i, r := range recs {
		if r.Part != "turns" {
			continue
		}
		switch {
		case r.Event == "SessionStart":
			hook(r)
			for _, id := range live() {
				if id != ids[0] {
					me = id
				}
			}
			mustSend(t, e, sender, me, "one")
			continue
		case r.Event == "PermissionRequest":
			hook(r)
			woken = nil
			mustSend(t, e, sender, me, "two")
			if len(woken) != 0 {
				t.Fatalf("woken during a permission prompt: %v", woken)
			}
			continue
		}
		txt := hook(r)
		var in claudeHookInput
		json.Unmarshal(r.Input, &in)
		first := r.Event == "UserPromptSubmit" && i > 0 && recs[i-1].Event == "SessionStart"
		if first && !strings.Contains(txt, "one") {
			t.Fatalf("first prompt: %q; want the mail", txt)
		}
		if r.Event == "UserPromptSubmit" && !first && (!strings.Contains(txt, "one") || !strings.Contains(txt, "two")) {
			t.Fatalf("prompt after Esc: %q; want the open turn's mail again and the held mail", txt)
		}
		if r.Event == "Stop" && pending(me) != 0 {
			t.Fatalf("after Stop: %d unacked; want 0", pending(me))
		}
	}
	if me == "" || pending(me) != 0 {
		t.Fatalf("turns: participant %q, unacked %d", me, pending(me))
	}
}

func mustSend(t *testing.T, e *core.Engine, c core.Caller, to, body string) {
	t.Helper()
	if _, err := e.Send(context.Background(), c, core.SendArgs{To: to, Body: body}); err != nil {
		t.Fatal(err)
	}
}
