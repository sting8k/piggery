package core_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/store"
	"github.com/sting8k/piggery/manifests"
)

// A solo founds a team once: it moves into a team named after its cwd, as the template's
// auto_join_role (or only role), same participant and run; a second founder in the same
// directory gets name-2. An unknown template and a template with several roles and no
// auto_join_role are refused. (A member founding leaves its team: TestLeave.)
func TestFound(t *testing.T) {
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	home := t.TempDir()
	if err := manifests.Unpack(home); err != nil {
		t.Fatal(err)
	}
	e := core.New(db, core.WithTemplates(func(name, _ string) (string, error) {
		return manifests.Resolve(name, home)
	}))
	dir := filepath.Join(t.TempDir(), "api")
	if err := os.MkdirAll(filepath.Join(manifests.Dir(home), "multi"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	multi := "template: m\nroles: {a: {tools: [send]}, b: {tools: [send]}}\n"
	if err := os.WriteFile(filepath.Join(manifests.Dir(home), "multi", manifests.ManifestFile), []byte(multi), 0o600); err != nil {
		t.Fatal(err)
	}
	solo := func(ref string) core.Caller {
		t.Helper()
		j, err := e.JoinAuto(ctx, core.JoinAutoArgs{Cwd: dir, Harness: "pi", Mode: "rpc", HarnessRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := e.Authenticate(ctx, j.ID, j.Token)
		return c
	}
	found := func(c core.Caller, template string) (core.AgentResult, error) {
		return e.Agent(ctx, c, core.AgentArgs{Action: core.AgentFound, Template: template})
	}
	s1 := solo("s1")
	if _, err := found(s1, "nope"); code(err) != core.CodeNotFound {
		t.Fatalf("unknown template: %v", err)
	}
	var ce *core.Error
	if _, err := found(s1, "multi"); !errors.As(err, &ce) || ce.RuleID != "found.no_role" {
		t.Fatalf("several roles, no auto_join_role: %v", err)
	}
	r, err := found(s1, "lead-peer")
	if err != nil || r.ParticipantID != s1.ParticipantID || r.RunID != s1.RunID || r.Token != "" {
		t.Fatalf("found = %+v, %v; want the same participant and run", r, err)
	}
	id, err := e.Identify(ctx, s1, core.IdentifyArgs{RunID: s1.RunID})
	if err != nil || id.Role != "lead" || id.TeamID != r.TeamID || !strings.Contains(id.RoleCard, "team \"api\"") {
		t.Fatalf("identify after found = %+v, %v", id, err)
	}
	r2, err := found(solo("s2"), "")
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM teams WHERE id=?`, r2.TeamID).Scan(&name); err != nil || name != "api-2" {
		t.Fatalf("second team name = %q, %v", name, err)
	}
}
