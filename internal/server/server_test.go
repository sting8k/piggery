package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/cli"
	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
	"github.com/sting8k/piggery/internal/store"
	"github.com/sting8k/piggery/manifests"
)

// startServer runs a daemon in a short temp dir (unix socket paths are length-limited on macOS).
func startServer(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(shortTmp(), "pg")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, server.Config{Dir: dir}) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server.Run: %v", err)
		}
		os.RemoveAll(dir)
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		c, err := cli.Dial(dir, false)
		if err == nil {
			c.Close()
			return dir
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up: %v", err)
		}
	}
}

func dial(t *testing.T, dir string) *cli.Client {
	t.Helper()
	c, err := cli.Dial(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestServerAuth(t *testing.T) {
	dir := startServer(t)
	adminTok, err := os.ReadFile(server.AdminTokenPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{server.AdminTokenPath(dir)}
	if sock := server.Leftover(dir); sock != "" { // the socket file (a pipe is no file)
		paths = append(paths, sock)
	}
	for _, p := range paths {
		if fi, err := os.Stat(p); err != nil || !permIs(fi.Mode().Perm(), 0o600) {
			t.Fatalf("%s: want mode 0600, got %v %v", p, fi.Mode(), err)
		}
	}

	// Singleton: a second daemon cannot take the lock, and the first keeps serving.
	if err := server.Run(context.Background(), server.Config{Dir: dir}); err == nil {
		t.Fatal("second server.Run succeeded while the lock was held")
	}

	team, alice, bob := p2pTeam(t, dir)

	// A wrong participant token is unauthorized even when a valid admin token rides along.
	raw, err := server.Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	req := proto.Request{ID: "1", Verb: proto.VerbWho, Auth: &proto.Auth{ID: alice.ID, Token: "wrong"}, AdminToken: strings.TrimSpace(string(adminTok))}
	var resp proto.Response
	if err := json.NewEncoder(raw).Encode(req); err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(raw).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil {
		t.Fatalf("bad participant token with admin token: %+v", resp)
	}
	wantCode(t, resp.Error, core.CodeUnauthorized)
	// A valid participant token never grants admin verbs.
	a := dial(t, dir)
	a.AsParticipant(alice.ID, alice.Token)
	_, err = a.Call(proto.VerbJoin, core.JoinArgs{Team: team.ID, Role: "peer", Name: "mallory", Cwd: dir})
	wantCode(t, err, core.CodeUnauthorized)
	_, err = a.Call(proto.VerbRelease, core.ReleaseArgs{ID: "x"})
	wantCode(t, err, core.CodeUnauthorized)
	admin := dial(t, dir)
	admin.AsAdmin(strings.TrimSpace(string(adminTok)))
	_, err = admin.Call(proto.VerbRelease, core.ReleaseArgs{ID: "no-such-message"})
	wantCode(t, err, core.CodeNotFound) // reached core as an admin verb

	// join.auto needs no credentials (a pi session registers as a solo, even in the team's
	// cwd), and that grants nothing else: participant verbs on the same credential-less
	// connection stay unauthorized.
	anon := dial(t, dir)
	var auto core.JoinResult
	if _, err := anon.CallInto(proto.VerbJoinAuto, core.JoinAutoArgs{Cwd: dir, Harness: "pi", Mode: "rpc", HarnessRef: "sess-1"}, &auto); err != nil || auto.Token == "" || auto.TeamID != "" {
		t.Fatalf("join.auto = %+v, %v", auto, err)
	}
	_, err = anon.Call(proto.VerbWho, nil)
	wantCode(t, err, core.CodeUnauthorized)

	// alice -> bob smoke.
	if _, err := a.Call(proto.VerbSend, core.SendArgs{To: "bob", Body: "hi bob"}); err != nil {
		t.Fatal(err)
	}
	b := dial(t, dir)
	b.AsParticipant(bob.ID, bob.Token)
	var got []core.Delivered
	if _, err := b.CallInto(proto.VerbInbox, core.InboxArgs{}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "hi bob" || !strings.HasPrefix(got[0].FromLabel, "alice") {
		t.Fatalf("bob inbox = %+v", got)
	}
}

// p2pTeam brings up the p2p team with alice and bob through the admin verbs.
func p2pTeam(t *testing.T, dir string) (core.Team, core.JoinResult, core.JoinResult) {
	t.Helper()
	adminTok, err := os.ReadFile(server.AdminTokenPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile("../../manifests/p2p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	admin := dial(t, dir)
	admin.AsAdmin(strings.TrimSpace(string(adminTok)))
	var team core.Team
	if _, err := admin.CallInto(proto.VerbTeamUp, core.TeamUpArgs{Manifest: string(manifest), Cwd: dir}, &team); err != nil {
		t.Fatal(err)
	}
	join := func(name string) core.JoinResult {
		var r core.JoinResult
		if _, err := admin.CallInto(proto.VerbJoin, core.JoinArgs{Team: team.ID, Role: "peer", Name: name, Cwd: dir}, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	return team, join("alice"), join("bob")
}

// An identified connection gets a wake push for new mail; closing it marks the participant gone.
func TestIdentifiedConnectionWakeAndGone(t *testing.T) {
	dir := startServer(t)
	_, alice, bob := p2pTeam(t, dir)

	raw, err := server.Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(raw)
	args, _ := json.Marshal(core.IdentifyArgs{NewRun: true, Harness: "pi", Mode: "rpc"})
	if err := json.NewEncoder(raw).Encode(proto.Request{ID: "1", Verb: proto.VerbIdentify,
		Auth: &proto.Auth{ID: bob.ID, Token: bob.Token}, Args: args}); err != nil {
		t.Fatal(err)
	}
	var resp proto.Response
	if err := dec.Decode(&resp); err != nil || !resp.OK {
		t.Fatalf("identify: %+v %v", resp, err)
	}

	a := dial(t, dir)
	a.AsParticipant(alice.ID, alice.Token)
	if _, err := a.Call(proto.VerbSend, core.SendArgs{To: "bob", Body: "wake up"}); err != nil {
		t.Fatal(err)
	}
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	var push proto.Push
	if err := dec.Decode(&push); err != nil || push.Event != proto.EventWake {
		t.Fatalf("push = %+v, %v", push, err)
	}

	raw.Close()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var who []core.Presence
		if _, err := a.CallInto(proto.VerbWho, nil, &who); err != nil {
			t.Fatal(err)
		}
		if who[1].Name == "bob" && who[1].State == "gone" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bob not gone after close: %+v", who)
		}
	}
}

// rawConn is a driver-like client: one socket, requests with auth, push frames interleaved.
type rawConn struct {
	t      *testing.T
	c      net.Conn
	dec    *json.Decoder
	auth   *proto.Auth
	pushes int
}

func rawDial(t *testing.T, dir string, j core.JoinResult) *rawConn {
	t.Helper()
	c, err := server.Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &rawConn{t: t, c: c, dec: json.NewDecoder(c), auth: &proto.Auth{ID: j.ID, Token: j.Token}}
}

// call returns the response to one request, counting push frames read on the way.
func (r *rawConn) call(verb string, args any) proto.Response {
	r.t.Helper()
	raw, _ := json.Marshal(args)
	if err := json.NewEncoder(r.c).Encode(proto.Request{ID: verb, Verb: verb, Auth: r.auth, Args: raw}); err != nil {
		r.t.Fatal(err)
	}
	r.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var frame struct {
			proto.Response
			Event string `json:"event"`
		}
		if err := r.dec.Decode(&frame); err != nil {
			r.t.Fatal(err)
		}
		if frame.Event != "" {
			r.pushes++
			continue
		}
		return frame.Response
	}
}

// A superseded process (old connection, old run) cannot ack, gets no wakes, and its open
// connection does not keep the participant alive once the current process is gone.
// team down pushes retire to the team's identified connections (the harness leaves at once).
func TestTeamDownPushesRetire(t *testing.T) {
	dir := startServer(t)
	team, _, bob := p2pTeam(t, dir)
	raw, err := server.Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	dec := json.NewDecoder(raw)
	args, _ := json.Marshal(core.IdentifyArgs{NewRun: true, Harness: "pi", Mode: "rpc"})
	if err := json.NewEncoder(raw).Encode(proto.Request{ID: "1", Verb: proto.VerbIdentify,
		Auth: &proto.Auth{ID: bob.ID, Token: bob.Token}, Args: args}); err != nil {
		t.Fatal(err)
	}
	var resp proto.Response
	if err := dec.Decode(&resp); err != nil || !resp.OK {
		t.Fatalf("identify: %+v %v", resp, err)
	}
	adminTok, err := os.ReadFile(server.AdminTokenPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	admin := dial(t, dir)
	admin.AsAdmin(strings.TrimSpace(string(adminTok)))
	if _, err := admin.Call(proto.VerbTeamDown, core.TeamDownArgs{Team: team.ID}); err != nil {
		t.Fatal(err)
	}
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	var push proto.Push
	if err := dec.Decode(&push); err != nil || push.Event != proto.EventRetire || push.Reason != "team.closed" {
		t.Fatalf("push = %+v, %v", push, err)
	}
}

// A gate closing its own team (agent close): its connection gets the answer and no retire (the
// answer is its signal, the harness is mid-call); the team's other sessions get retire as on admin
// team down.
func TestGateCloseRetiresTheOthers(t *testing.T) {
	dir := startServer(t)
	_, alice, bob := p2pTeam(t, dir) // alice joined first: the gate
	open := func(j core.JoinResult) (net.Conn, *json.Decoder) {
		raw, err := server.Dial(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { raw.Close() })
		dec := json.NewDecoder(raw)
		args, _ := json.Marshal(core.IdentifyArgs{NewRun: true, Harness: "pi", Mode: "rpc"})
		if err := json.NewEncoder(raw).Encode(proto.Request{ID: "1", Verb: proto.VerbIdentify,
			Auth: &proto.Auth{ID: j.ID, Token: j.Token}, Args: args}); err != nil {
			t.Fatal(err)
		}
		var resp proto.Response
		if err := dec.Decode(&resp); err != nil || !resp.OK {
			t.Fatalf("identify: %+v %v", resp, err)
		}
		return raw, dec
	}
	ra, da := open(alice)
	rb, db := open(bob)
	args, _ := json.Marshal(core.AgentArgs{Action: core.AgentClose})
	if err := json.NewEncoder(ra).Encode(proto.Request{ID: "2", Verb: proto.VerbAgent,
		Auth: &proto.Auth{ID: alice.ID, Token: alice.Token}, Args: args}); err != nil {
		t.Fatal(err)
	}
	var resp proto.Response
	if err := da.Decode(&resp); err != nil || !resp.OK || resp.ID != "2" {
		t.Fatalf("close: %+v %v", resp, err)
	}
	rb.SetReadDeadline(time.Now().Add(2 * time.Second))
	var push proto.Push
	if err := db.Decode(&push); err != nil || push.Event != proto.EventRetire {
		t.Fatalf("bob's push = %+v, %v; want retire", push, err)
	}
	ra.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var extra map[string]any
	if err := da.Decode(&extra); err == nil {
		t.Fatalf("the gate got %v after its close; want nothing", extra)
	}
}

func TestSupersededConnectionIsStale(t *testing.T) {
	dir := startServer(t)
	_, alice, bob := p2pTeam(t, dir)
	old, cur := rawDial(t, dir, bob), rawDial(t, dir, bob)
	for _, c := range []*rawConn{old, cur} {
		if r := c.call(proto.VerbIdentify, core.IdentifyArgs{NewRun: true}); !r.OK {
			t.Fatalf("identify: %+v", r.Error)
		}
	}

	a := dial(t, dir)
	a.AsParticipant(alice.ID, alice.Token)
	if _, err := a.Call(proto.VerbSend, core.SendArgs{To: "bob", Body: "for the current run"}); err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	if r := cur.call(proto.VerbInbox, core.InboxArgs{Batch: &one}); !r.OK || cur.pushes != 1 {
		t.Fatalf("current inbox: %+v, pushes %d", r, cur.pushes)
	}
	r := old.call(proto.VerbCompletion, core.CompletionArgs{Batch: 1})
	if r.OK || r.Error.Code != core.CodeUnauthorized || r.Error.RuleID != "run.stale" {
		t.Fatalf("old completion = %+v %+v, want run.stale", r, r.Error)
	}
	if old.pushes != 0 {
		t.Fatalf("old connection got %d wakes", old.pushes)
	}
	var still []core.Delivered
	if r := cur.call(proto.VerbInbox, core.InboxArgs{Batch: &one}); !r.OK || json.Unmarshal(r.Result, &still) != nil || len(still) != 1 {
		t.Fatalf("mail acked by the old run: %+v", r)
	}

	cur.c.Close()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var who []core.Presence
		if _, err := a.CallInto(proto.VerbWho, nil, &who); err != nil {
			t.Fatal(err)
		}
		if who[1].Name == "bob" && who[1].State == "gone" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bob not gone while only the superseded connection is open: %+v", who)
		}
	}
}

// The daemon wires the local runtime driver: a spawned worker that exits on its own ends gone
// (driver OnExit -> core.ProcessExited) without anyone calling stop.
func TestSpawnedWorkerExitEndsGone(t *testing.T) {
	dir := startServer(t)
	prof := workerProfile(t, "brief")
	if err := os.MkdirAll(filepath.Join(dir, "harness"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "harness", "pi.json"), prof, 0o600); err != nil {
		t.Fatal(err)
	}
	_, alice, _ := p2pTeam(t, dir)
	a := dial(t, dir)
	a.AsParticipant(alice.ID, alice.Token)
	var r core.AgentResult
	if _, err := a.CallInto(proto.VerbAgent, core.AgentArgs{Action: core.AgentSpawn, Role: "peer", Name: "w1", Task: "t"}, &r); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var ps []core.Presence
		if _, err := a.CallInto(proto.VerbWho, nil, &ps); err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.ID == r.ParticipantID && p.State == "gone" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s not gone after its process exited: %+v", r.ParticipantID, ps)
		}
	}
}

// runServer runs a daemon in dir until the returned stop func is called (and returns).
func runServer(t *testing.T, dir string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, server.Config{Dir: dir}) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err := cli.Dial(dir, false); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
	}
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server.Run: %v", err)
		}
	}
}

