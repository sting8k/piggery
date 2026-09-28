package cli

import (
	"cmp"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/charmtone"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/server"
)

// The look of top: gh-dash's master-detail
// (status header, tabs, list and sidebar boxes, events strip, key footer with ? for all) in
// Charm's charmtone palette, as fang's help and errors (fang DefaultColorScheme). Rows carry
// icon + coloured text only; pills are for the header and the sidebar. Every width comes from
// fitCols and box.

// The palette, set by usePalette for a light or dark terminal background. Plain text keeps the
// terminal's own foreground.
var (
	colPrimary, colSuccess, colMuted, colWarning, colError, colSubtle, colSurface, colTool color.Color
	colInk                                                                                 color.Color // text on a coloured pill
	colOnMain                                                                              color.Color // text on the primary pill

	stTitle, stMuted, stRule lipgloss.Style
	stPlain                  = lipgloss.NewStyle()
)

func init() { usePalette(true) }

// usePalette sets top's colours for a dark or light background: the one place they are chosen.
// Pairs are (light, dark) as in fang; charmtone names as Crush and fang use them.
func usePalette(dark bool) {
	c := lipgloss.LightDark(dark)
	colPrimary = charmtone.Charple
	colSuccess = c(lipgloss.Color("#0CB37F"), charmtone.Guac) // fang's flag green
	colMuted = charmtone.Squid
	colWarning = c(charmtone.Tang, charmtone.Mustard) // Mustard is unreadable on a light background
	colError = c(charmtone.Sriracha, charmtone.Cherry)
	colSubtle = c(charmtone.Smoke, charmtone.Oyster)
	colSurface = c(charmtone.Ash, charmtone.Charcoal)
	colTool = charmtone.Dolly
	colInk = charmtone.Pepper
	colOnMain = charmtone.Butter
	stTitle = lipgloss.NewStyle().Bold(true).Foreground(colPrimary)
	stMuted = lipgloss.NewStyle().Foreground(colMuted)
	stRule = lipgloss.NewStyle().Foreground(colSubtle)
}

// helpStyles is the key footer in the palette: keys muted, descriptions and separators subtle.
func helpStyles() help.Styles {
	return help.Styles{ShortKey: stMuted, ShortDesc: stRule, ShortSeparator: stRule, Ellipsis: stRule,
		FullKey: stMuted, FullDesc: stRule, FullSeparator: stRule}
}

// stateIcon is a member state as icon + word, coloured by what it asks of the operator.
func stateIcon(state string) (string, color.Color) {
	switch state {
	case "working":
		return "● working", colSuccess
	case "idle":
		return "○ idle", colMuted
	case "requested", "starting":
		return "◌ " + state, colWarning
	case "awaiting_permission":
		return "◐ waiting", colWarning
	case "parked":
		return "⏸ parked", colWarning
	case "gone":
		return "✗ gone", colError
	}
	return state, colMuted
}

// dirLabel is a directory line's text: a path, so it ends in "/".
func dirLabel(p string) string {
	return strings.TrimSuffix(p, "/") + "/"
}

func pill(text string, bg, fg color.Color) string {
	return lipgloss.NewStyle().Background(bg).Foreground(fg).Padding(0, 1).Render(text)
}

// When the list is narrow: CWD and MODEL shrink, then ROLE, MODEL, TURNS and AGE go, then NAME
// shrinks.
var (
	listFit = []namedFit{{"cwd", 8}, {"model", 8}, {"role", 0}, {"model", 0}, {"turns", 0}, {"age", 0}, {"name", 8}}

	eventCols = []string{"TIME", "WHO", "EVENT", "TARGET"}
	eventFit  = []fitStep{{3, 6}, {1, 6}, {2, 8}}
)

const (
	gap       = 2  // cells between columns
	sideMin   = 40 // the sidebar is at least this wide ...
	sideRight = 110
	// ... and goes right of the list from this terminal width, below it under.
)

// fitStep shrinks col to min cells when the row is too wide; min 0 hides the column.
type fitStep struct{ col, min int }

// fitCols sizes each column to its widest cell (header included), then applies steps in order
// until the row fits width. A width of 0 hides a column.
func fitCols(head []string, rows [][]string, width int, steps []fitStep) []int {
	w := make([]int, len(head))
	for c, h := range head {
		w[c] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for c, v := range r {
			w[c] = max(w[c], lipgloss.Width(v))
		}
	}
	total := func() int {
		n := -gap
		for _, x := range w {
			if x > 0 {
				n += x + gap
			}
		}
		return n
	}
	for _, s := range steps {
		over := total() - width
		if over <= 0 {
			break
		}
		if s.min == 0 {
			w[s.col] = 0
		} else {
			w[s.col] = max(s.min, w[s.col]-over)
		}
	}
	return w
}

