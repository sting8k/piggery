package cli

import (
	"encoding/json"
	"errors"
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
	"github.com/sting8k/piggery/internal/view"
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
	tm.kill = func(id string) error { // its own connection: the model's is busy with the next fetch
		kc, err := e.connect()
		if err != nil {
			return err
		}
		defer kc.Close()
		_, err = kc.CallInto(proto.VerbKill, core.AdminTarget{Target: id}, &core.AgentResult{})
		return err
	}
	own := func(f func(c *Client) error) error { // its own connection: the model's is busy with the next fetch
		kc, err := e.connect()
		if err != nil {
			return err
		}
		defer kc.Close()
		return f(kc)
	}
	tm.listModels = func(id string) (models []string, err error) {
		var r core.ModelsResult
		err = own(func(c *Client) error {
			_, err := c.CallInto(proto.VerbModels, core.AdminTarget{Target: id}, &r)
			return err
		})
		return r.Models, err
	}
	tm.setModel = func(a core.ModelArgs) error {
		return own(func(c *Client) error { _, err := c.CallInto(proto.VerbModel, a, &core.ModelResult{}); return err })
	}
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
	loaded  bool     // a snapshot has arrived
	tab     string   // "" All, a project's directory, or tabClosed; a tab that went away falls back to All
	tabLo   int      // the first tab the bar shows after All (it slides to keep the selected tab in view)
	sel     string   // selected member or solo (participant id), "" when the tab has none
	side    bool     // the sidebar is shown beside the list (wide terminal)
	full    bool     // the sidebar is shown instead of the list (narrow terminal)
	sideTab int      // sideOverview or sideTail
	events  bool     // the events strip is shown
	notices bool     // the notices box is shown (else its newest, on one line)
	mouse   bool     // clicks and the wheel are captured (off: select text)
	fold    topState // the teams the user opened or folded and the gone members expanded, remembered (topstate.go)
	hits    []hit    // the clickable spans of the last frame
	scroll  int      // the list's first drawn body line (below the sticky header); moves when the selection would leave the body
	page    int      // the list's body height in the last frame, for PgUp/PgDn
	tail    tailState
	keys    topKeys
	help    help.Model
	w, h    int
	err     error // a daemon error ends top with it

	kill     func(id string) error // kills a worker (the same verb as `piggery kill`, on a connection of its own)
	killing  killAsk               // the worker `x` asked about, until y or another key
	killNote string                // the answer to the last `x`, until the next key

	pick       *modelPicker                      // the open model picker (toppicker.go)
	hover      string                            // the worker whose Overview model row the mouse is on
	modelRow   int                               // the Overview line of the model row that opens the picker, -1 when there is none (set while drawing)
	modelOf    string                            // ... and its worker
	modelW     int                               // ... and the width of its value
	listModels func(id string) ([]string, error) // asks the daemon for the models a worker's harness accepts, on a connection of its own
	setModel   func(core.ModelArgs) error        // applies a model and thinking level (the verb `model`), on a connection of its own
	still      bool                              // the last message changed nothing drawn (the mouse moved within one target): View keeps the last frame
	frame      string                            // the last frame drawn
}

// killAsk is the worker `x` is asking to kill.
type killAsk struct{ id, name string }

// killed is the result of a kill.
type killed struct {
	name string
	err  error
}

const (
	tabClosed    = view.TabClosed // not a team id
	closedRow    = view.TeamRow   // + team id: the selectable line of a team (a live one, or listed as one line)
	goneRow      = view.GoneRow   // + team id: the selectable line of a team's folded gone members
	sideOverview = 0
	sideTail     = 1
)

func newTopModel(c *Client, dir string) *topModel {
	h := help.New()
	h.Styles = helpStyles()
	h.ShortSeparator = "   "
	// Its first read starts from what ps --json last read (logcache.go), not from the start of each log.
	fold := loadTopState(dir)
	return &topModel{c: c, dir: dir, modelRow: -1, side: true, events: fold.Events, notices: fold.Notices, mouse: true, fold: fold, keys: newTopKeys(), help: h,
		cols: server.DisplayColumns, logs: loadLogCache(dir)}
}

