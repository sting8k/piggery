package core_test

import (
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
	"github.com/sting8k/piggery/manifests"
)

// A Claude session a person opened is known by its host process, which its hooks and MCP server
// share: both join.auto and whichever comes first makes the one participant; /clear (a new session
// id, same process) keeps it and its run; callers authenticate by host; resuming any of its session
// ids finds it; a process that died without an end is taken over.
func TestClaudeSessionJoinsByHost(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	home := t.TempDir()
	if err := manifests.Unpack(home); err != nil {
		t.Fatal(err)
	}
	e := core.New(db, core.WithTemplates(func(name, _ string) (string, error) { return manifests.Resolve(name, home) }))
	dir := t.TempDir()
	join := func(ref, source, host string) core.JoinResult {
		t.Helper()
		r, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: dir, Harness: "claude", Mode: "interactive",
			HarnessRef: ref, Source: source, Host: host})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	count := func() int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM participants WHERE left_at IS NULL`).Scan(&n)
		return n
	}

	mcp := join("s1", "startup", "claude:100:1")
	hook := join("s1", "startup", "claude:100:1")
	cleared := join("s2", "clear", "claude:100:1")
	if count() != 1 || hook.ID != mcp.ID || cleared.ID != mcp.ID || cleared.RunID != mcp.RunID {
		t.Fatalf("mcp %+v, hook %+v, after /clear %+v, participants %d; want one participant, one run", mcp, hook, cleared, count())
	}
	c, err := e.AuthenticateHost(ctx, "claude:100:1")
	if err != nil || c.ParticipantID != mcp.ID || c.RunID != mcp.RunID {
		t.Fatalf("auth by host = %+v, %v", c, err)
	}
	if _, err := e.Identify(ctx, c, core.IdentifyArgs{Harness: "claude", Capabilities: []string{core.CapWake}}); err != nil {
		t.Fatalf("identify by host with no run_id: %v", err)
	}
	// No system prompt of piggery's: after a role change the next turn carries the new card, once;
	// the card given at identify is not repeated.
	turn := func(id string) string {
		t.Helper()
		r, err := e.HarnessEvent(ctx, c, core.HarnessEventArgs{Event: core.HarnessTurnStart, PromptID: id})
		if err != nil {
			t.Fatal(err)
		}
		return r.Text
	}
	if txt := turn("t1"); strings.Contains(txt, "role changed") {
		t.Fatalf("turn after identify repeats the card: %q", txt)
	}
	if _, err := db.Exec(`UPDATE participants SET name='renamed' WHERE id=?`, mcp.ID); err != nil {
		t.Fatal(err)
	}
	c.Name = "renamed"
	if txt := turn("t2"); !strings.Contains(txt, "role changed") || !strings.Contains(txt, "renamed") {
		t.Fatalf("turn after a role change: %q; want the new card", txt)
	}
	if txt := turn("t3"); strings.Contains(txt, "role changed") {
		t.Fatalf("the new card again: %q", txt)
	}
	// /clear or compact wipes the conversation: session_start gives the card again (without it
	// the model forgot piggery, seen live); a plain startup has it from the MCP server.
	start := func(source string) string {
		t.Helper()
		r, err := e.HarnessEvent(ctx, c, core.HarnessEventArgs{Event: core.HarnessSessionStart, Source: source})
		if err != nil {
			t.Fatal(err)
		}
		return r.Text
	}
	if txt := start("clear"); !strings.Contains(txt, "renamed") {
		t.Fatalf("session_start after /clear: %q; want the role card", txt)
	}
	if txt := start("startup"); txt != "" {
		t.Fatalf("session_start at startup: %q; want nothing", txt)
	}
	// Live test regression: a member that founds a team goes on as a new participant (the solo
	// first moves in as the same one); its host and session ids must follow, or every later call
	// by host is refused.
	c, _ = e.AuthenticateHost(ctx, "claude:100:1")
	if r, err := e.Agent(ctx, c, core.AgentArgs{Action: core.AgentFound, Template: "p2p"}); err != nil || r.ParticipantID != mcp.ID {
		t.Fatalf("found by the solo = %+v, %v", r, err)
	}
	c, _ = e.AuthenticateHost(ctx, "claude:100:1")
	founded, err := e.Agent(ctx, c, core.AgentArgs{Action: core.AgentFound, Template: "p2p"})
	if err != nil || founded.ParticipantID == mcp.ID {
		t.Fatalf("found by a member = %+v, %v; want a new participant", founded, err)
	}
	if c2, err := e.AuthenticateHost(ctx, "claude:100:1"); err != nil || c2.ParticipantID != founded.ParticipantID {
		t.Fatalf("auth by host after found = %+v, %v; want the new participant", c2, err)
	}
	mcp.ID, mcp.RunID = founded.ParticipantID, founded.RunID
	c, _ = e.AuthenticateHost(ctx, "claude:100:1")

	if _, err := e.AuthenticateHost(ctx, "claude:999:1"); code(err) != core.CodeUnauthorized {
		t.Fatalf("auth by an unknown host: %v", err)
	}

	// The session id it runs now is the latest (a wake names it). The end of an older id (Codex
	// ends it ~30 s after /clear started the new one) is not the participant's end.
	if ref := e.SessionRef(ctx, mcp.ID); ref != "s2" {
		t.Fatalf("session ref = %q; want s2", ref)
	}
	if _, err := e.HarnessEvent(ctx, c, core.HarnessEventArgs{Event: core.HarnessSessionEnd, HarnessRef: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AuthenticateHost(ctx, "claude:100:1"); err != nil {
		t.Fatalf("after the end of an older session id: %v; want still live", err)
	}

	// Quit, then `claude --resume s2` (a new process): the same participant, a new run.
	if _, err := e.HarnessEvent(ctx, c, core.HarnessEventArgs{Event: core.HarnessSessionEnd}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AuthenticateHost(ctx, "claude:100:1"); code(err) != core.CodeUnauthorized {
		t.Fatalf("auth by the host of a gone session: %v", err)
	}
	resumed := join("s2", "resume", "claude:200:1")
	if resumed.ID != mcp.ID || resumed.RunID == mcp.RunID || count() != 1 {
		t.Fatalf("resume of the cleared id = %+v; want the participant back on a new run", resumed)
	}

	// That process dies with no SessionEnd; resuming in another process takes the session over.
	taken := join("s1", "resume", "claude:300:1")
	if taken.ID != mcp.ID || taken.RunID == resumed.RunID || count() != 1 {
		t.Fatalf("take over = %+v; want the participant on a new run", taken)
	}
	if _, err := e.AuthenticateHost(ctx, "claude:200:1"); code(err) != core.CodeUnauthorized {
		t.Fatalf("auth by the dead process's host: %v", err)
	}
}
