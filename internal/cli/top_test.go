package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	if got := m.stats()["w1"]; got.Ctx != 16000 || got.Turns != 3 {
		t.Fatalf("stats = %+v; want ctx 16000 (latest of the current run, not a sum), turns 3 (both runs)", got)
	}
	write(`{"type":"message_end","message":{"role":"assistant","usage":{"input":10,"output":5,"cacheRead":2000,"cacheWrite":0}}}
{"type":"turn_end"}
`)
	m.logs = readLogs(dir, ps, m.logs)
	if got := m.stats()["w1"]; got.Ctx != 2015 || got.Turns != 4 {
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

// Tabs are All, each project directory (Closed last). Switching tabs moves the selection into the
// new tab; a refresh keeps it while the row is there, else takes the tab's first row; a tab whose
// project has nothing left falls back to All.
func TestTopTabsKeepASelection(t *testing.T) {
	ps := proto.PsResult{State: core.State{
		Teams: []core.TeamState{
			{ID: "ta", Name: "a", Root: "/p/a", Members: []core.MemberState{{ID: "a1", Name: "a1"}, {ID: "a2", Name: "a2"}}},
			{ID: "tb", Name: "b", Root: "/p/b", Members: []core.MemberState{{ID: "b1", Name: "b1"}}}},
		Solos: []core.SoloState{{ID: "s1", Name: "s1", Cwd: "/p/b"}}}}
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
	if want := "/p/a:a2 /p/b:b1 :b1"; strings.Join(got, " ") != want {
		t.Fatalf("tabs: got %q want %q", strings.Join(got, " "), want)
	}
	m.Update(right) // team a
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	gone := ps
	gone.Teams = []core.TeamState{{ID: "ta", Name: "a", Root: "/p/a", Members: []core.MemberState{{ID: "a1", Name: "a1"}}}, ps.Teams[1]}
	m.Update(fetched{ps: gone})
	if m.tab != "/p/a" || m.sel != "a1" {
		t.Fatalf("a2 left: tab %q sel %q; want /p/a, a1", m.tab, m.sel)
	}
	gone.Teams = gone.Teams[1:]
	m.Update(fetched{ps: gone})
	if m.tab != "" || m.sel != "b1" {
		t.Fatalf("project a emptied: tab %q sel %q; want All, b1", m.tab, m.sel)
	}
}

// A click lands on what the frame drew there: on a member's row it selects that member, on a
// tab it switches to that tab.
func TestTopClickSelectsWhatWasDrawn(t *testing.T) {
	ps := proto.PsResult{State: core.State{
		Teams: []core.TeamState{
			{ID: "ta", Name: "a", Root: "/p/a", Members: []core.MemberState{{ID: "a1", Name: "a1"}, {ID: "a2", Name: "a2"}}},
			{ID: "tb", Name: "b", Root: "/p/b", Members: []core.MemberState{{ID: "b1", Name: "b1"}}}}}}
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
	if m.tab != "/p/b" || m.sel != "b1" {
		t.Fatalf("click on tab b: tab %q sel %q", m.tab, m.sel)
	}
}

// A closed team is only in the Closed tab, until gc removes it, never in All (which lists what is
// alive); there it is one line, and enter on it expands it into its members, which can be selected.
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
	if got, want := strings.Join(m.items(), " "), closedRow+"ta a1"; got != want {
		t.Fatalf("All lists %q; want only the open team's line and member", got)
	}
	for m.tab != tabClosed {
		m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	if got, want := strings.Join(m.items(), " "), closedRow+"tr "+closedRow+"to"; got != want {
		t.Fatalf("Closed lists %q; want every closed team, newest first, one line each", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got, want := strings.Join(m.items(), " "), closedRow+"tr r1 "+closedRow+"to"; got != want {
		t.Fatalf("after enter on the closed team Closed lists %q; want its member r1 too", got)
	}
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.sel != "r1" {
		t.Fatalf("selected %q; want r1, a member of the expanded closed team", m.sel)
	}
}

// An open team whose members are all gone is not in All, which lists what is alive; its project's
// tab lists it with its members, after the projects with someone alive; when a member is back it
// is a normal team in All again, its gone member folded.
func TestTopDeadOpenTeamIsOneLine(t *testing.T) {
	ps := func(d2 string) proto.PsResult {
		return proto.PsResult{State: core.State{Teams: []core.TeamState{
			{ID: "ta", Name: "a", Root: "/p/a", Members: []core.MemberState{{ID: "a1", Name: "a1", State: "idle"}}},
			{ID: "td", Name: "dead", Root: "/p/d", Members: []core.MemberState{{ID: "d1", Name: "d1", State: "gone"}, {ID: "d2", Name: "d2", State: d2}}}}}}
	}
	m := newTopModel(nil, "")
	m.Update(fetched{ps: ps("gone")})
	if got, want := strings.Join(m.items(), " "), closedRow+"ta a1"; got != want {
		t.Fatalf("All lists %q; want only the live team", got)
	}
	var keys []string
	for _, tab := range m.tabs() {
		keys = append(keys, tab.Key)
	}
	if want := ",/p/a,/p/d"; strings.Join(keys, ",") != want {
		t.Fatalf("tabs %q; want All, the live project, then the all-gone one", keys)
	}
	for m.tab != "/p/d" {
		m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	if got, want := strings.Join(m.items(), " "), "d1 d2"; got != want {
		t.Fatalf("its project's tab lists %q; want its members, no collapsed line", got)
	}
	m.tab = ""
	m.Update(fetched{ps: ps("idle")})
	if got, want := strings.Join(m.items(), " "), closedRow+"ta a1 "+closedRow+"td d2 "+goneRow+"td"; got != want {
		t.Fatalf("with d2 back All lists %q; want a normal team, d1 folded as gone", got)
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

// A member's task sits right under the header, before team/kind/model: its title on at most two
// lines, who gave it, how its reply chain stands, and a newer unmarked mail as a "mail" row.
func TestTopOverviewShowsTheAssignment(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	ago := func(min int) int64 { return now.Add(-time.Duration(min) * time.Minute).UnixMilli() }
	long := "omp phase 2, your part: the interactive side of the extension with a very long title that goes past two lines of the pane"
	a := &core.Assignment{Seq: 175, Title: long, From: "summer-hamster", At: ago(22),
		Latest: &core.ChainMail{Seq: 183, At: ago(3), ByMember: true}, Newer: &core.NewerMail{Seq: 190, Title: "next: the dsh part", At: ago(1)}}
	m := newTopModel(nil, "")
	m.Update(fetched{ps: proto.PsResult{State: core.State{Teams: []core.TeamState{{ID: "ta", Name: "a", Members: []core.MemberState{
		{ID: "a1", Name: "a1", Role: "executor", State: "idle", Assignment: a}, {ID: "a2", Name: "a2", Role: "executor", State: "idle"}}}}}}})
	strip := regexp.MustCompile("\x1b\\[[0-9;]*m")
	side := func(sel string) string {
		m.sel = sel
		return strip.ReplaceAllString(strings.Join(m.sidebar(50, 30, now), "\n"), "")
	}
	got := strings.Split(side("a1"), "\n")
	if len(got) < 8 || !strings.HasPrefix(got[2], " task ") || !strings.Contains(got[2], "#175 omp phase 2, your part:") || !strings.HasSuffix(got[3], "…") ||
		strings.TrimSpace(got[4]) != "from summer-hamster · 22m ago" || strings.TrimSpace(got[5]) != "handed back #183 · 3m ago" ||
		!strings.HasPrefix(got[6], " mail ") || !strings.Contains(got[6], "#190 next: the dsh part · 1m ago") || !strings.HasPrefix(got[7], " team ") {
		t.Fatalf("overview = %q; want the task rows (title over two lines, from, handed back, mail) before team", got)
	}
	if strings.Contains(side("a2"), "task") {
		t.Fatal("a member without an assignment shows a task row")
	}
}

// The footer ends with the daemon's version; another cli build says how to fix it, in full when it
// fits and as the version alone before that; on a terminal too narrow for either the version goes
// and the key hints stay.
func TestTopFooterShowsTheDaemonVersion(t *testing.T) {
	old := Version
	Version = "dev-cli"
	t.Cleanup(func() { Version = old })
	ps := proto.PsResult{Version: "dev-daemon"}
	sgr := regexp.MustCompile("\x1b\\[[0-9;]*m")
	last := func(width int) string {
		m := newTopModel(nil, "")
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m.Update(fetched{ps: ps})
		lines := strings.Split(sgr.ReplaceAllString(m.render(), ""), "\n")
		return lines[len(lines)-1]
	}
	if l := last(180); !strings.HasSuffix(l, "dev-daemon (cli dev-cli: piggery restart)") {
		t.Fatalf("mismatch footer: %q", l)
	}
	if l := last(80); !strings.HasSuffix(l, "dev-daemon") || strings.Contains(l, "cli") {
		t.Fatalf("mismatch footer, room for the version only: %q", l)
	}
	Version = "dev-daemon"
	if l := last(140); !strings.HasSuffix(l, "dev-daemon") || strings.Contains(l, "cli") {
		t.Fatalf("same version footer: %q", l)
	}
	if l := last(30); strings.Contains(l, "dev-daemon") || strings.TrimSpace(l) == "" {
		t.Fatalf("narrow footer: %q, want the key hints and no version", l)
	}
}

// `x` asks once and `y` kills the selected headless worker (the same verb as `piggery kill`); any
// other key cancels; a session or a stopped worker gets a reason and no question. `x` is also the
// short name of the kill command.
func TestTopKillKey(t *testing.T) {
	ps := proto.PsResult{State: core.State{Teams: []core.TeamState{{ID: "ta", Name: "a", Members: []core.MemberState{
		{ID: "s1", Name: "boss"}, {ID: "w1", Name: "w1", Headless: true, State: "working"}, {ID: "w2", Name: "w2", Headless: true, State: "gone"}}}}}}
	var killedIDs []string
	m := newTopModel(nil, "")
	m.kill = func(id string) error { killedIDs = append(killedIDs, id); return nil }
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m.Update(fetched{ps: ps})
	press := func(s string) tea.Cmd {
		_, cmd := m.Update(tea.KeyPressMsg{Code: rune(s[0]), Text: s})
		return cmd
	}
	frame := func() string { return regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(m.render(), "") }
	m.sel = "w1"
	press("x")
	if !strings.Contains(frame(), "kill w1? y/n") {
		t.Fatalf("no question:\n%s", frame())
	}
	if cmd := press("n"); cmd != nil || len(killedIDs) != 0 || strings.Contains(frame(), "y/n") {
		t.Fatalf("another key: cmd %v, killed %v", cmd != nil, killedIDs)
	}
	press("x")
	cmd := press("y")
	if cmd == nil {
		t.Fatal("y did not kill")
	}
	m.Update(cmd())
	if !slices.Equal(killedIDs, []string{"w1"}) || !strings.Contains(frame(), "killed w1") {
		t.Fatalf("killed %v:\n%s", killedIDs, frame())
	}
	for id, want := range map[string]string{"s1": "boss is not a headless worker: stop it in its own window (Esc)", "w2": "w2 is already stopped"} {
		m.sel = id
		press("x")
		if !strings.Contains(frame(), want) || m.killing.id != "" {
			t.Fatalf("%s: want %q:\n%s", id, want, frame())
		}
	}
	if len(killedIDs) != 1 {
		t.Fatalf("killed %v; only w1", killedIDs)
	}
	if c, _, err := (&env{}).root().Find([]string{"x", "w1"}); err != nil || c.Name() != "kill" {
		t.Fatalf("x resolves to %v, %v; want kill", c, err)
	}
}

// Events are a box like the Overview's (no column-name line) that `e` folds into one line of text
// with the latest event; the keys are two lines with `x kill` on the second.
func TestTopEventsBoxAndKeyLines(t *testing.T) {
	sgr := regexp.MustCompile("\x1b\\[[0-9;]*m")
	now := time.Now().UnixMilli()
	ev := func(typ, who string) core.Event { return core.Event{Ts: now - 120_000, Type: typ, Participant: who} }
	m := newTopModel(nil, "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.Update(fetched{ps: proto.PsResult{State: core.State{Teams: []core.TeamState{{ID: "ta", Name: "a", Members: []core.MemberState{
		{ID: "a1", Name: "a1", Role: "executor", State: "idle"}}}}, Events: []core.Event{ev("spawned", "a1"), ev("handback", "a1")}}}})
	lines := func() []string { return strings.Split(sgr.ReplaceAllString(m.render(), ""), "\n") }

	m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"}) // events start folded
	got := lines()
	i := len(got) - 1
	for i >= 0 && !strings.HasPrefix(got[i], "┌─ Events") {
		i--
	}
	if i < 0 || strings.Contains(strings.Join(got, "\n"), "TIME") || !strings.Contains(got[i+1], "handback") {
		t.Fatalf("open events = %q; want a box, newest first, no column names", got[i:])
	}
	keys := got[len(got)-2:]
	if !strings.Contains(keys[0], "enter open/close") || !strings.Contains(keys[1], "x kill") || !strings.HasPrefix(got[len(got)-3], "────") {
		t.Fatalf("key lines = %q; want a rule, then move and look, then act and toggle", got[len(got)-3:])
	}
	// The two key lines are one grid: the i-th entries start at the same cell.
	for _, pair := range [][2]string{{"←/→ tab", "M model"}, {"enter open/close", "e events"}, {"esc back", "n notices"}} {
		if a, b := strings.Index(keys[0], pair[0]), strings.Index(keys[1], pair[1]); lipgloss.Width(keys[0][:a]) != lipgloss.Width(keys[1][:b]) {
			t.Fatalf("key lines = %q; want %q above %q", keys, pair[0], pair[1])
		}
	}

	m.Update(fetched{ps: proto.PsResult{}})
	if got = lines(); !strings.Contains(got[len(got)-5], "No events yet.") {
		t.Fatalf("no events = %q; want the empty-state line", got[len(got)-6:])
	}
	m.Update(fetched{ps: proto.PsResult{State: core.State{Events: []core.Event{ev("handback", "a1")}}}})
	m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	got = lines()
	folded := got[len(got)-4]
	if !strings.HasPrefix(folded, " ● Events · ") || !strings.Contains(folded, "handback") || strings.Contains(folded, "─") ||
		strings.Contains(strings.Join(got, "\n"), "┌─ Events") {
		t.Fatalf("collapsed events = %q; want one line of text with the latest event, no rule", folded)
	}
}