// A graceful daemon stop ends live workers and records their exits before the DB closes,
// so the next start has nothing to reconcile for them. The stop is `piggery -a shutdown`: it returns
// once the daemon is gone and no worker is left.
func TestGracefulStopEndsWorkersAndRecordsExits(t *testing.T) {
	dir, err := os.MkdirTemp(shortTmp(), "pg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Join(dir, "harness"), 0o700); err != nil {
		t.Fatal(err)
	}
	prof := workerProfile(t, "stdin") // exits when stdin closes
	if err := os.WriteFile(filepath.Join(dir, "harness", "pi.json"), prof, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := runServer(t, dir)
	_, alice, _ := p2pTeam(t, dir)
	a := dial(t, dir)
	a.AsParticipant(alice.ID, alice.Token)
	var r core.AgentResult
	if _, err := a.CallInto(proto.VerbAgent, core.AgentArgs{Action: core.AgentSpawn, Role: "peer", Name: "w1", Task: "t"}, &r); err != nil {
		t.Fatal(err)
	}
	a.Close()
	var out, errOut bytes.Buffer
	if code := cli.Main(dir, []string{"-a", "shutdown"}, &out, &errOut); code != 0 {
		t.Fatalf("shutdown: exit %d: %s%s", code, out.String(), errOut.String())
	}
	if left := server.Leftover(dir); left != "" {
		t.Fatalf("socket after stop returned: %s", left)
	}
	stop() // Run has returned; this collects its result
	db, err := store.Open(server.DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	err = db.QueryRow(`SELECT pid FROM processes WHERE participant_id=?`, r.ParticipantID).Scan(&pid)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !procGone(pid) {
		t.Fatalf("worker pid %d after stop: still there; want no such process", pid)
	}

	stop = runServer(t, dir)
	defer stop()
	adminTok, _ := os.ReadFile(server.AdminTokenPath(dir))
	admin := dial(t, dir)
	admin.AsAdmin(strings.TrimSpace(string(adminTok)))
	var evs []core.Event
	if _, err := admin.CallInto(proto.VerbLog, core.LogArgs{Limit: 1000}, &evs); err != nil {
		t.Fatal(err)
	}
	var exited, reconciled []string
	for _, ev := range evs {
		if ev.Participant != r.ParticipantID {
			continue
		}
		switch ev.Type {
		case "exited":
			exited = append(exited, string(ev.Payload))
		case "reconcile":
			reconciled = append(reconciled, string(ev.Payload))
		}
	}
	if len(exited) != 1 || strings.Contains(exited[0], "lost") || len(reconciled) != 0 {
		t.Fatalf("worker after graceful stop: exited=%v reconcile=%v; want one recorded exit, nothing to reconcile", exited, reconciled)
	}
}

// A notice the engine writes to notify (here: a team left with no live member) runs every
// executable file of ~/.piggery/hooks/notify.d with the message as one JSON line, in parallel: a hook
// that outlives the timeout is killed with its children and does not hold the other back; a file in
// the old hooks/notify is not run; with no hook nothing happens.
func TestNotifyHooksRunInParallel(t *testing.T) {
	defer server.SetHookTimeout(time.Second)()
	dir := startServer(t)
	// leaveTeam: a solo founds a team and founds another: the first has nobody live left.
	leaveTeam := func(ref string) {
		t.Helper()
		c := dial(t, dir)
		var solo core.JoinResult
		if _, err := c.CallInto(proto.VerbJoinAuto, core.JoinAutoArgs{Cwd: t.TempDir(), Harness: "pi", Mode: "rpc", HarnessRef: ref}, &solo); err != nil {
			t.Fatal(err)
		}
		c.AsParticipant(solo.ID, solo.Token)
		for range 2 {
			if _, err := c.Call(proto.VerbAgent, core.AgentArgs{Action: core.AgentFound}); err != nil {
				t.Fatal(err)
			}
		}
	}
	leaveTeam("sess-1") // before any hook exists: runs nothing
	out, slowDone, legacyOut := filepath.Join(dir, "fast.out"), filepath.Join(dir, "slow.done"), filepath.Join(dir, "legacy.out")
	writeNotifyHooks(t, server.NotifyHooksDir(dir), slowDone, out, legacyOut)
	if err := os.WriteFile(filepath.Join(dir, "hooks", "notify"), []byte("#!/bin/sh\ntouch '"+legacyOut+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	leaveTeam("sess-2")
	var b []byte
	start := time.Now()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, _ = os.ReadFile(out); len(b) > 0 && b[len(b)-1] == '\n' {
			break
		}
	}
	if since := time.Since(start); since > 900*time.Millisecond {
		t.Fatalf("the fast hook ran after %v: it waited for the slow one (timeout 1s)", since)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var got core.NotifyMail
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &got) != nil {
		t.Fatalf("hook stdin = %q; want exactly one JSON line (the notice written before the hooks existed runs nothing)", b)
	}
	if got.ID == "" || got.FromLabel != "engine" || got.Team == "" || got.Dir == "" || got.Kind != "gate_lost" || got.Gate != "" ||
		!strings.Contains(got.Body, "no live member") || got.CreatedAt == 0 {
		t.Fatalf("hook got %+v", got)
	}
	time.Sleep(1500 * time.Millisecond) // past the timeout: the slow hook's children are gone, so it never gets to touch its file
	for _, f := range []string{slowDone, legacyOut} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s exists: the slow hook outlived its timeout, or hooks/notify or a non-executable file ran", filepath.Base(f))
		}
	}
}

// found and templates read only ~/.piggery/templates: the built-ins unpacked there by the daemon
// plus the user's own; a <cwd>/manifests template is not a template any more.
func TestTemplatesAction(t *testing.T) {
	dir := startServer(t)
	cwd := t.TempDir()
	put := func(p, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(cwd, "manifests", "p2p.yaml"), "template: p2p\nsummary: repo peers\nroles: {peer: {tools: [send]}}\n")
	put(filepath.Join(cwd, "manifests", "repo.yaml"), "template: repo\nroles: {peer: {tools: [send]}}\n")
	put(filepath.Join(manifests.Dir(dir), "mine", manifests.ManifestFile),
		"template: mine\nsummary: my flow\nroles:\n  peer: {description: me, tools: [send, inbox, who, agent]}\n")
	c := dial(t, dir)
	var solo core.JoinResult
	if _, err := c.CallInto(proto.VerbJoinAuto, core.JoinAutoArgs{Cwd: cwd, Harness: "pi", Mode: "rpc", HarnessRef: "sess-1"}, &solo); err != nil {
		t.Fatal(err)
	}
	c.AsParticipant(solo.ID, solo.Token)
	var r core.AgentResult
	if _, err := c.CallInto(proto.VerbAgent, core.AgentArgs{Action: core.AgentTemplates}, &r); err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]core.TemplateInfo{}
	for _, tpl := range r.Templates {
		names = append(names, tpl.Name)
		byName[tpl.Name] = tpl
	}
	if strings.Join(names, " ") != "amp-like council gastown-like mine p2p slp supervisor-executor" {
		t.Fatalf("templates = %v; want the home's only", names)
	}
	if p := byName["p2p"]; p.Summary == "repo peers" || p.From != filepath.Join(manifests.Dir(dir), "p2p") {
		t.Fatalf("p2p = %+v; want the unpacked built-in, not <cwd>/manifests", p)
	}
	if m := byName["mine"]; m.Summary != "my flow" || len(m.Roles) != 1 || m.Roles[0].Description != "me" {
		t.Fatalf("mine = %+v", m)
	}
	for _, tpl := range r.Templates {
		if tpl.Summary == "" || tpl.Error != "" {
			t.Fatalf("template %s = %+v; want a summary", tpl.Name, tpl)
		}
	}
	_, err := c.Call(proto.VerbAgent, core.AgentArgs{Action: core.AgentFound, Template: "repo"})
	wantCode(t, err, core.CodeNotFound)
}