// topKeys are top's keys; the footer shows the short list, ? the full one.
type topKeys struct {
	Next, Prev, Up, Down, Preview, Pane, Close, Events, Notices, Mouse, Kill, Help, Quit key.Binding
	PgUp, PgDown, Home, End                                                              key.Binding // ? only
	Model                                                                                key.Binding
	mouseOn                                                                              bool // what the m entry says
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
		Preview: b([]string{"enter"}, "enter", "open/close"),
		Pane:    b([]string{"t"}, "t", "overview/tail"),
		Close:   b([]string{"esc"}, "esc", "back"),
		Events:  b([]string{"e"}, "e", "events"),
		Notices: b([]string{"n"}, "n", "notices"),
		Mouse:   b([]string{"m"}, "m", "mouse"),
		Kill:    b([]string{"x"}, "x", "kill worker"),
		Model:   b([]string{"M"}, "M", "model"),
		Help:    b([]string{"?"}, "?", "help"),
		Quit:    b([]string{"q", "ctrl+c"}, "q", "quit"),
		mouseOn: true,
		PgUp:    b([]string{"pgup", "ctrl+b"}, "pgup/pgdn", "page"),
		PgDown:  b([]string{"pgdown", "ctrl+f"}, "pgup/pgdn", "page"),
		Home:    b([]string{"home", "g"}, "home/end", "first/last"),
		End:     b([]string{"end", "G"}, "home/end", "first/last"),
	}
}

func (k topKeys) ShortHelp() []key.Binding { return append(k.look(), k.act()...) }

// look is the footer's first line: move and look. act is the second: act and toggle, with the short
// names of the keys whose full list entry is longer.
func (k topKeys) look() []key.Binding {
	return []key.Binding{k.Down, k.Next, k.Preview, k.Close, k.Pane}
}

func (k topKeys) act() []key.Binding {
	// it says its state, in a cell as wide for "on" as for "off", so the grid does not shift
	mouse := key.NewBinding(key.WithKeys("m"), key.WithHelp("m", fmt.Sprintf("%-9s", "mouse "+onOff(k.mouseOn))))
	all := key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "all keys"))
	kill := key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "kill"))
	return []key.Binding{kill, k.Model, k.Events, k.Notices, mouse, all, k.Quit} // M next to x: the footer drops entries from the end, and these two are the actions on a worker
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func (k topKeys) FullHelp() [][]key.Binding {
	k.Mouse.SetHelp("m", "mouse "+onOff(k.mouseOn)+" (off: select text)")
	return [][]key.Binding{{k.Next, k.Prev}, {k.Down, k.PgUp, k.Home}, {k.Preview, k.Pane, k.Close}, {k.Events, k.Notices, k.Mouse, k.Kill, k.Model, k.Help, k.Quit}}
}

// tailState follows one log incrementally: a worker's run log or a session's transcript.
type tailState struct {
	worker, path string
	off          int64
	lines        []string
	reader       transcriptReader // path's reader, kept with off
}

// logState is what was read of one log (a worker's run log or a session's transcript): up to off,
// its turns, and its latest context (not a sum). ps --json keeps it between calls in the cache
// (logcache.go); top keeps it in memory.
type logState struct {
	off    int64
	turns  int
	tokens int
	seen   bool             // a record with the context was read
	mark   string           // mark() of the bytes before off: a rewritten file is read again
	reader transcriptReader // the file's reader, kept so its state follows off
	state  json.RawMessage  // a resumable reader's saved state, until reader is made from it
}

type fetched struct {
	ps   proto.PsResult
	tail tailState
	logs map[string]logState
	err  error
}

type tick struct{}

func (m *topModel) Init() tea.Cmd { return m.fetch() }