// row renders cells in widths after a 2-cell selection bar; a selected row is filled to width
// with the surface colour (gh-dash), others are not.
func row(cells []string, widths []int, right map[int]bool, style func(c int) lipgloss.Style, sel bool, width int) string {
	bg := func(s lipgloss.Style) lipgloss.Style {
		if sel {
			return s.Background(colSurface)
		}
		return s
	}
	var b strings.Builder
	bar := "  "
	if sel {
		bar = "▌ "
	}
	b.WriteString(bg(lipgloss.NewStyle().Foreground(colPrimary)).Render(bar))
	n, first := 2, true
	for c, v := range cells {
		if widths[c] == 0 {
			continue
		}
		if !first {
			b.WriteString(bg(stPlain).Render(strings.Repeat(" ", gap)))
			n += gap
		}
		first = false
		v = truncate(v, widths[c])
		fill := strings.Repeat(" ", widths[c]-lipgloss.Width(v))
		if right[c] {
			v = fill + v
		} else {
			v += fill
		}
		b.WriteString(bg(style(c)).Render(v))
		n += widths[c]
	}
	if sel && n < width {
		b.WriteString(bg(stPlain).Render(strings.Repeat(" ", width-n)))
	}
	return b.String()
}

// box draws lines in a w x h border with an optional title in the top edge; lines are cut or
// padded to fit.
func box(title string, lines []string, w, h int) []string {
	inner := max(w-2, 0)
	top := "┌" + strings.Repeat("─", inner) + "┐"
	if title != "" {
		t := lipgloss.NewStyle().MaxWidth(max(inner-2, 0)).Render(" " + title + " ")
		top = stRule.Render("┌─") + t + stRule.Render(strings.Repeat("─", max(inner-1-lipgloss.Width(t), 0))+"┐")
	} else {
		top = stRule.Render(top)
	}
	out := []string{top}
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(lines) {
			l = lipgloss.NewStyle().MaxWidth(inner).Render(lines[i])
		}
		out = append(out, stRule.Render("│")+l+strings.Repeat(" ", max(inner-lipgloss.Width(l), 0))+stRule.Render("│"))
	}
	return append(out, stRule.Render("└"+strings.Repeat("─", inner)+"┘"))
}

func (m *topModel) render() string {
	if !m.loaded {
		return "loading…"
	}
	now := time.Now()
	width, height := m.w, m.h
	if width <= 0 {
		width = 120
	}
	if height <= 0 {
		height = 30
	}
	cut := lipgloss.NewStyle().MaxWidth(width)

	m.hits = m.hits[:0]
	head := []string{cut.Render(m.header(now)), stRule.Render(strings.Repeat("─", width))}
	if tabs := m.tabs(); len(tabs) > 1 {
		head = append(head, cut.Render(m.tabBar(tabs, len(head))))
	}
	var foot []string
	if m.events {
		foot = m.eventLines(width, height, now)
	}
	m.help.SetWidth(width - 2)
	for _, l := range strings.Split(m.help.View(m.keys), "\n") {
		foot = append(foot, " "+l)
	}

	room := max(height-len(head)-len(foot), 6)
	y := len(head)
	var main []string
	switch {
	case !m.sideShown() || m.sel == "": // no sidebar with nothing to show in it
		main = m.listBox(width, room, y, now)
	case !m.narrow():
		sw := max(sideMin, width/3)
		left := m.listBox(width-sw, room, y, now)
		right := m.sideBox(width-sw, y, sw, room, now)
		for i := range left {
			main = append(main, left[i]+right[i])
		}
	default: // narrow: the sidebar instead of the list, until esc
		main = m.sideBox(0, y, width, room, now)
	}
	return strings.Join(append(append(head, main...), foot...), "\n")
}

// hit is a clickable span of one screen row, recorded while drawing: a tab (tab), a row of
// the list (id), a sidebar tab (side >= 0), or the list's body (list: the wheel moves there).
type hit struct {
	y, x0, x1 int
	tab, id   string
	side      int
	list      bool
}

func (m *topModel) at(x, y int) (hit, bool) {
	for _, h := range m.hits {
		if h.y == y && x >= h.x0 && x < h.x1 {
			return h, true
		}
	}
	return hit{}, false
}