// Live test regression: an adapter identifies again on its connection after found or admit to read
// its new role. That is not a reconnect: the open turn goes on and its end acks (a reconnect on a
// new connection closes it unacked).
func TestIdentifyAgainKeepsTheTurn(t *testing.T) {
	dir := startServer(t)
	_, alice, bob := p2pTeam(t, dir)
	b := rawDial(t, dir, bob)
	identify := func(a core.IdentifyArgs) core.IdentifyResult {
		t.Helper()
		r := b.call(proto.VerbIdentify, a)
		var res core.IdentifyResult
		if !r.OK || json.Unmarshal(r.Result, &res) != nil {
			t.Fatalf("identify: %+v", r)
		}
		return res
	}
	run := identify(core.IdentifyArgs{NewRun: true, Harness: "claude"}).RunID
	a := dial(t, dir)
	a.AsParticipant(alice.ID, alice.Token)
	if _, err := a.Call(proto.VerbSend, core.SendArgs{To: "bob", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	if r := b.call(proto.VerbHarnessEvent, core.HarnessEventArgs{Event: core.HarnessTurnStart, PromptID: "p1"}); !r.OK {
		t.Fatalf("turn_start: %+v", r)
	}
	identify(core.IdentifyArgs{RunID: run})
	if r := b.call(proto.VerbHarnessEvent, core.HarnessEventArgs{Event: core.HarnessTurnEnd, PromptID: "p1",
		Outcome: core.HarnessOutcomeOK}); !r.OK {
		t.Fatalf("turn_end: %+v", r)
	}
	var d []core.Delivered
	if r := b.call(proto.VerbInbox, core.InboxArgs{}); !r.OK || json.Unmarshal(r.Result, &d) != nil || len(d) != 0 {
		t.Fatalf("unacked after the turn's end = %+v %+v; want the turn acked", r, d)
	}
}

// A wake goes to the participant's newest identified connection only, naming its session id (Codex
// runs the old MCP server ~30 s after /clear; two nudges would be two turns).
func TestWakeGoesToTheNewestConnection(t *testing.T) {
	dir := startServer(t)
	_, alice, bob := p2pTeam(t, dir)
	old, cur := rawDial(t, dir, bob), rawDial(t, dir, bob)
	var id core.IdentifyResult
	r := old.call(proto.VerbIdentify, core.IdentifyArgs{NewRun: true, Harness: "claude"})
	if !r.OK || json.Unmarshal(r.Result, &id) != nil {
		t.Fatalf("identify: %+v", r)
	}
	if r := cur.call(proto.VerbIdentify, core.IdentifyArgs{RunID: id.RunID}); !r.OK {
		t.Fatalf("identify again: %+v", r)
	}
	if r := cur.call(proto.VerbHarnessEvent, core.HarnessEventArgs{Event: core.HarnessSessionStart, HarnessRef: "s-2"}); !r.OK {
		t.Fatalf("session_start: %+v", r)
	}
	a := dial(t, dir)
	a.AsParticipant(alice.ID, alice.Token)
	if _, err := a.Call(proto.VerbSend, core.SendArgs{To: "bob", Body: "wake up"}); err != nil {
		t.Fatal(err)
	}
	cur.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var push proto.Push
	if err := cur.dec.Decode(&push); err != nil || push.Event != proto.EventWake || push.Ref != "s-2" {
		t.Fatalf("newest connection: push %+v, %v; want a wake naming the session", push, err)
	}
	old.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if err := old.dec.Decode(&push); err == nil {
		t.Fatalf("older connection got %+v; want nothing", push)
	}
}

// A worker's identity is bound to its process: identify and harness.event with the worker's token come
// only from the process the driver spawned or its direct child (a hook, the MCP server). A harness nested
// in the worker inherits PIGGERY_ID and PIGGERY_TOKEN and is refused even two levels down: `zsh -lc
// 'codex exec …'` execs in place, so the nested harness is the worker's child and its hook a grandchild
// (live, Codex worker). The clients are this test binary run again as TestWorkerPeerClient.
func TestNestedHarnessCannotSpeakForTheWorker(t *testing.T) {
	dir := startServer(t)
	out := t.TempDir()
	t.Setenv("PGTEST_DIR", dir)
	t.Setenv("PGTEST_OUT", out)
	prof := workerProfile(t, "nested")
	if err := os.MkdirAll(filepath.Join(dir, "harness"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "harness", "pi.json"), prof, 0o600); err != nil {
		t.Fatal(err)
	}
	_, alice, _ := p2pTeam(t, dir)
	a := dial(t, dir)
	a.AsParticipant(alice.ID, alice.Token)
	if _, err := a.Call(proto.VerbAgent, core.AgentArgs{Action: core.AgentSpawn, Role: "peer", Name: "w1", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	read := func(tag string) string {
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if b, err := os.ReadFile(filepath.Join(out, tag)); err == nil {
				return string(b)
			}
			if time.Now().After(deadline) {
				t.Fatalf("no result from the %s client", tag)
			}
		}
	}
	if got := read("direct"); got != "identify ok; harness.event ok" {
		t.Fatalf("direct child of the worker: %s; want both taken", got)
	}
	if got := read("nested"); got != "identify unauthorized; harness.event unauthorized" {
		t.Fatalf("harness nested in the worker: %s; want both refused", got)
	}
}

// TestWorkerPeerClient is the client of TestNestedHarnessCannotSpeakForTheWorker (skipped when
// run on its own): it speaks with the worker's inherited identity and writes what it got.
func TestWorkerPeerClient(t *testing.T) {
	tag := os.Getenv("PGTEST_TAG")
	if tag == "" {
		t.Skip("a client run by TestNestedHarnessCannotSpeakForTheWorker")
	}
	c, err := cli.Dial(os.Getenv("PGTEST_DIR"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.AsParticipant(os.Getenv("PIGGERY_ID"), os.Getenv("PIGGERY_TOKEN"))
	result := func(err error) string {
		var ce *core.Error
		if errors.As(err, &ce) {
			return ce.Code
		}
		if err != nil {
			return err.Error()
		}
		return "ok"
	}
	_, err1 := c.Call(proto.VerbIdentify, core.IdentifyArgs{Harness: "pi", Mode: "rpc", RunID: os.Getenv("PIGGERY_RUN_ID")})
	_, err2 := c.Call(proto.VerbHarnessEvent, core.HarnessEventArgs{Event: core.HarnessTurnStart, PromptID: tag})
	got := "identify " + result(err1) + "; harness.event " + result(err2)
	if err := os.WriteFile(filepath.Join(os.Getenv("PGTEST_OUT"), tag), []byte(got), 0o600); err != nil {
		t.Fatal(err)
	}
}
