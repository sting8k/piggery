package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
)

// top is the live view: the ps rows refreshed every second, the latest
// events, and the tail of a selected worker. View only. Not a terminal: prints once like ps.
func (e *env) top(args []string) error {
	pos, err := parse(e.flags("top"), args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%w: top takes no arguments", errUsage)
	}
	f, ok := e.stdout.(*os.File)
	if !ok || e.json || !term.IsTerminal(f.Fd()) {
		return e.ps(nil)
	}
	c, err := e.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	cols, err := e.columns()
	if err != nil {
		return err
	}
	usePalette(lipgloss.HasDarkBackground(os.Stdin, f)) // as fang decides for --help
	tm := newTopModel(c, e.dir)
	tm.cols = cols
	m, err := tea.NewProgram(tm, tea.WithOutput(f)).Run()
	if err != nil {
		return err
	}
	return m.(*topModel).err
}

const (
	topEvery     = time.Second
	topEvents    = 8
	tailKeep     = 200 // readable tail lines kept for the selected worker
	topTailLines = 20  // the latest tail lines shown (fewer when the window has less room)
)

type topModel struct {
	cols    []string // display.columns: what the lists show after the name
	c       *Client
	dir     string              // the daemon's dir: worker logs are read here (driver/local.LogPath)
	logs    map[string]logState // worker run logs read so far, by path
	ps      proto.PsResult
	loaded  bool            // a snapshot has arrived
	tab     string          // "" All, a team id, or tabClosed; a tab that went away falls back to All
	sel     string          // selected member or solo (participant id), "" when the tab has none
	side    bool            // the sidebar is shown beside the list (wide terminal)
	full    bool            // the sidebar is shown instead of the list (narrow terminal)
	sideTab int             // sideOverview or sideTail
	events  bool            // the events strip is shown
	mouse   bool            // clicks and the wheel are captured (off: the terminal selects text)
	open    map[string]bool // closed teams expanded, by team id
	hits    []hit           // the clickable spans of the last frame
	tail    tailState
	keys    topKeys
	help    help.Model
	w, h    int
	err     error // a daemon error ends top with it
}

const (
	tabClosed    = "\x00closed" // not a team id
	closedRow    = "\x00team:"  // + team id: the selectable line of a closed team
	closedRecent = time.Hour    // All shows teams closed this recently
	sideOverview = 0
	sideTail     = 1
)

func newTopModel(c *Client, dir string) *topModel {
	h := help.New()
	h.Styles = helpStyles()
	return &topModel{c: c, dir: dir, side: true, events: true, mouse: true, open: map[string]bool{}, keys: newTopKeys(), help: h,
		cols: server.DisplayColumns}
}

// topKeys are top's keys; the footer shows the short list, ? the full one.
type topKeys struct {
	Next, Prev, Up, Down, Preview, Pane, Close, Events, Mouse, Help, Quit key.Binding
}

func newTopKeys() topKeys {
	b := func(keys []string, k, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(k, desc))
	}
	return topKeys{
		Next:    b([]string{"right", "l", "tab"}, "←/→", "tab"),
		Prev:    b([]string{"left", "h", "shift+tab"}, "tab/⇧tab", "next/previous tab"),
		Up:      b([]string{"up", "k"}, "↑/↓", "select"),
		Down:    b([]string{"down", "j"}, "↑/↓", "select"),
		Preview: b([]string{"enter"}, "enter", "details"),
		Pane:    b([]string{"t"}, "t", "overview/tail"),
		Close:   b([]string{"esc"}, "esc", "back"),
		Events:  b([]string{"e"}, "e", "events"),
		Mouse:   b([]string{"m"}, "m", "mouse/select text"),
		Help:    b([]string{"?"}, "?", "help"),
		Quit:    b([]string{"q", "ctrl+c"}, "q", "quit"),
	}
}

func (k topKeys) ShortHelp() []key.Binding {
	return []key.Binding{k.Help, k.Next, k.Down, k.Preview, k.Close, k.Pane, k.Events, k.Mouse, k.Quit}
}

func (k topKeys) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Next, k.Prev}, {k.Down}, {k.Preview, k.Pane, k.Close}, {k.Events, k.Mouse, k.Help, k.Quit}}
}

// tailState follows one run's log incrementally.
type tailState struct {
	worker, path string
	off          int64
	lines        []string
}

// logState is what top has read of one run log: up to off, its turn_end records, and the
// usage of its latest assistant message_end (the context then, not a sum).
type logState struct {
	off    int64
	turns  int
	tokens int
	seen   bool // an assistant message_end with usage was read
}