// listBox is the list in a w x h box at row y (left edge 0), scrolled to keep the selection
// in view; it records a hit per row.
func (m *topModel) listBox(w, h, y int, now time.Time) []string {
	lines, ids := m.list(w-2, now)
	n, start := h-2, 0
	if len(lines) > n && n > 0 {
		sel := slices.Index(ids, m.sel)
		if sel >= n-1 {
			start = sel - n + 2
		}
		start = min(start, len(lines)-n)
		lines = lines[start : start+n]
	}
	for i := range max(n, 0) {
		hl := hit{y: y + 1 + i, x0: 1, x1: w - 1, side: -1, list: true}
		if start+i < len(ids) {
			hl.id = ids[start+i]
		}
		m.hits = append(m.hits, hl)
	}
	return box("", lines, w, h)
}

// sideBox is the sidebar in a w x h box at column x, row y; its Overview and Tail titles are
// hits.
func (m *topModel) sideBox(x, y, w, h int, now time.Time) []string {
	ox := x + 3 // after "┌─ "
	m.hits = append(m.hits, hit{y: y, x0: ox, x1: ox + len("Overview"), side: sideOverview},
		hit{y: y, x0: ox + len("Overview  "), x1: ox + len("Overview  Tail"), side: sideTail})
	return box(m.sideTitle(), m.sidebar(w-2, h-2, now), w, h)
}

// header is the title pill and the daemon's status: counts muted, held and unacked as pills
// (amber when mail is held).
func (m *topModel) header(now time.Time) string {
	working, idle := 0, 0
	count := func(state string) {
		switch state {
		case "working":
			working++
		case "idle":
			idle++
		}
	}
	for _, t := range m.ps.Teams {
		for _, mem := range t.Members {
			count(mem.State)
		}
	}
	for _, s := range m.ps.Solos {
		count(s.State)
	}
	held := pill(fmt.Sprintf("held %d", m.ps.Held), colSurface, colMuted)
	if m.ps.Held > 0 {
		held = pill(fmt.Sprintf("held %d", m.ps.Held), colWarning, colInk)
	}
	return " " + lipgloss.NewStyle().Bold(true).Render(pill("🐷 piggery", colPrimary, colOnMain)) + " " +
		pill("● daemon "+ago(m.ps.StartedAt, now), colSurface, colSuccess) + " " +
		stMuted.Render(fmt.Sprintf(" %s · %d working · %d idle ", plural(len(m.ps.Teams), "team"), working, idle)) +
		held + " " + pill(fmt.Sprintf("unacked %d", m.ps.Unacked), colSurface, colMuted)
}

func (m *topModel) tabBar(tabs []topTab, y int) string {
	var parts []string
	x := 2
	for _, t := range tabs {
		label := fmt.Sprintf("%s %d", t.label, t.count)
		w := lipgloss.Width(label)
		m.hits = append(m.hits, hit{y: y, x0: x, x1: x + w, tab: t.key, side: -1})
		x += w + 3
		if t.key == m.tab {
			parts = append(parts, stTitle.Underline(true).Render(label))
		} else {
			parts = append(parts, stMuted.Render(label))
		}
	}
	return "  " + strings.Join(parts, "   ")
}

