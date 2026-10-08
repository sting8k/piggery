package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
	"github.com/sting8k/piggery/internal/view"
)

// ps --json carries the text ps's grouping as "projects" (one source: the same grouping code):
// projects in ps order with their short label and full path, teams and solos in display order,
// a team's members as its reports_to tree, cwds as ps shows them. What the daemon sent stays
// byte for byte.
func TestPsJSONProjects(t *testing.T) {
	skipOnWindows(t, "the fixture's paths are unix paths (the view helpers are covered for Windows paths in internal/view)")
	r := proto.PsResult{PID: 7, State: core.State{
		Teams: []core.TeamState{
			{ID: "t1", Name: "shop", Root: "/w/a/shop", CreatedAt: 10, Members: []core.MemberState{
				{ID: "lead", Name: "lead", Cwd: "/w/a/shop", LastActivity: 100},
				{ID: "dev", Name: "dev", ReportsTo: "lead", Cwd: "/w/a/shop/web"},
				{ID: "rev", Name: "rev", ReportsTo: "lead", Cwd: "/elsewhere"}}},
			{ID: "t2", Name: "ops", Root: "/w/b/shop", CreatedAt: 20, Members: []core.MemberState{
				{ID: "o", Name: "o", Cwd: "/w/b/shop", LastActivity: 900}}}},
		Solos: []core.SoloState{{ID: "s", Name: "fern", Cwd: "/w/a/shop/docs", CreatedAt: 5}},
	}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	out, err := withProjects(raw, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, raw[:len(raw)-1]) {
		t.Fatalf("the daemon's fields changed:\n%s", out)
	}
	var got struct {
		PID      int         `json:"pid"`
		Projects []psProject `json:"projects"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.PID != 7 {
		t.Fatalf("not one JSON object: %v\n%s", err, out)
	}
	want := []psProject{
		{Label: "…/b/shop", Path: "/w/b/shop", Units: []psUnit{{Kind: "team", ID: "t2", Members: []psMember{{ID: "o", Cwd: ""}}}}},
		{Label: "…/a/shop", Path: "/w/a/shop", Units: []psUnit{
			{Kind: "solo", ID: "s", Cwd: "./docs"},
			{Kind: "team", ID: "t1", Members: []psMember{
				{ID: "lead"},
				{ID: "dev", Depth: 1, Prefix: "├─ ", Cwd: "./web"},
				{ID: "rev", Depth: 1, Prefix: "└─ ", Cwd: "/elsewhere"}}}}},
	}
	gj, _ := json.Marshal(got.Projects)
	wj, _ := json.Marshal(want)
	if !bytes.Equal(gj, wj) {
		t.Fatalf("projects\n got %s\nwant %s", gj, wj)
	}
	var dirs []string // the text ps lists the same directories, in the same order
	for _, l := range psLines(r, time.Now(), nil, nil) {
		if l.kind == "dir" {
			dirs = append(dirs, strings.TrimSuffix(l.text, "/"))
		}
	}
	for i, p := range got.Projects {
		if i >= len(dirs) || !strings.HasSuffix(dirs[i], p.Label) {
			t.Fatalf("text ps directories %v, json projects %s", dirs, gj)
		}
	}
}

// ps --json is top's All tab, but for what is gone: projects list what top lists, in its order (a
// team closed within the last hour as a "closed" unit, an older one not; top's All has no closed
// team), and a worker's ctx and turns are top's
// stats (context of the current run, turns of every run).
func TestPsJSONMatchesTop(t *testing.T) {
	dir := t.TempDir()
	logs := map[string]string{
		"r1": `{"type":"turn_end"}` + "\n",
		"r2": `{"type":"message_end","message":{"role":"assistant","usage":{"totalTokens":12345}}}` + "\n" + `{"type":"turn_end"}` + "\n",
	}
	for run, body := range logs {
		path := local.LogPath(dir, "w", run)
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte(body), 0o600)
	}
	now := time.Now().UnixMilli()
	r := proto.PsResult{State: core.State{
		Teams: []core.TeamState{{ID: "t", Name: "shop", Root: "/w/shop", Members: []core.MemberState{
			{ID: "lead", Name: "lead", Cwd: "/w/shop"},
			{ID: "w", Name: "w", ReportsTo: "lead", Headless: true, RunID: "r2", Cwd: "/w/shop"}}}},
		Closed: []core.ClosedTeam{
			{TeamState: core.TeamState{ID: "c1", Name: "old", Root: "/w/shop", Members: []core.MemberState{{ID: "x", Name: "x"}}}, ClosedAt: now - 2*time.Hour.Milliseconds()},
			{TeamState: core.TeamState{ID: "c2", Name: "done", Root: "/w/shop", Members: []core.MemberState{{ID: "y", Name: "y"}}}, ClosedAt: now - time.Minute.Milliseconds()}},
	}}
	top := &topModel{dir: dir, ps: r, logs: readLogs(dir, r, nil)}
	stats := top.stats()
	if ws := stats["w"]; ws != (view.Stats{Ctx: 12345, HasCtx: true, Turns: 2}) {
		t.Fatalf("top's stats %+v", ws)
	}
	projects := psProjects(r, stats)
	var ids []string // as top.items names them
	for _, p := range projects {
		for _, u := range p.Units {
			if u.Kind == "closed" { // top's All lists none
				continue
			}
			ids = append(ids, closedRow+u.ID) // an open team's line
			for _, m := range u.Members {
				ids = append(ids, m.ID)
				if m.ID == "w" && (m.Ctx == nil || *m.Ctx != 12345 || m.Turns == nil || *m.Turns != 2) {
					t.Errorf("w: ctx %v turns %v", m.Ctx, m.Turns)
				}
				if m.ID == "lead" && (m.Ctx != nil || m.Turns != nil) {
					t.Errorf("a session has no ctx or turns: %+v", m)
				}
			}
		}
	}
	if want := top.items(); !slices.Equal(ids, want) {
		t.Fatalf("projects %v, top lists %v", ids, want)
	}
}

// ps --view is what the Paseo plugin draws from: through the real command it prints one JSON
// document with the version the plugin checks, and every row top's list selects is in it with the
// same id, its actions as top decides them, and its Overview.
func TestPsViewCarriesWhatTopSelects(t *testing.T) {
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
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
	}
	var out, errOut bytes.Buffer
	if code := Main(dir, []string{"ps", "--view"}, &out, &errOut); code != 0 {
		t.Fatalf("ps --view: exit %d\n%s", code, errOut.String())
	}
	var got struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Version != view.Version {
		t.Fatalf("ps --view: version %d (%v), want %d\n%s", got.Version, err, view.Version, out.String())
	}

	// a snapshot with rows: a team with a worker and a session, a solo, a closed team
	r := proto.PsResult{State: core.State{
		Teams: []core.TeamState{{ID: "t", Name: "shop", Root: "/w/shop", Members: []core.MemberState{
			{ID: "lead", Name: "lead", State: "idle", Cwd: "/w/shop"},
			{ID: "w", Name: "w", ReportsTo: "lead", State: "working", Headless: true, RunID: "r", Cwd: "/w/shop"},
			{ID: "old", Name: "old", ReportsTo: "lead", State: "gone", Headless: true, Cwd: "/w/shop"}}}},
		Solos:  []core.SoloState{{ID: "s", Name: "fern", Cwd: "/w/shop", State: "idle"}},
		Closed: []core.ClosedTeam{{TeamState: core.TeamState{ID: "c", Name: "done", Root: "/w/shop", Members: []core.MemberState{{ID: "x", Name: "x", State: "gone"}}}, ClosedAt: time.Now().UnixMilli()}},
	}}
	top := &topModel{dir: t.TempDir(), ps: r, fold: topState{Teams: map[string]bool{}, Gone: map[string]bool{}}}
	raw, err := json.Marshal(viewDoc(r, nil, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		All struct {
			Dirs []struct {
				Blocks []struct {
					Head *struct{ Detail string }
					Rows []struct {
						ID      string
						Actions *struct{ Tail bool }
					}
				}
			}
		}
		Details map[string]json.RawMessage
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	rows := map[string]*struct{ Tail bool }{}
	for _, d := range doc.All.Dirs {
		for _, b := range d.Blocks {
			if b.Head != nil {
				rows[b.Head.Detail] = nil
			}
			for _, r := range b.Rows {
				rows[r.ID] = r.Actions
			}
		}
	}
	for _, id := range top.items() {
		a, ok := rows[id]
		if !ok || doc.Details[id] == nil {
			t.Errorf("top selects %q: in ps --view rows %v, details %v", id, ok, doc.Details[id] != nil)
		} else if a != nil && a.Tail != top.tailable(id) {
			t.Errorf("%q: ps --view tail %v, top %v", id, a.Tail, top.tailable(id))
		}
	}
}