// workerStats is a worker's context now (its current run) and turns over its whole life (all
// its runs). Read from the driver's logs on the CLI side: no DB column, no event.
type workerStats struct {
	ctx    int
	hasCtx bool
	turns  int
}

type fetched struct {
	ps   proto.PsResult
	tail tailState
	logs map[string]logState
	err  error
}

type tick struct{}

func (m *topModel) Init() tea.Cmd { return m.fetch() }

// fetch reads the snapshot and, when on, the selected worker's new log lines. One fetch is in
// flight at a time (the next is scheduled when it lands), so the client is not shared.
func (m *topModel) fetch() tea.Cmd {
	sel, on, tail, logs := m.sel, m.sideShown() && m.sideTab == sideTail && m.headless(m.sel), m.tail, m.logs
	return func() tea.Msg {
		var f fetched
		if _, f.err = m.c.CallInto(proto.VerbPs, core.StateArgs{Events: topEvents}, &f.ps); f.err != nil {
			return f
		}
		f.logs = readLogs(m.dir, f.ps, logs)
		if !on || sel == "" {
			return f
		}
		var r proto.TailResult
		if _, err := m.c.CallInto(proto.VerbTail, core.WorkerLogArgs{Worker: sel}, &r); err != nil {
			f.tail = tailState{worker: sel, lines: []string{"(" + err.Error() + ")"}}
			return f
		}
		if tail.worker != sel || tail.path != r.Path {
			tail = tailState{worker: sel, path: r.Path}
		}
		recs, off, err := readRecords(tail.path, tail.off)
		if err != nil {
			f.err = err
			return f
		}
		tail.off = off
		tail.lines = append([]string(nil), tail.lines...)
		for _, rec := range recs {
			if l, ok := tailLine(rec); ok {
				tail.lines = append(tail.lines, l)
			}
		}
		if len(tail.lines) > tailKeep {
			tail.lines = tail.lines[len(tail.lines)-tailKeep:]
		}
		f.tail = tail
		return f
	}
}

func (m *topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.KeyPressMsg:
		k := m.keys
		switch {
		case key.Matches(msg, k.Quit):
			return m, tea.Quit
		case key.Matches(msg, k.Next):
			m.switchTab(1)
		case key.Matches(msg, k.Prev):
			m.switchTab(-1)
		case key.Matches(msg, k.Up):
			m.move(-1)
		case key.Matches(msg, k.Down):
			m.move(1)
		case key.Matches(msg, k.Preview):
			if id, ok := strings.CutPrefix(m.sel, closedRow); ok {
				m.open[id] = !m.open[id]
			} else {
				m.toggleSide()
			}
		case key.Matches(msg, k.Pane):
			m.side, m.sideTab = true, 1-m.sideTab // the tail comes with the next fetch
		case key.Matches(msg, k.Close):
			if m.narrow() {
				m.full = false // back to the list
			} else {
				m.side = false
			}
			m.help.ShowAll = false
		case key.Matches(msg, k.Events):
			m.events = !m.events
		case key.Matches(msg, k.Mouse):
			m.mouse = !m.mouse
		case key.Matches(msg, k.Help):
			m.help.ShowAll = !m.help.ShowAll
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			m.click(msg.X, msg.Y)
		}
	case tea.MouseWheelMsg:
		if h, ok := m.at(msg.X, msg.Y); ok && h.list {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.move(-1)
			case tea.MouseWheelDown:
				m.move(1)
			}
		}
	case fetched:
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
		m.ps, m.logs, m.loaded = msg.ps, msg.logs, true
		if msg.tail.worker == m.sel {
			m.tail = msg.tail
		}
		m.keepSel()
		return m, tea.Tick(topEvery, func(time.Time) tea.Msg { return tick{} })
	case tick:
		return m, m.fetch()
	}
	return m, nil
}

// readLogs brings every run log of each headless member up to date, reading only what each
// file gained since the last call (prev, by path; not modified). A member's logs are one file
// per run (driver/local.LogPath), so turns add up across resumes.
func readLogs(dir string, ps proto.PsResult, prev map[string]logState) map[string]logState {
	out := map[string]logState{}
	for _, t := range teamsOf(ps) {
		for _, mem := range t.Members {
			if !logged(mem) {
				continue
			}
			paths, _ := filepath.Glob(filepath.Join(filepath.Dir(local.LogPath(dir, mem.ID, "run")), "*.jsonl"))
			for _, path := range paths {
				st := prev[path]
				recs, off, err := readRecords(path, st.off)
				if err != nil {
					continue
				}
				st.off = off
				for _, rec := range recs {
					if n, ok := contextSize(rec); ok {
						st.tokens, st.seen = n, true
					} else if bytes.Contains(rec, []byte(`"turn_end"`)) && recordType(rec) == "turn_end" {
						st.turns++
					}
				}
				out[path] = st
			}
		}
	}
	return out
}

