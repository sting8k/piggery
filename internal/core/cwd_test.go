package core_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
)

const lanes = `
template: lanes
roles:
  lead:   {can_spawn: [worker], can_set_cwd: true, tools: [send, inbox, who, agent]}
  worker: {can_spawn: [worker], tools: [send, inbox, who, agent]}
routing:
  - {from: lead, to: worker, allow: true}
  - {from: worker, to: worker, allow: true}
limits: {depth: 3, concurrency: 10}
`

type cwdFixture struct {
	db   *sql.DB
	e    *core.Engine
	rt   *fakeRuntime
	lead core.Caller
	root string
}

// newCwdFixture is a team whose root is root (symlinks resolved), its lead there, and the
// engine given allowed as spawn.allowed_roots.
func newCwdFixture(t *testing.T, root string, allowed ...string) cwdFixture {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rt := &fakeRuntime{}
	e := core.New(db, core.WithRuntime(rt), core.WithAllowedRoots(allowed))
	team, err := e.TeamUp(ctx, core.TeamUpArgs{Manifest: lanes, Name: "lanes", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.Join(ctx, core.JoinArgs{Team: team.ID, Role: "lead", Name: "lead", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	lead, err := e.Authenticate(ctx, j.ID, j.Token)
	if err != nil {
		t.Fatal(err)
	}
	return cwdFixture{db: db, e: e, rt: rt, lead: lead, root: root}
}

func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A spawn's cwd must lie inside the team root, a git worktree of the repo at the root, or
// spawn.allowed_roots, symlinks resolved; a role without can_set_cwd keeps its own. The spawn event
// records where the worker runs.
// js is s as a JSON string (a Windows path holds backslashes).
func js(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSpawnCwdBounds(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root, outside, allowed := realDir(t), realDir(t), realDir(t)
	lane := filepath.Join(realDir(t), "lane") // a worktree outside the root
	git(t, root, "init", "-q")
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, root, "worktree", "add", "-q", lane)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	f := newCwdFixture(t, root, allowed)
	spawn := func(by core.Caller, name, cwd string) (string, error) {
		_, err := f.e.Agent(ctx, by, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: name, Task: "t", Cwd: cwd})
		if err != nil {
			return "", err
		}
		return f.rt.starts[len(f.rt.starts)-1].Cwd, nil
	}
	for _, c := range []struct{ name, cwd, want string }{
		{"in-root", "sub", filepath.Join(root, "sub")}, // relative to the lead's cwd
		{"in-lane", lane, lane},
		{"allowed", allowed, allowed},
	} {
		if got, err := spawn(f.lead, c.name, c.cwd); err != nil || got != c.want {
			t.Fatalf("%s: cwd %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	for _, cwd := range []string{outside, "escape"} {
		if _, err := spawn(f.lead, "out", cwd); rule(err) != "bounds/cwd.bounds" {
			t.Fatalf("cwd %s: %v; want bounds/cwd.bounds", cwd, err)
		}
	}
	var payload string
	if err := f.db.QueryRow(`SELECT payload FROM events WHERE type='spawned' AND payload LIKE '%in-lane%'`).Scan(&payload); err != nil ||
		!strings.Contains(payload, `"cwd":`+js(lane)) {
		t.Fatalf("spawned event %s, %v; want its cwd", payload, err)
	}
	s := f.rt.starts[0] // in-root, a worker: no can_set_cwd
	w, err := f.e.Authenticate(ctx, s.ParticipantID, s.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spawn(w, "w2", root); rule(err) != "permission/can_set_cwd" {
		t.Fatalf("worker with a cwd: %v; want permission/can_set_cwd", err)
	}
	if got, err := spawn(w, "w3", ""); err != nil || got != filepath.Join(root, "sub") {
		t.Fatalf("worker without a cwd: %q, %v; want its own", got, err)
	}
}

// Resume runs a worker in the cwd stored at spawn; when that directory is gone it says so and
// starts nothing, never another directory.
func TestResumeNeedsTheWorkersCwd(t *testing.T) {
	root := realDir(t)
	dir := filepath.Join(root, "lane")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := newCwdFixture(t, root)
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "t", Cwd: "lane"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	_, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"})
	if err == nil || !strings.Contains(err.Error(), dir+" no longer exists") || len(f.rt.starts) != 1 {
		t.Fatalf("resume without its cwd: %v, %d starts", err, len(f.rt.starts))
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"}); err != nil || f.rt.starts[1].Cwd != dir {
		t.Fatalf("resume: %v; want it in %s", err, dir)
	}
}

// A member's cwd is inside its team's bounds, so a worker that inherits it is too: the live hole of
// was a member joined in the root's parent (root …/sub, member …) whose worker, spawned with no
// cwd, ran in the parent. Join refuses that cwd; a worker whose stored cwd is outside anyway (a
// member row from before, a session reopened elsewhere) is refused at spawn and at resume.
func TestInheritedCwdIsBounded(t *testing.T) {
	parent := realDir(t)
	root := filepath.Join(parent, "sub")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	f := newCwdFixture(t, root)
	if _, err := f.e.Join(ctx, core.JoinArgs{Team: "lanes", Role: "lead", Name: "pp", Cwd: parent}); rule(err) != "bounds/cwd.bounds" {
		t.Fatalf("join in the root's parent: %v; want bounds/cwd.bounds", err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w1", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentStop, Target: "w1"}); err != nil {
		t.Fatal(err)
	}
	// Both rows moved outside, as a reopened session's or an older daemon's would be.
	if _, err := f.db.Exec(`UPDATE participants SET cwd=?`, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentSpawn, Role: "worker", Name: "w2", Task: "t"}); rule(err) != "bounds/cwd.bounds" {
		t.Fatalf("spawn inheriting %s: %v; want bounds/cwd.bounds", parent, err)
	}
	if _, err := f.e.Agent(ctx, f.lead, core.AgentArgs{Action: core.AgentResume, Target: "w1"}); rule(err) != "bounds/cwd.bounds" {
		t.Fatalf("resume in %s: %v; want bounds/cwd.bounds", parent, err)
	}
	if len(f.rt.starts) != 1 {
		t.Fatalf("%d starts; want only w1's first", len(f.rt.starts))
	}
}
