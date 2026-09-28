package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/proto"
)

// A worker's context in top is the usage of its latest assistant message_end in its current
// run, not a sum; its turns count turn_end across all its runs; refreshes read only what the
// logs gained.
func TestTopWorkerStats(t *testing.T) {
	dir := t.TempDir()
	ps := proto.PsResult{State: core.State{Teams: []core.TeamState{{Members: []core.MemberState{
		{ID: "w1", RunID: "r1", Headless: true, Capabilities: []string{core.CapUsage}}}}}}}
	path := local.LogPath(dir, "w1", "r1")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.WriteString(s)
	}
	write(`{"type":"message_end","message":{"role":"assistant","usage":{"input":857,"output":142,"cacheRead":14464,"cacheWrite":0,"totalTokens":15463}}}
{"type":"turn_end"}
{"type":"message_end","message":{"role":"user"}}
{"type":"message_end","message":{"role":"assistant","usage":{"input":900,"output":100,"cacheRead":15000,"cacheWrite":0,"totalTokens":16000}}}
`)
	old := local.LogPath(dir, "w1", "r0") // an earlier run: its turns count, its usage does not
	if err := os.WriteFile(old, []byte(`{"type":"turn_end"}
{"type":"message_end","message":{"role":"assistant","usage":{"totalTokens":99999}}}
{"type":"turn_end"}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &topModel{dir: dir, ps: ps}
	m.logs = readLogs(dir, ps, nil)
	if got := m.stats()["w1"]; got.ctx != 16000 || got.turns != 3 {
		t.Fatalf("stats = %+v; want ctx 16000 (latest of the current run, not a sum), turns 3 (both runs)", got)
	}
	write(`{"type":"message_end","message":{"role":"assistant","usage":{"input":10,"output":5,"cacheRead":2000,"cacheWrite":0}}}
{"type":"turn_end"}
`)
	m.logs = readLogs(dir, ps, m.logs)
	if got := m.stats()["w1"]; got.ctx != 2015 || got.turns != 4 {
		t.Fatalf("stats after append = %+v; want ctx 2015 (no totalTokens: the sum of its parts), turns 4", got)
	}
}

// Switching the tail from w1 to w2 never shows w1's lines under w2, also while the fetch in
// flight still carries w1's tail.
func TestTopTailFollowsTheSelection(t *testing.T) {
	ps := proto.PsResult{State: core.State{Teams: []core.TeamState{{Name: "t", Members: []core.MemberState{
		{ID: "w1", Name: "w1", Headless: true, Capabilities: []string{core.CapUsage}}, {ID: "w2", Name: "w2", Headless: true, Capabilities: []string{core.CapUsage}}}}}}}
	m := newTopModel(nil, "")
	m.Update(fetched{ps: ps})
	m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m.Update(fetched{ps: ps, tail: tailState{worker: "w1", lines: []string{"from w1"}}})
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.sel != "w2" || !m.side || m.sideTab != sideTail {
		t.Fatalf("sel %q side %v tab %d; want w2's tail", m.sel, m.side, m.sideTab)
	}
	m.Update(fetched{ps: ps, tail: tailState{worker: "w1", lines: []string{"from w1", "more from w1"}}})
	if out := m.render(); strings.Contains(out, "from w1") {
		t.Fatalf("w2's tail shows w1's lines:\n%s", out)
	}
}

// Tabs are All, each team (Closed last). Switching tabs moves the selection into the new tab; a
// refresh keeps it while the row is there, else takes the tab's first row; a tab whose team
// closed falls back to All.
func TestTopTabsKeepASelection(t *testing.T) {
	ps := proto.PsResult{State: core.State{
		Teams: []core.TeamState{
			{ID: "ta", Name: "a", Members: []core.MemberState{{ID: "a1", Name: "a1"}, {ID: "a2", Name: "a2"}}},
			{ID: "tb", Name: "b", Members: []core.MemberState{{ID: "b1", Name: "b1"}}}},
		Solos: []core.SoloState{{ID: "s1", Name: "s1"}}}}
	m := newTopModel(nil, "")
	m.Update(fetched{ps: ps})
	right := tea.KeyPressMsg{Code: tea.KeyRight}
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.tab != "" || m.sel != "a2" {
		t.Fatalf("start: tab %q sel %q; want All, a2", m.tab, m.sel)
	}
	m.Update(fetched{ps: ps})
	if m.sel != "a2" {
		t.Fatalf("refresh moved the selection to %q", m.sel)
	}
	var got []string
	for range 3 {
		m.Update(right)
		got = append(got, m.tab+":"+m.sel)
	}
	if want := "ta:a2 tb:b1 :b1"; strings.Join(got, " ") != want {
		t.Fatalf("tabs: got %q want %q", strings.Join(got, " "), want)
	}
	m.Update(right) // team a
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	gone := ps
	gone.Teams = []core.TeamState{{ID: "ta", Name: "a", Members: []core.MemberState{{ID: "a1", Name: "a1"}}}, ps.Teams[1]}
	m.Update(fetched{ps: gone})
	if m.tab != "ta" || m.sel != "a1" {
		t.Fatalf("a2 left: tab %q sel %q; want ta, a1", m.tab, m.sel)
	}
	gone.Teams = gone.Teams[1:]
	m.Update(fetched{ps: gone})
	if m.tab != "" || m.sel != "b1" {
		t.Fatalf("team a closed: tab %q sel %q; want All, b1", m.tab, m.sel)
	}
}

// A click lands on what the frame drew there: on a member's row it selects that member, on a
// tab it switches to that tab.
func TestTopClickSelectsWhatWasDrawn(t *testing.T) {
	ps := proto.PsResult{State: core.State{
		Teams: []core.TeamState{
			{ID: "ta", Name: "a", Members: []core.MemberState{{ID: "a1", Name: "a1"}, {ID: "a2", Name: "a2"}}},
			{ID: "tb", Name: "b", Members: []core.MemberState{{ID: "b1", Name: "b1"}}}}}}
	m := newTopModel(nil, "")
	m.w, m.h = 120, 30
	m.Update(fetched{ps: ps})
	find := func(text string) (int, int) {
		sgr := regexp.MustCompile("\x1b\\[[0-9;]*m")
		for y, l := range strings.Split(sgr.ReplaceAllString(m.render(), ""), "\n") {
			if i := strings.Index(l, text); i >= 0 {
				return lipgloss.Width(l[:i]), y
			}
		}
		t.Fatalf("%q not drawn", text)
		return 0, 0
	}
	x, y := find("a2 ")
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if m.sel != "a2" {
		t.Fatalf("click on a2's row selected %q", m.sel)
	}
	x, y = find("b 1")
	m.Update(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	if m.tab != "tb" || m.sel != "b1" {
		t.Fatalf("click on tab b: tab %q sel %q", m.tab, m.sel)
	}
}

// A closed team is listed as one line: in All while it closed within the hour, in Closed until gc
// removes it; enter on its line expands it into its members, which can then be selected.
func TestTopClosedTeamsExpand(t *testing.T) {
	now := time.Now()
	ps := proto.PsResult{State: core.State{
		Teams: []core.TeamState{{ID: "ta", Name: "a", Members: []core.MemberState{{ID: "a1", Name: "a1"}}}},
		Closed: []core.ClosedTeam{
			{TeamState: core.TeamState{ID: "tr", Name: "recent", Members: []core.MemberState{{ID: "r1", Name: "r1", State: "gone"}}},
				ClosedAt: now.Add(-10 * time.Minute).UnixMilli()},
			{TeamState: core.TeamState{ID: "to", Name: "old", Members: []core.MemberState{{ID: "o1", Name: "o1", State: "gone"}}},
				ClosedAt: now.Add(-2 * time.Hour).UnixMilli()}}}}
	m := newTopModel(nil, "")
	m.Update(fetched{ps: ps})
	if got, want := strings.Join(m.items(), " "), "a1 "+closedRow+"tr"; got != want {
		t.Fatalf("All lists %q; want the open member and the recent closed team, collapsed", got)
	}
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got, want := strings.Join(m.items(), " "), "a1 "+closedRow+"tr r1"; got != want {
		t.Fatalf("after enter on the closed team All lists %q; want its member r1 too", got)
	}
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.sel != "r1" {
		t.Fatalf("selected %q; want r1, a member of the expanded closed team", m.sel)
	}
	for m.tab != tabClosed {
		m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	if got, want := strings.Join(m.items(), " "), closedRow+"tr r1 "+closedRow+"to"; got != want {
		t.Fatalf("Closed lists %q; want every closed team, newest first", got)
	}
}

// On a narrow terminal the details start hidden; enter shows them instead of the list and esc
// goes back to the list with the same member selected.
func TestTopNarrowDetailsFullScreen(t *testing.T) {
	ps := proto.PsResult{State: core.State{Teams: []core.TeamState{{ID: "ta", Name: "a", Members: []core.MemberState{
		{ID: "a1", Name: "a1"}, {ID: "a2", Name: "a2"}}}}}}
	m := newTopModel(nil, "")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.Update(fetched{ps: ps})
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.sideShown() || m.sel != "a2" {
		t.Fatalf("narrow start: details shown %v, sel %q; want hidden, a2", m.sideShown(), m.sel)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.full || !m.sideShown() {
		t.Fatal("enter did not show the details full screen")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.sideShown() || m.sel != "a2" {
		t.Fatalf("after esc: details shown %v, sel %q; want the list, a2 still selected", m.sideShown(), m.sel)
	}
}

// Tool output in worker logs carries terminal escapes; a tail line never passes them on.
func TestTailLineDropsControlSequences(t *testing.T) {
	rec := `{"type":"tool_execution_end","toolName":"bash","result":{"content":[{"type":"text","text":"HTTP/1.1 200 OK\r\n\u001b[31m\u001b[1mWARNING: dev server\u001b[0m\u001b]0;title\u0007 done\u0008"}]}}`
	l, ok := tailLine([]byte(rec))
	if !ok || l != "< bash ok: HTTP/1.1 200 OK WARNING: dev server done" {
		t.Fatalf("tail line = %q", l)
	}
}

// A tool line shows what the call does: bash its command, edit its path, another tool its
// arguments as key=value.
func TestTailLineShortensToolArgs(t *testing.T) {
	for rec, want := range map[string]string{
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"go test ./...","timeout":60}}`:        "> bash go test ./...",
		`{"type":"tool_execution_start","toolName":"edit","args":{"path":"internal/core/x.go","edits":[{"a":1}]}}`: "> edit internal/core/x.go",
		`{"type":"tool_execution_start","toolName":"grep","args":{"pattern":"TODO","path":"internal","limit":5}}`:  "> grep limit=5 path=internal pattern=TODO",
	} {
		if l, ok := tailLine([]byte(rec)); !ok || l != want {
			t.Fatalf("tail line = %q, want %q", l, want)
		}
	}
}