// list is the current tab's lines, by project directory (groupByDir): the directory, then its
// units oldest first (a team's title and its members' tree; a closed team's line, its members
// when expanded; a solo's row), all rows in one table with the same columns. Per line, the id of
// the member, solo or closed team on it ("" for other lines).
func (m *topModel) list(width int, now time.Time) ([]string, []string) {
	stats := m.stats()
	var lines, ids []string
	// line appends l, for id when it is a selectable row.
	line := func(l, id string) {
		lines, ids = append(lines, l), append(ids, id)
	}
	muted := func(s string) string { return "  " + stMuted.Render(s) }

	cols := shown(m.cols, server.DisplayColumns)
	head, right, fit := layout(cols, listFit)
	cellsOf := func(val map[string]string) []string {
		out := make([]string, len(cols))
		for c, name := range cols {
			out[c] = cmp.Or(val[name], "-")
		}
		return out
	}
	groups := m.groups()
	dirs := make([]string, len(groups))
	for i, g := range groups {
		dirs[i] = g.dir
	}
	short := shortPaths(dirs)

	// The rows first (their widths are shared by the whole table), then the lines in order.
	type trow struct {
		cells []string
		id    string
		state string
		dim   bool
	}
	type entry struct {
		text string // a line that is not a row ("" = the row)
		id   string
		row  *trow
	}
	var entries []entry
	var all [][]string
	addRow := func(r trow) {
		entries = append(entries, entry{row: &r})
		all = append(all, r.cells)
	}
	member := func(g dirGroup, tr treeRow, closed bool) {
		mem := tr.m
		state, _ := stateIcon(mem.State)
		ctx, turns := "-", "-"
		if ws, ok := stats[mem.ID]; ok {
			if ws.hasCtx {
				ctx = tokens(ws.ctx)
			}
			turns = fmt.Sprint(ws.turns)
		}
		cwd := relCwd(g.dir, mem.Cwd)
		addRow(trow{id: mem.ID, state: mem.State, dim: closed || mem.State == "gone" || tr.parentGone, cells: cellsOf(map[string]string{
			"name": tr.prefix + mem.Name, "role": mem.Role, "state": state, "harness": harnessLabel(mem.Harness, mem.Headless),
			"model": modelID(mem.Model), "ctx": ctx, "turns": turns, "unacked": fmt.Sprint(mem.Unacked),
			"age": ago(mem.CreatedAt, now), "since": ago(mem.StateSince, now), "cwd": cmp.Or(cwd, " ")})})
	}
	for gi, g := range groups {
		if gi > 0 {
			entries = append(entries, entry{text: " "})
		}
		entries = append(entries, entry{text: " " + stMuted.Render(dirLabel(short[gi]))})
		for _, u := range g.units {
			switch {
			case u.solo != nil:
				s := u.solo
				state, _ := stateIcon(s.State)
				addRow(trow{id: s.ID, state: s.State, cells: cellsOf(map[string]string{
					"name": "solo " + s.Name, "state": state, "harness": harnessLabel(s.Harness, false),
					"model": modelID(s.Model), "unacked": fmt.Sprint(s.Unacked), "age": ago(s.CreatedAt, now),
					"since": ago(s.StateSince, now), "cwd": cmp.Or(relCwd(g.dir, s.Cwd), " ")})})
			case u.closed != nil:
				c := u.closed
				mark := "▸ "
				if m.open[c.ID] {
					mark = "▾ "
				}
				by := ""
				if c.ClosedBy != "" {
					by = " by " + c.ClosedBy
				}
				text := mark + c.Name + "   closed " + ago(c.ClosedAt, now) + " ago" + by + " · " + plural(len(c.Members), "member")
				entries = append(entries, entry{id: closedRow + c.ID, text: row([]string{text}, []int{width - 3}, nil,
					func(int) lipgloss.Style { return lipgloss.NewStyle().Foreground(colSubtle) }, m.sel == closedRow+c.ID, width)})
				if m.open[c.ID] {
					for _, tr := range memberTree(c.Members) {
						member(g, tr, true)
					}
				}
			default:
				entries = append(entries, entry{text: " " + m.teamTitle(*u.team)})
				if len(u.team.Members) == 0 {
					entries = append(entries, entry{text: muted("No members. Open an agent session in " + home(u.team.Root) + " and ask the gate to admit it.")})
					continue
				}
				for _, tr := range memberTree(u.team.Members) {
					member(g, tr, false)
				}
			}
		}
	}
	widths := fitCols(head, all, width-3, fit)
	if c := slices.Index(cols, "cwd"); c >= 0 { // a path too wide keeps its end
		for _, r := range all {
			r[c] = truncLeft(r[c], widths[c])
		}
	}
	if len(all) > 0 { // one header: the table is shared by every directory
		line(row(head, widths, right, func(int) lipgloss.Style { return stMuted }, false, width), "")
	}
	for _, e := range entries {
		switch {
		case e.row != nil:
			r := e.row
			line(row(r.cells, widths, right, func(c int) lipgloss.Style {
				switch {
				case r.dim:
					return lipgloss.NewStyle().Foreground(colSubtle)
				case cols[c] == "state":
					_, col := stateIcon(r.state)
					return lipgloss.NewStyle().Foreground(col)
				case cols[c] == "name":
					return stPlain
				}
				return stMuted
			}, r.id == m.sel, width), r.id)
		default:
			line(e.text, e.id)
		}
	}
	if len(groups) == 0 {
		switch m.tab {
		case "":
			line(muted("No teams. In pi: “found a team here”"), "")
		case tabClosed:
			line(muted("No closed teams."), "")
		}
	}
	return lines, ids
}