// strip removes colour codes from a render.
func strip(s string) string { return regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(s, "") }

// The table's columns never move with the data or with a fold: the header is the same whether a
// team is open or folded, its gone members listed or one row, and someone works or nobody does.
// CWD is a column only while some row (a folded one too) has a directory to show.
func TestTopLayoutDoesNotMove(t *testing.T) {
	mem := func(id, state, cwd string) core.MemberState {
		return core.MemberState{ID: id, Name: id, State: state, ReportsTo: "boss", Headless: true, Harness: "pi", Cwd: cwd}
	}
	view := func(boss, gone string) (*topModel, string) {
		ps := proto.PsResult{State: core.State{
			Solos: []core.SoloState{{ID: "s1", Name: "s1", State: "idle", Harness: "pi", Cwd: "/w/shop"}},
			Teams: []core.TeamState{{ID: "t", Name: "shop", Root: "/w/shop", Members: []core.MemberState{
				{ID: "boss", Name: "boss", State: boss, Harness: "pi", Cwd: "/w/shop"}, mem("w1", "idle", "/w/shop"), mem("w2", "gone", gone)}}}}}
		m := newTopModel(nil, t.TempDir())
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		m.Update(fetched{ps: ps})
		return m, listHeader(m)
	}
	m, idle := view("idle", "/w/shop")
	m.sel = closedRow + "t"
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // fold the team
	folded := listHeader(m)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.sel = goneRow + "t"
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // list its gone members
	if _, working := view("working", "/w/shop"); working != idle || folded != idle || listHeader(m) != idle {
		t.Fatalf("columns moved:\nidle    %q\nworking %q\nfolded  %q\ngone open %q", idle, working, folded, listHeader(m))
	}
	if strings.Contains(idle, "CWD") {
		t.Fatalf("CWD shown with every row in its directory: %q", idle)
	}
	if _, sub := view("idle", "/w/shop/sub"); !strings.Contains(sub, "CWD") { // w2 is folded into the gone row
		t.Fatalf("CWD hidden though a folded row has one: %q", sub)
	}
}

