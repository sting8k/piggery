package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/server"
)

// Codex Desktop runs every thread in one app-server with one piggery mcp: two threads of it are two
// participants (one host each), a tool call acts as the thread its _meta.sessionId names, and a call
// without it is refused instead of acting as another thread (issue #4).
func TestSharedAppServerThreads(t *testing.T) {
	dir, err := os.MkdirTemp(shortTmp(), "pg") // short: unix socket paths are limited on macOS
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, server.Config{Dir: dir}) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err := Dial(dir, false); err == nil {
			c.Close()
			break
		} else if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
	}
	// This process plays the app-server: the daemon accepts its connections as the host's.
	hostID := fmt.Sprintf("codex:%d:%d", os.Getpid(), local.ProcessStartTime(os.Getpid()))
	oldHost, oldShared := sessionHost, sharedAppServer
	sessionHost = func(...string) string { return hostID }
	sharedAppServer = func(string) bool { return true }
	t.Cleanup(func() { sessionHost, sharedAppServer = oldHost, oldShared })
	t.Setenv("PIGGERY_ID", "")
	t.Setenv("PIGGERY_TOKEN", "")
	t.Setenv("PIGGERY_DISABLED", "")

	e := &env{dir: dir, stdout: io.Discard}
	// The daemon keeps a cwd with its short (8.3) names resolved, as Windows temp dirs have them.
	cwdA, _ := filepath.EvalSymlinks(t.TempDir())
	cwdB, _ := filepath.EvalSymlinks(t.TempDir())
	for _, th := range []struct{ session, cwd string }{{"thread-a", cwdA}, {"thread-b", cwdB}} {
		e.runHook("codex", "SessionStart", strings.NewReader(fmt.Sprintf(`{"session_id":%q,"source":"startup","cwd":%s}`, th.session, mustJSON(th.cwd))), io.Discard)
	}

	s := &mcpServer{dir: dir, host: hostID, harness: "codex", shared: true, sessions: map[string]*mcpServer{}, out: io.Discard}
	who := func(params string) (text string, isError bool) {
		t.Helper()
		res, rerr := s.handle(rpcMsg{Method: "tools/call", Params: json.RawMessage(params)})
		if rerr != nil {
			t.Fatalf("rpc error %+v", rerr)
		}
		b, _ := json.Marshal(res)
		var r struct {
			Content []struct{ Text string }
			IsError bool
		}
		json.Unmarshal(b, &r)
		return r.Content[0].Text, r.IsError
	}
	youAre := func(text string) string { // the cwd of the line marked (you)
		for _, l := range strings.Split(text, "\n") {
			if strings.Contains(l, "(you)") {
				return l
			}
		}
		return ""
	}
	if text, isErr := who(`{"name":"who","arguments":{},"_meta":{"sessionId":"thread-a"}}`); isErr || !strings.Contains(text, cwdA) || !strings.Contains(text, cwdB) || !strings.Contains(youAre(text), cwdA) {
		t.Fatalf("thread-a's who (error %v): want both threads listed, a as you:\n%s", isErr, text)
	}
	if text, isErr := who(`{"name":"who","arguments":{},"_meta":{"sessionId":"thread-b"}}`); isErr || !strings.Contains(youAre(text), cwdB) {
		t.Fatalf("thread-b's who (error %v): want b as you:\n%s", isErr, text)
	}
	if text, isErr := who(`{"name":"who","arguments":{}}`); !isErr || !strings.Contains(text, "refused") || strings.Contains(text, "(you)") {
		t.Fatalf("a call with no sessionId (error %v): want it refused:\n%s", isErr, text)
	}
}