func (m *topModel) teamTitle(t core.TeamState) string {
	// a pill tells it from the directory line above, which is its root
	title := pill("team", colPrimary, colOnMain) + " " + stTitle.Render(t.Name)
	if t.Gate != "" {
		title += stMuted.Render(" · gate " + t.Gate)
	} else {
		title += lipgloss.NewStyle().Foreground(colWarning).Render(" · no gate")
	}
	if t.Held > 0 {
		title += lipgloss.NewStyle().Foreground(colWarning).Render(fmt.Sprintf(" · %d held", t.Held))
	}
	return title
}

func (m *topModel) sideTitle() string {
	tabs := []string{"Overview", "Tail"}
	for i, t := range tabs {
		if i == m.sideTab {
			tabs[i] = stTitle.Underline(true).Render(t)
		} else {
			tabs[i] = stMuted.Render(t)
		}
	}
	return strings.Join(tabs, "  ")
}

// sidebar is the selected row's Overview (every fact top has of it) or its Tail, in w x h.
func (m *topModel) sidebar(w, h int, now time.Time) []string {
	var team *core.TeamState
	var mem *core.MemberState
	var solo *core.SoloState
	teams := make([]*core.TeamState, 0, len(m.ps.Teams)+len(m.ps.Closed))
	for i := range m.ps.Teams {
		teams = append(teams, &m.ps.Teams[i])
	}
	for i := range m.ps.Closed {
		c := &m.ps.Closed[i]
		if m.sel == closedRow+c.ID { // a closed team's line: the team's facts
			kv := func(k, v string) string { return " " + stMuted.Render(fmt.Sprintf("%-9s", k)) + v }
			return []string{" " + lipgloss.NewStyle().Bold(true).Render(c.Name) + stMuted.Render(" · closed"), "",
				kv("closed", ago(c.ClosedAt, now)+" ago"), kv("by", cmp.Or(c.ClosedBy, "admin")), kv("gate", orDash(c.Gate)),
				kv("members", fmt.Sprint(len(c.Members))), kv("root", home(c.Root)),
				" " + stMuted.Render("enter shows its members")}
		}
		teams = append(teams, &c.TeamState)
	}
	for _, t := range teams {
		for j := range t.Members {
			if t.Members[j].ID == m.sel {
				team, mem = t, &t.Members[j]
			}
		}
	}
	for i := range m.ps.Solos {
		if m.ps.Solos[i].ID == m.sel {
			solo = &m.ps.Solos[i]
		}
	}
	if mem == nil && solo == nil {
		return []string{" " + stMuted.Render("Nothing selected.")}
	}

	if m.sideTab == sideTail {
		if mem == nil || !logged(*mem) {
			return []string{" " + stMuted.Render("No tail: its harness keeps no driver log here.")}
		}
		var lines []string
		if m.tail.worker == m.sel { // a fetch in flight may still carry the previous worker's
			lines = m.tail.lines
		}
		if len(lines) == 0 {
			return []string{" " + stMuted.Render("No output yet.")}
		}
		if n := min(h, topTailLines); len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = " " + styleTail(l, w-2)
		}
		return out
	}

	names := m.names()
	kv := func(k, v string) string { return " " + stMuted.Render(fmt.Sprintf("%-9s", k)) + v }
	var name, role, state, ref string
	var since, created int64
	var unacked int
	if mem != nil {
		name, role, state, since, unacked, ref = mem.Name, mem.Role, mem.State, mem.StateSince, mem.Unacked, mem.ID
		created = mem.CreatedAt
	} else {
		name, role, state, since, unacked, ref = solo.Name, "solo", solo.State, solo.StateSince, solo.Unacked, solo.ID
		created = solo.CreatedAt
	}
	came := "joined " + time.UnixMilli(created).Format("15:04") // a session opened by the Human
	if mem != nil && mem.Headless && mem.SpawnedBy != "" {
		came = "spawned " + time.UnixMilli(created).Format("15:04") + " by " + orDash(names[mem.SpawnedBy])
	}
	icon, col := stateIcon(state)
	out := []string{" " + lipgloss.NewStyle().Bold(true).Render(name) + stMuted.Render(" · "+role+" ") + pill(icon, col, colInk), ""}
	if mem != nil {
		kind := "session"
		if mem.Headless {
			kind = "headless worker"
		}
		if mem.Harness != "" {
			kind = mem.Harness + " " + kind
		}
		if mem.Gate {
			kind += " · gate"
		}
		out = append(out, kv("team", team.Name), kv("kind", kind), kv("model", modelLabel(mem.Model, mem.Thinking)))
		if logged(*mem) {
			ws := m.stats()[mem.ID]
			ctx := "-"
			if ws.hasCtx {
				ctx = tokens(ws.ctx)
			}
			out = append(out, kv("ctx", ctx), kv("turns", fmt.Sprint(ws.turns)))
		}
	}
	out = append(out, " "+came, kv("since", ago(since, now)), kv("unacked", fmt.Sprint(unacked)))
	if mem != nil {
		if mem.ReportsTo != "" {
			out = append(out, kv("reports", orDash(names[mem.ReportsTo])))
		}
		if mem.LastTurnEnd > 0 {
			out = append(out, kv("last turn", ago(mem.LastTurnEnd, now)+" ago"))
		}
		out = append(out, kv("root", home(team.Root)))
	} else {
		out = append(out, kv("model", modelLabel(solo.Model, "")), kv("cwd", home(solo.Cwd)))
	}
	return append(out, kv("id", stMuted.Render(ref)))
}