// listHeader is the list's column header as drawn.
func listHeader(m *topModel) string {
	h, _, _ := m.list(100, time.Now())
	return strip(h)
}

// scrollPS is a long list: a small team above a team of thirty, in one directory.
func scrollPS() proto.PsResult {
	mem := func(id, reports string) core.MemberState {
		return core.MemberState{ID: id, Name: id, Role: "dev", State: "idle", ReportsTo: reports, Headless: reports != "", Harness: "pi", Cwd: "/w/shop"}
	}
	big := []core.MemberState{mem("boss", "")}
	for i := 1; i < 30; i++ {
		big = append(big, mem(fmt.Sprintf("w%02d", i), "boss"))
	}
	return proto.PsResult{State: core.State{Teams: []core.TeamState{
		{ID: "a", Name: "aaa", Root: "/w/shop", CreatedAt: 1, Members: []core.MemberState{mem("a1", "")}},
		{ID: "b", Name: "big", Root: "/w/shop", CreatedAt: 2, Members: big}}}}
}

// listFrame is the list box of a 120-wide frame as drawn (colours removed), border to border, and the
// terminal row it starts on.
func listFrame(m *topModel) ([]string, int) {
	var out []string
	first := -1
	for y, l := range strings.Split(strip(m.render()), "\n") {
		r := []rune(l)
		if len(r) < 80 {
			continue
		}
		if first < 0 && r[0] == '┌' {
			first = y
		}
		if first >= 0 {
			out = append(out, string(r[:80]))
			if r[0] == '└' {
				break
			}
		}
	}
	return out, first
}