func recordType(rec []byte) string {
	var r struct{ Type string }
	json.Unmarshal(rec, &r)
	return r.Type
}

// contextSize is the context of an assistant message_end with usage: its totalTokens (input,
// output and cache together), or their sum when totalTokens is missing.
func contextSize(rec []byte) (int, bool) {
	var r struct {
		Type    string
		Message struct {
			Role  string
			Usage *struct {
				Input, Output, CacheRead, CacheWrite, TotalTokens int
			}
		}
	}
	if json.Unmarshal(rec, &r) != nil || r.Type != "message_end" || r.Message.Role != "assistant" || r.Message.Usage == nil {
		return 0, false
	}
	u := r.Message.Usage
	if u.TotalTokens > 0 {
		return u.TotalTokens, true
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite, true
}

// stats is each headless worker's context (current run) and turns (all runs).
func (m *topModel) stats() map[string]workerStats {
	out := map[string]workerStats{}
	for _, t := range teamsOf(m.ps) {
		for _, mem := range t.Members {
			if !logged(mem) {
				continue
			}
			var ws workerStats
			current := local.LogPath(m.dir, mem.ID, mem.RunID)
			prefix := filepath.Dir(current) + string(filepath.Separator)
			for path, st := range m.logs {
				if !strings.HasPrefix(path, prefix) {
					continue
				}
				ws.turns += st.turns
				if path == current && st.seen {
					ws.ctx, ws.hasCtx = st.tokens, true
				}
			}
			out[mem.ID] = ws
		}
	}
	return out
}

// topTab is one tab: All (key ""), a team (its id), or Closed; count is its rows.
type topTab struct {
	key, label string
	count      int
}

// tabs are All, then one per team, then Closed (teams gc has not removed) last; solos are only in
// All, under their directory. With only one group besides All, All is the only one (and no tab bar is drawn).
func (m *topModel) tabs() []topTab {
	all := topTab{label: "All", count: len(m.ps.Solos)}
	for _, t := range m.ps.Teams {
		all.count += len(t.Members)
	}
	now := time.Now()
	for _, c := range m.ps.Closed {
		if recent(c, now) {
			all.count++
		}
	}
	out := []topTab{all}
	for _, t := range m.ps.Teams {
		out = append(out, topTab{key: t.ID, label: t.Name, count: len(t.Members)})
	}
	if len(m.ps.Closed) > 0 {
		out = append(out, topTab{key: tabClosed, label: "Closed", count: len(m.ps.Closed)})
	}
	if len(out) < 3 { // one group besides All: All shows it all, no tab bar
		return out[:1]
	}
	return out
}

// recent reports whether c closed within closedRecent of now (All lists it).
func recent(c core.ClosedTeam, now time.Time) bool {
	return now.Sub(time.UnixMilli(c.ClosedAt)) < closedRecent
}

// closedIn is the closed teams the current tab lists: the recent ones in All, all in Closed.
func (m *topModel) closedIn() []core.ClosedTeam {
	var out []core.ClosedTeam
	now := time.Now()
	for _, c := range m.ps.Closed {
		if m.tab == tabClosed || m.tab == "" && recent(c, now) {
			out = append(out, c)
		}
	}
	return out
}

// teamsOf is every team of the snapshot, open then closed (logs, names, lookups).
func teamsOf(ps proto.PsResult) []core.TeamState {
	out := slices.Clone(ps.Teams)
	for _, c := range ps.Closed {
		out = append(out, c.TeamState)
	}
	return out
}

// switchTab moves to the next/previous tab (wrapping) and keeps the selection in it.
func (m *topModel) switchTab(d int) {
	tabs := m.tabs()
	i := 0
	for j, t := range tabs {
		if t.key == m.tab {
			i = j
		}
	}
	m.tab = tabs[(i+d+len(tabs))%len(tabs)].key
	m.keepSel()
}

// groups is what the current tab lists, by project directory (groupByDir): All every open team,
// the recent closed ones and the solos; a team's tab that team; Closed every closed team.
func (m *topModel) groups() []dirGroup {
	var teams []core.TeamState
	for _, t := range m.ps.Teams {
		if m.tab == "" || m.tab == t.ID {
			teams = append(teams, t)
		}
	}
	var solos []core.SoloState
	if m.tab == "" {
		solos = m.ps.Solos
	}
	var roots []string
	for _, t := range teamsOf(m.ps) {
		roots = append(roots, t.Root)
	}
	return groupByDir(teams, m.closedIn(), solos, roots)
}

// items are the selectable ids of the current tab, in display order (list): per directory and
// unit, a team's members as its tree, a closed team's line (then its members when expanded),
// a solo.
func (m *topModel) items() []string {
	var ids []string
	for _, g := range m.groups() {
		for _, u := range g.units {
			switch {
			case u.solo != nil:
				ids = append(ids, u.solo.ID)
			case u.closed != nil:
				ids = append(ids, closedRow+u.closed.ID)
				if m.open[u.closed.ID] {
					for _, r := range memberTree(u.closed.Members) {
						ids = append(ids, r.m.ID)
					}
				}
			default:
				for _, r := range memberTree(u.team.Members) {
					ids = append(ids, r.m.ID)
				}
			}
		}
	}
	return ids
}

// keepSel keeps the tab and selection valid after a refresh or a tab switch: a tab that went
// away falls back to All; a selection not in the tab moves to its first row.
func (m *topModel) keepSel() {
	found := false
	for _, t := range m.tabs() {
		found = found || t.key == m.tab
	}
	if !found {
		m.tab = ""
	}
	ids := m.items()
	if slices.Contains(ids, m.sel) {
		return
	}
	m.sel, m.tail = "", tailState{}
	if len(ids) > 0 {
		m.sel = ids[0]
	}
}

// move selects the previous/next row of the tab.
func (m *topModel) move(d int) {
	ids := m.items()
	if len(ids) == 0 {
		return
	}
	i := slices.Index(ids, m.sel)
	switch {
	case i < 0:
		i = 0
	default:
		i = min(max(i+d, 0), len(ids)-1)
	}
	if ids[i] != m.sel {
		m.sel, m.tail = ids[i], tailState{}
	}
}

// click acts on what the last frame drew at x, y: a tab switches to it, a row selects it (the
// selected row toggles the sidebar), a sidebar title switches the sidebar to it.
func (m *topModel) click(x, y int) {
	h, ok := m.at(x, y)
	switch {
	case !ok:
	case h.side >= 0:
		m.side, m.sideTab = true, h.side
	case strings.HasPrefix(h.id, closedRow):
		m.sel = h.id
		id := strings.TrimPrefix(h.id, closedRow)
		m.open[id] = !m.open[id]
	case h.id != "" && h.id == m.sel:
		m.toggleSide()
	case h.id != "":
		m.sel, m.tail = h.id, tailState{}
	case !h.list:
		m.tab = h.tab
		m.keepSel()
	}
}

// narrow is a terminal too narrow for the sidebar beside the list: there it replaces the list
// (full) and is off until asked for.
func (m *topModel) narrow() bool { return m.w > 0 && m.w < sideRight }

func (m *topModel) sideShown() bool {
	if m.narrow() {
		return m.full
	}
	return m.side
}

func (m *topModel) toggleSide() {
	if m.narrow() {
		m.full = !m.full
	} else {
		m.side = !m.side
	}
}

// headless reports whether id has a driver log to tail (capability usage).
func (m *topModel) headless(id string) bool {
	for _, t := range teamsOf(m.ps) {
		for _, mem := range t.Members {
			if mem.ID == id {
				return logged(mem)
			}
		}
	}
	return false
}

// logged reports whether mem's harness declares usage: a driver log with context, turns and a
// tail. top reads the capability; a member that declared none (spawned before capabilities)
// is judged as before, by being headless.
func logged(mem core.MemberState) bool {
	if mem.Capabilities == nil {
		return mem.Headless
	}
	return slices.Contains(mem.Capabilities, core.CapUsage)
}

func (m *topModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// names maps participant ids of the snapshot to names.
func (m *topModel) names() map[string]string {
	out := map[string]string{}
	for id, n := range m.ps.Names { // event ids, closed teams included
		out[id] = n
	}
	for _, t := range teamsOf(m.ps) {
		for _, p := range t.Members {
			out[p.ID] = p.Name
		}
	}
	for _, s := range m.ps.Solos {
		out[s.ID] = s.Name
	}
	return out
}