// eventLines is the latest events, newest first (3 in a short window, 5, or 8 in a tall one):
// muted; denied and held amber, exited and gone red. Nothing when there are none.
func (m *topModel) eventLines(width, height int, now time.Time) []string {
	names := m.names()
	name := func(id string) string {
		if n := names[id]; n != "" || len(id) <= 6 {
			return n
		}
		return id[len(id)-6:]
	}
	n := 8
	switch {
	case height < 30:
		n = 3
	case height < 40:
		n = 5
	}
	var rows [][]string
	var types []string
	for i := len(m.ps.Events) - 1; i >= 0 && len(rows) < n; i-- {
		ev := m.ps.Events[i]
		target := ""
		if ev.RefID != "" && ev.RefID != ev.Participant && ev.RefID != ev.TeamID {
			target = name(ev.RefID)
		}
		rows = append(rows, []string{ago(ev.Ts, now), name(ev.Participant), ev.Type, target})
		types = append(types, ev.Type)
	}
	if len(rows) == 0 {
		return nil
	}
	w := fitCols(eventCols, rows, width-2, eventFit)
	out := []string{row(eventCols, w, nil, func(int) lipgloss.Style { return stMuted }, false, width)}
	for i, r := range rows {
		st := stMuted
		switch types[i] {
		case "denied", "held":
			st = lipgloss.NewStyle().Foreground(colWarning)
		case "exited", "gone":
			st = lipgloss.NewStyle().Foreground(colError)
		}
		out = append(out, row(r, w, map[int]bool{0: true}, func(int) lipgloss.Style { return st }, false, width))
	}
	return out
}

// styleTail colors a tail line (tailLine's forms) cut to n cells: tool calls secondary with
// dim arguments, results muted (errors red), problems amber, assistant text plain.
func styleTail(l string, n int) string {
	switch {
	case strings.HasPrefix(l, "> "):
		name, args, _ := strings.Cut(strings.TrimPrefix(l, "> "), " ")
		head := truncate("▸ "+name, n)
		return lipgloss.NewStyle().Foreground(colTool).Render(head) + stMuted.Render(truncate("  "+args, n-lipgloss.Width(head)))
	case strings.HasPrefix(l, "< "):
		head, text, _ := strings.Cut(strings.TrimPrefix(l, "< "), ": ")
		if name, ok := strings.CutSuffix(head, " error"); ok {
			return lipgloss.NewStyle().Foreground(colError).Render(truncate("✗ "+name+"  "+text, n))
		}
		return stMuted.Render(truncate("✓ "+text, n))
	case strings.HasPrefix(l, "! "):
		return lipgloss.NewStyle().Foreground(colWarning).Render(truncate(l, n))
	case strings.HasPrefix(l, "-- "):
		return stRule.Render(truncate(l, n))
	case strings.HasPrefix(l, "user: "):
		return stMuted.Render(truncate(l, n))
	}
	return truncate(strings.TrimPrefix(l, "assistant: "), n)
}

// ago is the time since ms in one unit: 12s, 4m, 3h, 2d; "-" when unknown.
func ago(ms int64, now time.Time) string {
	if ms == 0 {
		return "-"
	}
	d := max(now.Sub(time.UnixMilli(ms)), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// home shortens a path under the home directory to ~/…
func home(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if rel, err := filepath.Rel(h, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}

// truncate cuts plain text to n display cells, with … when cut.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > n-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}