// Members show as the tree of reports_to: every parent before its children, each child under
// its own parent in team order; a member whose parent left the team is a root.
func TestMemberTree(t *testing.T) {
	ms := []core.MemberState{
		{ID: "s", Name: "s"},
		{ID: "lead-a", Name: "lead-a", ReportsTo: "s"},
		{ID: "lead-b", Name: "lead-b", ReportsTo: "s", State: "gone"},
		{ID: "peer-a1", Name: "peer-a1", ReportsTo: "lead-a"},
		{ID: "peer-b1", Name: "peer-b1", ReportsTo: "lead-b"},
		{ID: "peer-a2", Name: "peer-a2", ReportsTo: "lead-a"},
		{ID: "orphan", Name: "orphan", ReportsTo: "left-the-team"},
	}
	var got []string
	for _, r := range memberTree(ms) {
		got = append(got, fmt.Sprintf("%d %s<-%s", r.depth, r.m.Name, r.m.ReportsTo))
	}
	want := []string{"0 s<-", "1 lead-a<-s", "2 peer-a1<-lead-a", "2 peer-a2<-lead-a", "1 lead-b<-s",
		"2 peer-b1<-lead-b", "0 orphan<-left-the-team"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tree order:\n got %v\nwant %v", got, want)
	}
}
