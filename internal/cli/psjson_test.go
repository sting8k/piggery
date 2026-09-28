package cli

import (
	"bytes"
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
)

// ps --json carries the text ps's grouping as "projects" (one source: the same grouping code):
// projects in ps order with their short label and full path, teams and solos in display order,
// a team's members as its reports_to tree, cwds as ps shows them. What the daemon sent stays
// byte for byte.
func TestPsJSONProjects(t *testing.T) {
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

// ps --json is top's All tab: projects list what top lists, in its order (a team closed within
// the last hour as a "closed" unit, an older one not), and a worker's ctx and turns are top's
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
	if ws := stats["w"]; ws != (workerStats{ctx: 12345, hasCtx: true, turns: 2}) {
		t.Fatalf("top's stats %+v", ws)
	}
	projects := psProjects(r, stats)
	var ids []string // as top.items names them
	for _, p := range projects {
		for _, u := range p.Units {
			if u.Kind == "closed" {
				ids = append(ids, closedRow+u.ID)
				continue
			}
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