// fetch reads the snapshot and, when on, the selected participant's new log lines. One fetch is in
// flight at a time (the next is scheduled when it lands), so the client is not shared.
func (m *topModel) fetch() tea.Cmd {
	sel, on, tail, logs := m.sel, m.sideShown() && m.sideTab == sideTail && m.tailable(m.sel), m.tail, m.logs
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
		path, newReader, err := tailSource(r)
		if err != nil {
			f.tail = tailState{worker: sel, lines: []string{"(" + err.Error() + ")"}}
			return f
		}
		if tail.worker != sel || tail.path != path {
			// A new log: its last lines, read back from its end (not the whole file).
			lines, off, reader, err := lastLines(path, newReader, tailKeep, false)
			if err != nil {
				f.err = err
				return f
			}
			f.tail = tailState{worker: sel, path: path, off: off, lines: lines, reader: reader}
			return f
		}
		recs, off, err := readRecords(tail.path, tail.off)
		if err != nil {
			f.err = err
			return f
		}
		tail.off = off
		tail.lines = append([]string(nil), tail.lines...)
		for _, rec := range recs {
			if l := tail.reader.read(rec).line; l != "" {
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
	m.still = false
	switch msg := msg.(type) {
	case tea.MouseMotionMsg: // hover: only a change of target draws again
		h, ok := m.at(msg.X, msg.Y)
		hover := ""
		if ok && m.pick == nil {
			hover = h.pick
		}
		m.still = hover == m.hover
		m.hover = hover
	case modelsLoaded:
		if m.pick != nil && m.pick.id == msg.id {
			m.pick.loaded(msg)
		}
	case modelApplied:
		if p := m.pick; p != nil && p.id == msg.id {
			p.busy = false
			if msg.err != nil {
				p.note = msg.err.Error()
			} else {
				m.pick = nil
			}
		}
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case killed:
		m.killNote = "killed " + msg.name
		if msg.err != nil {
			m.killNote = "kill " + msg.name + ": " + msg.err.Error()
		}
	case tea.KeyPressMsg:
		if m.pick != nil {
			return m, m.pickKey(msg)
		}
		k := m.keys
		if ask := m.killing; ask.id != "" { // asked once: y kills, any other key cancels
			m.killing = killAsk{}
			if msg.String() == "y" {
				return m, m.killCmd(ask)
			}
			m.killNote = ""
			return m, nil
		}
		m.killNote = ""
		switch {
		case key.Matches(msg, k.Kill):
			m.askKill()
		case key.Matches(msg, k.Model): // the picker without the mouse, for the selected worker when its model can change
			if mem, team := m.selMember(); mem != nil && view.Pickable(*mem, team.ID, m.ps.Closed) {
				return m, m.openPicker(*mem)
			}
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
		case key.Matches(msg, k.PgUp):
			m.move(-max(m.page, 1))
		case key.Matches(msg, k.PgDown):
			m.move(max(m.page, 1))
		case key.Matches(msg, k.Home):
			m.move(-len(m.items()))
		case key.Matches(msg, k.End):
			m.move(len(m.items()))
		case key.Matches(msg, k.Preview):
			if !m.toggleRow(m.sel) {
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
			m.fold.Events = m.events // remembered when opened: the default is folded
			saveTopState(m.dir, m.fold, teamIDs(m.ps))
		case key.Matches(msg, k.Notices):
			m.notices = !m.notices
			m.fold.Notices = m.notices // remembered when opened, like the events
			saveTopState(m.dir, m.fold, teamIDs(m.ps))
		case key.Matches(msg, k.Mouse):
			m.mouse = !m.mouse
			m.keys.mouseOn = m.mouse
		case key.Matches(msg, k.Help):
			m.help.ShowAll = !m.help.ShowAll
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			if m.pick != nil {
				return m, m.pickClick(msg.X, msg.Y)
			}
			return m, m.click(msg.X, msg.Y)
		}
	case tea.MouseWheelMsg:
		if m.pick != nil {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.pick.move(-1)
			case tea.MouseWheelDown:
				m.pick.move(1)
			}
			return m, nil
		}
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

// readLogs brings every run log of each headless member, and each session's transcript, up to
// date, reading only what each file gained since the last call (prev, by path; not modified). A
// worker's logs are one file per run (driver/local.LogPath), so turns add up across resumes; a
// session's are its transcript's.
func readLogs(dir string, ps proto.PsResult, prev map[string]logState) map[string]logState {
	out := map[string]logState{}
	session := func(t *core.Transcript) {
		if newReader, ok := sessionReader(t); ok {
			if st, ok := advanceLog(t.Path, newReader, prev[t.Path]); ok {
				out[t.Path] = st
			}
		}
	}
	for _, s := range ps.Solos {
		session(s.Transcript)
	}
	for _, t := range teamsOf(ps) {
		for _, mem := range t.Members {
			if !view.Logged(mem) {
				session(mem.Transcript)
				continue
			}
			paths, _ := filepath.Glob(filepath.Join(filepath.Dir(local.LogPath(dir, mem.ID, "run")), "*.jsonl"))
			for _, path := range paths {
				if st, ok := advanceLog(path, formats[driverLog], prev[path]); ok {
					out[path] = st
				}
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

// stats is each headless worker's context (current run) and turns (all runs), and each session's
// from its transcript; none for a participant with no log read.
func (m *topModel) stats() map[string]view.Stats {
	out := map[string]view.Stats{}
	session := func(id string, t *core.Transcript) {
		if _, ok := sessionReader(t); !ok {
			return
		}
		if st, ok := m.logs[t.Path]; ok {
			out[id] = view.Stats{Ctx: st.tokens, HasCtx: st.seen, Turns: st.turns}
		}
	}
	for _, s := range m.ps.Solos {
		session(s.ID, s.Transcript)
	}
	for _, t := range teamsOf(m.ps) {
		for _, mem := range t.Members {
			if !view.Logged(mem) {
				session(mem.ID, mem.Transcript)
				continue
			}
			var ws view.Stats
			current := local.LogPath(m.dir, mem.ID, mem.RunID)
			prefix := filepath.Dir(current) + string(filepath.Separator)
			for path, st := range m.logs {
				if !strings.HasPrefix(path, prefix) {
					continue
				}
				ws.Turns += st.turns
				if path == current && st.seen {
					ws.Ctx, ws.HasCtx = st.tokens, true
				}
			}
			out[mem.ID] = ws
		}
	}
	return out
}

// tabs are All, then one per project, then Closed last (view.ProjectTabs).
func (m *topModel) tabs() []view.Tab { return view.ProjectTabs(m.ps.State, time.Now()) }

// toggleRow opens or closes what the row id stands for (a team's line, or its gone members' line)
// and remembers it; false when id is neither.
func (m *topModel) toggleRow(id string) bool {
	if team, ok := strings.CutPrefix(id, closedRow); ok {
		def := view.OpenByDefault(m.ps.State, team)
		if open := !view.IsOpen(m.fold.Teams, team, def); open == def {
			delete(m.fold.Teams, team) // only a choice that differs from the default is remembered
		} else {
			m.fold.Teams[team] = open
		}
	} else if team, ok := strings.CutPrefix(id, goneRow); ok {
		if m.fold.Gone[team] {
			delete(m.fold.Gone, team)
		} else {
			m.fold.Gone[team] = true
		}
	} else {
		return false
	}
	saveTopState(m.dir, m.fold, teamIDs(m.ps))
	return true
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
		if t.Key == m.tab {
			i = j
		}
	}
	m.tab = tabs[(i+d+len(tabs))%len(tabs)].Key
	m.keepSel()
}

// listOf is what the current tab lists (view.BuildList, All only what is alive): the folds are the user's (m.fold), stats
// the logs read so far (nil where only the ids are wanted).
func (m *topModel) listOf(stats map[string]view.Stats, now time.Time) view.List {
	return view.BuildList(view.ListInput{State: m.ps.State, Tab: m.tab, Now: now, Open: m.fold.Teams, Gone: m.fold.Gone, Stats: stats, Projects: true})
}

// items are the selectable ids of the current tab, in display order (list).
func (m *topModel) items() []string { return m.listOf(nil, time.Now()).Items }

// keepSel keeps the tab and selection valid after a refresh or a tab switch: a tab that went
// away falls back to All; a selection not in the tab moves to its first row.
func (m *topModel) keepSel() {
	found := false
	for _, t := range m.tabs() {
		found = found || t.Key == m.tab
	}
	if !found {
		m.tab = ""
	}
	ids := m.items()
	if slices.Contains(ids, m.sel) {
		return
	}
	m.sel, m.tail = "", tailState{}
	if len(ids) > 0 { // the first member or solo (its details show at once), else the first line
		m.sel = ids[max(slices.IndexFunc(ids, func(id string) bool { return !isLine(id) }), 0)]
	}
}

// isLine: id is a team's line or its gone members' line, not a member or a solo.
func isLine(id string) bool {
	return strings.HasPrefix(id, closedRow) || strings.HasPrefix(id, goneRow)
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
func (m *topModel) click(x, y int) tea.Cmd {
	h, ok := m.at(x, y)
	switch {
	case !ok:
	case h.pick != "":
		if mem, _ := m.selMember(); mem != nil && mem.ID == h.pick {
			return m.openPicker(*mem)
		}
	case h.side >= 0:
		m.side, m.sideTab = true, h.side
	case strings.HasPrefix(h.id, closedRow) || strings.HasPrefix(h.id, goneRow):
		m.sel = h.id
		m.toggleRow(h.id)
	case h.id != "" && h.id == m.sel:
		m.toggleSide()
	case h.id != "":
		m.sel, m.tail = h.id, tailState{}
	case !h.list:
		m.tab = h.tab
		m.keepSel()
	}
	return nil
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

// tailable reports whether id has a log to tail: a driver log (capability usage), or a
// transcript piggery has a reader for.
func (m *topModel) tailable(id string) bool {
	for _, s := range m.ps.Solos {
		if s.ID == id {
			return view.SoloTailable(s, hasReader)
		}
	}
	for _, t := range teamsOf(m.ps) {
		for _, mem := range t.Members {
			if mem.ID == id {
				return view.MemberTailable(mem, hasReader)
			}
		}
	}
	return false
}

// hasReader reports whether piggery has a reader for a session's transcript.
func hasReader(t *core.Transcript) bool {
	_, ok := sessionReader(t)
	return ok
}

func (m *topModel) View() tea.View {
	if !m.still || m.frame == "" {
		m.frame = m.render()
	}
	v := tea.NewView(m.frame)
	v.AltScreen = true
	if m.mouse {
		v.MouseMode = tea.MouseModeAllMotion // hover needs motion without a button
	}
	return v
}

// askKill is `x`: ask to kill the selected worker, or say why it cannot be.
func (m *topModel) askKill() {
	for _, t := range teamsOf(m.ps) {
		for _, mem := range t.Members {
			if mem.ID != m.sel {
				continue
			}
			if m.killNote = view.KillNote(mem.Name, mem.Headless, mem.State); m.killNote == "" {
				m.killing = killAsk{id: mem.ID, name: mem.Name}
			}
			return
		}
	}
	name := "this row"
	for _, s := range m.ps.Solos {
		if s.ID == m.sel {
			name = s.Name
		}
	}
	m.killNote = view.KillNote(name, false, "")
}

// killCmd kills ask's worker in the background and reports the result as a killed message.
func (m *topModel) killCmd(ask killAsk) tea.Cmd {
	kill := m.kill
	return func() tea.Msg {
		if kill == nil {
			return killed{ask.name, errors.New("no connection")}
		}
		return killed{ask.name, kill(ask.id)}
	}
}