// A long list scrolls under a header that stays on the list's first line; a fold that shortens the
// list never leaves blank lines under its last row; a click after scrolling selects the row under
// the pointer.
func TestTopScrollsUnderStickyHeader(t *testing.T) {
	m := newTopModel(nil, t.TempDir())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 22})
	m.Update(fetched{ps: scrollPS()})
	body := func() (string, []string) {
		f, _ := listFrame(m)
		return f[1], f[2 : len(f)-1]
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if head, _ := body(); !strings.Contains(head, "NAME") {
		t.Fatalf("after paging the first line is %q; want the column header", head)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	f, first := listFrame(m)
	var y int
	for i, l := range f {
		if strings.Contains(l, "w20") {
			y = first + i
		}
	}
	m.Update(tea.MouseClickMsg{X: 5, Y: y, Button: tea.MouseLeft})
	if m.sel != "w20" {
		t.Fatalf("a click on the w20 row selected %q", m.sel)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	body()
	for range 3 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	body()
	m.toggleRow(closedRow + "a") // a fold above the window shortens the list under it
	if _, rows := body(); strings.Trim(rows[len(rows)-1], "│ ") == "" {
		t.Fatalf("a fold above left a blank last line: %q", rows[len(rows)-1])
	}
}

// What the user folded is remembered by the next top (cache/top.json): a team fold, an expanded
// gone row and an opened Events list; the defaults are not written, and a team that no longer exists
// is dropped.
func TestTopFoldsAreRemembered(t *testing.T) {
	dir := t.TempDir()
	mem := func(id, state, reports string) core.MemberState {
		return core.MemberState{ID: id, Name: id, State: state, ReportsTo: reports}
	}
	ps := proto.PsResult{State: core.State{Teams: []core.TeamState{{ID: "t", Name: "shop", Members: []core.MemberState{
		mem("boss", "idle", ""), mem("w1", "working", "boss"), mem("w2", "gone", "boss")}}}}}
	fresh := func(ps proto.PsResult) *topModel {
		m := newTopModel(nil, dir)
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		m.Update(fetched{ps: ps})
		return m
	}
	items := func(m *topModel) string { return strings.Join(m.items(), " ") }
	if m := fresh(ps); items(m) != strings.Join([]string{closedRow + "t", "boss", "w1", goneRow + "t"}, " ") || m.events {
		t.Fatalf("defaults: items %q, events %v; want the team open, its gone member folded, events folded", items(m), m.events)
	}
	m := fresh(ps)
	m.sel = goneRow + "t"
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // list the gone member
	m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if got := fresh(ps); !strings.Contains(items(got), "w2") || !got.events {
		t.Fatalf("a new top lists %q, events %v; want the gone member listed and events open", items(got), got.events)
	}
	m.sel = closedRow + "t"
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // fold the team
	if got := items(fresh(ps)); got != closedRow+"t" {
		t.Fatalf("a new top lists %q; want the team folded", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // and back to the default: nothing kept for it
	m.sel = goneRow + "t"
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if b, _ := os.ReadFile(topStatePath(dir)); strings.Contains(string(b), "true") || strings.Contains(string(b), "false") || strings.Contains(string(b), "events") {
		t.Fatalf("back at the defaults, saved %s; want nothing kept", b)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	other := proto.PsResult{State: core.State{Teams: []core.TeamState{{ID: "u", Name: "other", Members: []core.MemberState{mem("x", "idle", "")}}}}}
	m.Update(fetched{ps: other})
	m.sel = closedRow + "u"
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s := loadTopState(dir); len(s.Teams) != 1 || len(s.Gone) != 0 {
		t.Fatalf("saved %+v; want only the team that still exists", s)
	}
}

// Choosing in the model picker applies the model and level chosen to that worker, through the verb
// `model`, and only what changed. The picker opens from a click on a live headless worker's model row,
// or from M with the mouse off; a double-click on a model applies it like Enter.
func TestModelPickerAppliesTheChoice(t *testing.T) {
	w := core.MemberState{ID: "w1", Name: "w1", State: "working", ReportsTo: "boss", Headless: true, Harness: "pi", Model: "HP/glm-5.3-flash", Thinking: "high"}
	boss := core.MemberState{ID: "boss", Name: "boss", State: "idle", Harness: "pi"}
	m := newTopModel(nil, t.TempDir())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(fetched{ps: proto.PsResult{State: core.State{Teams: []core.TeamState{{ID: "t", Name: "shop", Members: []core.MemberState{boss, w}}}}}})
	var applied []core.ModelArgs
	m.listModels = func(string) ([]string, error) { return []string{"HP/glm-5.3-flash", "HP/kimi-k3", "OAI/gpt-5"}, nil }
	m.setModel = func(a core.ModelArgs) error { applied = append(applied, a); return nil }
	run := func(cmd tea.Cmd) { // deliver a command's message, as the program would
		if cmd != nil {
			m.Update(cmd())
		}
	}
	m.sel = "boss" // an interactive session's model row is plain
	m.render()
	if _, ok := hitFor(m, "w1"); ok {
		t.Fatal("the model row of a session is a target")
	}
	m.sel = "w1"
	m.render()
	h, ok := hitFor(m, "w1")
	if !ok {
		t.Fatal("the model row of a live headless worker is not a target")
	}
	_, cmd := m.Update(tea.MouseClickMsg{X: h.x0, Y: h.y, Button: tea.MouseLeft})
	run(cmd) // the list arrives
	m.render()
	for _, k := range []tea.KeyPressMsg{{Text: "k"}, {Text: "\x1b[<0;36;16m"}, {Text: "i"}, {Code: tea.KeyRight}, {Code: tea.KeyRight}} { // filter to kimi (a mouse report is not typing), high -> max
		m.Update(k)
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	run(cmd)
	if len(applied) != 1 || applied[0].Target != "w1" || applied[0].Model != "HP/kimi-k3" || applied[0].Thinking != "max" || m.pick != nil {
		t.Fatalf("applied %+v, picker open %v; want w1 to HP/kimi-k3 at max, picker closed", applied, m.pick != nil)
	}

	m.Update(tea.KeyPressMsg{Text: "m"}) // mouse off: M still opens it
	_, cmd = m.Update(tea.KeyPressMsg{Text: "M"})
	run(cmd)
	m.render()
	var row hit
	for _, h := range m.hits {
		if h.opt == "m:1" {
			row = h
		}
	}
	m.Update(tea.MouseClickMsg{X: row.x0 + 3, Y: row.y, Button: tea.MouseLeft})
	if len(applied) != 1 {
		t.Fatal("a single click applied")
	}
	_, cmd = m.Update(tea.MouseClickMsg{X: row.x0 + 3, Y: row.y, Button: tea.MouseLeft})
	run(cmd)
	if len(applied) != 2 || applied[1].Model != "HP/kimi-k3" {
		t.Fatalf("applied %+v; want the second click on a model to apply it", applied)
	}
}

// hitFor is the Overview model-row target of worker id in the last frame.
func hitFor(m *topModel, id string) (hit, bool) {
	for _, h := range m.hits {
		if h.pick == id {
			return h, true
		}
	}
	return hit{}, false
}

// The tab bar always shows All and the selected tab; the others that do not fit are the "‹ +N" and
// "+N ›" ends, and the shown ones slide one tab at a time as the selection moves along.
func TestTabSpan(t *testing.T) {
	w := []int{5, 8, 8, 8, 8, 8, 8} // All and six projects, each 8 cells with its gap of 3
	lo, hi := 1, 0
	var seen []string
	for sel := 0; sel < len(w); sel++ {
		lo, hi = tabSpan(w, sel, lo, 5+2*(3+8)+2*(3+5)) // All, two projects and both ends
		seen = append(seen, fmt.Sprintf("%d:%d-%d", sel, lo, hi))
	}
	if got, want := strings.Join(seen, " "), "0:1-2 1:1-2 2:1-2 3:2-3 4:3-4 5:4-5 6:5-6"; got != want {
		t.Fatalf("spans %q, want %q", got, want)
	}
}
