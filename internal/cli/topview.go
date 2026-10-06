package cli

import (
	"cmp"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/charmtone"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
	"github.com/sting8k/piggery/internal/view"
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
	colLink                                                                                color.Color // blue: what a click acts on (the model value that opens the picker)
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
	colLink = c(charmtone.Damson, charmtone.Malibu)
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

// stateIcon is a member state as icon + word (view.StateText), coloured by what it asks of the operator.
func stateIcon(state string) (string, color.Color) { return view.StateText(state), stateColor(state) }

func stateColor(state string) color.Color {
	switch view.StatusOf(state) {
	case view.StatusWorking:
		return colSuccess
	case view.StatusWaiting:
		return colWarning
	case view.StatusGone:
		return colError
	}
	return colMuted
}

// gateTag follows the name of a team's gate member in its NAME cell, drawn muted by row(); the
// cell's width includes it, so nothing moves.
const gateTag = " (gate)"

func gateTagOf(gate bool) string {
	if gate {
		return gateTag
	}
	return ""
}

// A solo's name cell has a team line's shape: the fold-mark slot, empty (a solo has nothing to
// fold), a pill of the team pill's width in a quiet colour, then the name. Plain text with the
// pill drawn by row(), so widths and truncation see cells only.
const (
	soloSlot = "  "
	soloPill = " solo "
	soloLead = soloSlot + soloPill + " "
)

func pill(text string, bg, fg color.Color) string {
	return lipgloss.NewStyle().Background(bg).Foreground(fg).Padding(0, 1).Render(text)
}

// topColumns are the columns top's table can show: every configurable one but unacked, which top
// gives in its header, the team lines and a member's details; ps text keeps its unacked= field.
var topColumns = slices.DeleteFunc(slices.Clone(server.DisplayColumns), func(c string) bool { return c == "unacked" })

// When the list is narrow: CWD and MODEL shrink, then ROLE, MODEL, TURNS and AGE go, then NAME
// shrinks.
var (
	listFit = []namedFit{{"cwd", 8}, {"model", 8}, {"role", 0}, {"model", 0}, {"turns", 0}, {"age", 0}, {"name", 8}}

	noticeCols = []string{"AGE", "WHERE", "KIND", "BODY"}
	noticeFit  = []fitStep{{3, 20}, {1, 8}, {2, 4}}
	eventCols  = []string{"TIME", "WHO", "EVENT", "TARGET"}
	eventFit   = []fitStep{{3, 6}, {1, 6}, {2, 8}}
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
		tag := ""
		if c == 0 && strings.HasSuffix(v, gateTag) { // the gate's tag stays whole when the name is cut
			v, tag = strings.TrimSuffix(v, gateTag), gateTag
		}
		v = truncate(v, widths[c]-lipgloss.Width(tag))
		fill := strings.Repeat(" ", widths[c]-lipgloss.Width(v)-lipgloss.Width(tag))
		if right[c] {
			v = fill + v
		} else if tag == "" {
			v += fill
		}
		if tag != "" {
			b.WriteString(bg(style(c)).Render(v))
			b.WriteString(bg(stMuted).Render(tag))
			b.WriteString(bg(style(c)).Render(fill))
		} else if c == 0 && strings.HasPrefix(v, soloLead) { // a solo's pill, quiet, where a team has its own
			rest := strings.TrimPrefix(v, soloLead)
			b.WriteString(bg(style(c)).Render(soloSlot))
			b.WriteString(bg(lipgloss.NewStyle().Foreground(colMuted).Background(colSurface)).Render(soloPill))
			b.WriteString(bg(style(c)).Render(" " + rest))
		} else {
			b.WriteString(bg(style(c)).Render(v))
		}
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
	return boxMarked(title, lines, w, h, "", "")
}

// boxMarked is box with a muted note at the right end of the top and of the bottom border ("" for
// none): what is hidden above and below.
func boxMarked(title string, lines []string, w, h int, above, below string) []string {
	inner := max(w-2, 0)
	// edge draws a border of dashes from a left part of left cells to the corner, the note before it
	edge := func(l, r, left string, note string) string {
		room := inner - (lipgloss.Width(l) - 1) - lipgloss.Width(left) // dashes the border has room for
		if note != "" && room >= lipgloss.Width(note)+4 {
			note = " " + note + " "
			return stRule.Render(l) + left + stRule.Render(strings.Repeat("─", room-lipgloss.Width(note)-1)) + stMuted.Render(note) + stRule.Render("─"+r)
		}
		return stRule.Render(l) + left + stRule.Render(strings.Repeat("─", max(room, 0))+r)
	}
	top := edge("┌", "┐", "", above)
	if title != "" {
		t := lipgloss.NewStyle().MaxWidth(max(inner-2, 0)).Render(" " + title + " ")
		top = stRule.Render("┌─") + t + stRule.Render(strings.Repeat("─", max(inner-1-lipgloss.Width(t), 0))+"┐")
		if above != "" {
			top = edge("┌─", "┐", t, above)
		}
	}
	out := []string{top}
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(lines) {
			l = lipgloss.NewStyle().MaxWidth(inner).Render(lines[i])
		}
		out = append(out, stRule.Render("│")+l+strings.Repeat(" ", max(inner-lipgloss.Width(l), 0))+stRule.Render("│"))
	}
	return append(out, edge("└", "┘", "", below))
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
		head = append(head, cut.Render(m.tabBar(tabs, len(head), width)))
	}
	sw, beside := m.sideSplit(width)
	foot := m.band(width, height, sw, beside, now)
	if m.killing.id != "" { // the one question, above the keys
		foot = append(foot, " "+lipgloss.NewStyle().Foreground(colWarning).Bold(true).Render("kill "+m.killing.name+"? y/n"))
	} else if m.killNote != "" {
		foot = append(foot, " "+lipgloss.NewStyle().Foreground(colWarning).Render(truncate(m.killNote, width-2)))
	}
	m.help.SetWidth(width - 2)
	foot = append(foot, stRule.Render(strings.Repeat("─", width))) // mirrors the rule under the header
	for _, l := range m.keyLines(width - 2) {
		foot = append(foot, " "+l)
	}
	notes, mismatch := view.VersionNotes(m.ps.Version, Version)
	foot[len(foot)-1] = withVersion(foot[len(foot)-1], notes, mismatch, width)

	room := max(height-len(head)-len(foot), 6)
	y := len(head)
	var main []string
	switch {
	case beside:
		left := m.listBox(width-sw, room, y, now)
		right := m.sideBox(width-sw, y, sw, room, now)
		for i := range left {
			main = append(main, left[i]+right[i])
		}
	case m.sideShown() && m.sel != "": // narrow: the sidebar instead of the list, until esc
		main = m.sideBox(0, y, width, room, now)
	default: // no sidebar, or nothing to show in it
		main = m.listBox(width, room, y, now)
	}
	frame := append(append(head, main...), foot...)
	if m.pick != nil {
		frame = m.overlay(frame, width)
	}
	return strings.Join(frame, "\n")
}

// hit is a clickable span of one screen row, recorded while drawing: a tab (tab), a row of
// the list (id), a sidebar tab (side >= 0), or the list's body (list: the wheel moves there).
type hit struct {
	y, x0, x1 int
	tab, id   string
	side      int
	list      bool
	pick      string // the Overview's model row of this worker: hover tints it, a click opens the picker
	opt       string // a choice inside the open picker (m:<row>, l:<level>)
}

func (m *topModel) at(x, y int) (hit, bool) {
	for _, h := range m.hits {
		if h.y == y && x >= h.x0 && x < h.x1 {
			return h, true
		}
	}
	return hit{}, false
}

// listBox is the list in a w x h box at row y (left edge 0): the column header on its first line,
// then the body from m.scroll, which moves only when the selection would leave the body (with a
// line of margin). When lines are hidden the borders say how many (↑ N above, ↓ N below). It
// records a hit per drawn line.
func (m *topModel) listBox(w, h, y int, now time.Time) []string {
	head, body, ids := m.list(w-2, now)
	n := max(h-2, 0) // lines in the box
	room := n
	if head != "" {
		room--
	}
	m.page = max(room, 1)
	m.scroll = min(max(m.scroll, 0), max(len(body)-room, 0)) // the list shrank: no blank space under the last line
	if sel := slices.Index(ids, m.sel); sel >= 0 && room > 0 {
		if sel < m.scroll+1 {
			m.scroll = max(sel-1, 0)
		} else if sel > m.scroll+room-2 {
			m.scroll = min(sel-room+2, max(len(body)-room, 0))
		}
	}
	start := m.scroll
	shown, shownIDs := body[min(start, len(body)):min(start+max(room, 0), len(body))], ids[min(start, len(ids)):min(start+max(room, 0), len(ids))]
	above, below := start, max(len(body)-start-len(shown), 0)
	lines, rowIDs := shown, shownIDs
	if head != "" {
		lines, rowIDs = append([]string{head}, shown...), append([]string{""}, shownIDs...)
	}
	for i := range n {
		hl := hit{y: y + 1 + i, x0: 1, x1: w - 1, side: -1, list: true}
		if i < len(rowIDs) {
			hl.id = rowIDs[i]
		}
		m.hits = append(m.hits, hl)
	}
	var up, down string
	if above > 0 {
		up = fmt.Sprintf("↑ %d", above)
	}
	if below > 0 {
		down = fmt.Sprintf("↓ %d", below)
	}
	return boxMarked("", lines, w, h, up, down)
}

// sideBox is the sidebar in a w x h box at column x, row y; its Overview and Tail titles are
// hits.
func (m *topModel) sideBox(x, y, w, h int, now time.Time) []string {
	ox := x + 3 // after "┌─ "
	m.hits = append(m.hits, hit{y: y, x0: ox, x1: ox + len("Overview"), side: sideOverview},
		hit{y: y, x0: ox + len("Overview  "), x1: ox + len("Overview  Tail"), side: sideTail})
	lines := m.sidebar(w-2, h-2, now)
	if m.modelRow >= 0 && m.sideShown() { // the model value, after "│ " and the label
		vx := x + 1 + 1 + overviewLabel
		m.hits = append(m.hits, hit{y: y + 1 + m.modelRow, x0: vx, x1: vx + m.modelW, side: -1, pick: m.modelOf})
	}
	return box(m.sideTitle(), lines, w, h)
}

// header is the title pill and the daemon's status: counts muted, held and unacked as pills
// (amber when mail is held).
func (m *topModel) header(now time.Time) string {
	working, idle := view.Activity(m.ps.State)
	held := pill(fmt.Sprintf("held %d", m.ps.Held), colSurface, colMuted)
	if m.ps.Held > 0 {
		held = pill(fmt.Sprintf("held %d", m.ps.Held), colWarning, colInk)
	}
	head := " " + lipgloss.NewStyle().Bold(true).Render(pill("🐷 piggery", colPrimary, colOnMain)) + " " +
		pill("● daemon "+view.Ago(m.ps.StartedAt, now), colSurface, colSuccess) + " " +
		stMuted.Render(fmt.Sprintf(" %s · %d working · %d idle ", view.Plural(len(m.ps.Teams), "team"), working, idle)) +
		held + " " + pill(fmt.Sprintf("unacked %d", m.ps.Unacked), colSurface, colMuted)
	if n := proto.Notice(m.ps.Outdated); n != "" { // an install only `piggery setup --outdated` brings up to date
		head += " " + pill(n, colWarning, colInk)
	}
	if n := proto.UpdateNotice(m.ps.Update); n != "" { // the daily update check found a newer release
		head += " " + pill(n, colWarning, colInk)
	}
	return head
}

// tabGap separates the tabs of the bar.
const tabGap = 3

// tabSpan is which tabs a bar of avail cells shows, given the cells each tab's label takes: tab 0
// (All) and sel always; of the others, from lo as many as fit, with a "‹ +N" before them when some
// are left out at the start and a "+N ›" after them when some are left out at the end. lo moves
// only to bring sel into view, so the bar slides one tab at a time as ←/→ moves the selection.
// It returns the lo to keep and the last tab shown.
func tabSpan(widths []int, sel, lo, avail int) (newLo, hi int) {
	n := len(widths)
	if n < 2 {
		return 1, 0
	}
	marker := func(k int) int { return tabGap + 3 + len(fmt.Sprint(k)) } // "‹ +N" and "+N ›"
	cost := func(lo, hi int) int {
		c := widths[0]
		for i := lo; i <= hi; i++ {
			c += tabGap + widths[i]
		}
		if lo > 1 {
			c += marker(lo - 1)
		}
		if hi < n-1 {
			c += marker(n - 1 - hi)
		}
		return c
	}
	lo = min(max(lo, 1), n-1)
	if sel < 1 {
		lo = 1 // All: the bar starts over
	} else if sel < lo {
		lo = sel
	}
	for {
		hi = lo - 1
		for hi+1 <= n-1 && cost(lo, hi+1) <= avail {
			hi++
		}
		if sel < 1 || hi >= sel {
			return lo, hi
		}
		if lo >= sel { // not even the selected tab alone fits: show it, cut
			return lo, sel
		}
		lo++
	}
}

// tabBar is the tab bar drawn in the room of width cells (tabSpan), with a hit per tab and per end.
func (m *topModel) tabBar(tabs []view.Tab, y, width int) string {
	labels := make([]string, len(tabs))
	widths := make([]int, len(tabs))
	sel := 0
	for i, t := range tabs {
		labels[i] = fmt.Sprintf("%s %d", t.Label, t.Count)
		widths[i] = lipgloss.Width(labels[i])
		if t.Key == m.tab {
			sel = i
		}
	}
	lo, hi := tabSpan(widths, sel, m.tabLo, width-2)
	m.tabLo = lo
	var parts []string
	x := 2
	add := func(text, key string, style lipgloss.Style) {
		w := lipgloss.Width(text)
		m.hits = append(m.hits, hit{y: y, x0: x, x1: x + w, tab: key, side: -1})
		x += w + tabGap
		parts = append(parts, style.Render(text))
	}
	style := func(i int) lipgloss.Style {
		if i == sel {
			return stTitle.Underline(true)
		}
		return stMuted
	}
	add(labels[0], tabs[0].Key, style(0))
	if lo > 1 { // a click on an end moves to the neighbour that is left out
		add(fmt.Sprintf("‹ +%d", lo-1), tabs[lo-1].Key, stMuted)
	}
	for i := lo; i <= hi; i++ {
		add(labels[i], tabs[i].Key, style(i))
	}
	if hi < len(tabs)-1 {
		add(fmt.Sprintf("+%d ›", len(tabs)-1-hi), tabs[hi+1].Key, stMuted)
	}
	return "  " + strings.Join(parts, strings.Repeat(" ", tabGap))
}

// list is the column header ("" when there are no rows) and the current tab's lines, by project
// directory (view.BuildList): the directory, then its units oldest first (a team's title and its
// members' tree; a closed team's line, its members when expanded; a solo's row), all rows in one
// table with the same columns. Per line, the id of the member, solo or closed team on it ("" for
// other lines).
func (m *topModel) list(width int, now time.Time) (string, []string, []string) {
	var lines, ids []string
	// line appends l, for id when it is a selectable row.
	line := func(l, id string) {
		lines, ids = append(lines, l), append(ids, id)
	}
	muted := func(s string) string { return "  " + stMuted.Render(s) }

	l := m.listOf(m.stats(), now)
	kinds := topColumns
	if !l.AnyCwd { // CWD only when some row has one to show, decided from every row so a fold or a state never toggles it
		kinds = slices.DeleteFunc(slices.Clone(topColumns), func(c string) bool { return c == "cwd" })
	}
	cols := shown(m.cols, kinds)
	head, right, fit := layout(cols, listFit)

	// The rows first (their widths are shared by the whole table), then the lines in order.
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
	// Opening or closing a fold never moves a column: the widths come from every member of every
	// team, the ones a fold hides included.
	var hidden [][]string
	for di, d := range l.Dirs {
		if di > 0 {
			entries = append(entries, entry{text: " "})
		}
		entries = append(entries, entry{text: "  " + stMuted.Render(d.Label)}) // under the NAME header
		for _, b := range d.Blocks {
			for _, r := range b.Sizing {
				hidden = append(hidden, rowOf(r, cols, b.Depth).cells)
			}
			if h := b.Head; h != nil {
				if h.Line { // All: the team's line can fold it; in its own tab the title is only a title
					entries = append(entries, entry{id: closedRow + h.ID, text: m.teamLine(*h, m.sel == closedRow+h.ID, width, b.Depth)})
				} else {
					entries = append(entries, entry{text: stepIndent + stepIndent + m.teamTitle(*h, func(s lipgloss.Style) lipgloss.Style { return s }, width-6)})
				}
			}
			if b.NoMembers != "" {
				entries = append(entries, entry{text: muted(stepIndent + b.NoMembers)})
			}
			for _, r := range b.Rows {
				addRow(rowOf(r, cols, b.Depth))
			}
		}
	}
	if c := slices.Index(cols, "state"); c >= 0 { // the widest state word top can show: STATE's width never follows the current states
		phantom := make([]string, len(cols))
		for _, s := range view.AllStates {
			if w := view.StateText(s); lipgloss.Width(w) > lipgloss.Width(phantom[c]) {
				phantom[c] = w
			}
		}
		hidden = append(hidden, phantom)
	}
	widths := fitCols(head, append(all[:len(all):len(all)], hidden...), width-3, fit)
	if c := slices.Index(cols, "cwd"); c >= 0 { // a path too wide keeps its end
		for _, r := range all {
			r[c] = truncLeft(r[c], widths[c])
		}
	}
	var header string // one header: the table is shared by every directory; the box keeps it on its first line
	if len(all) > 0 {
		header = row(head, widths, right, func(int) lipgloss.Style { return stMuted }, false, width)
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
					return lipgloss.NewStyle().Foreground(stateColor(r.state))
				case cols[c] == "name":
					return stPlain
				}
				return stMuted
			}, r.id == m.sel, width), r.id)
		default:
			line(e.text, e.id)
		}
	}
	if l.Empty != "" {
		line(muted(l.Empty), "")
	}
	return header, lines, ids
}

// rowOf is a view row as a table row on cols: the cells as the list draws them. A member's or a
// solo's cells are "-" where it has nothing; a team's line and a gone line leave them blank.
func rowOf(r view.Row, cols []string, depth int) trow {
	in := strings.Repeat(stepIndent, depth) // a taskforce's rows are a step in under its caller
	mark := "▸ "
	if r.Open {
		mark = "▾ "
	}
	val := map[string]string{}
	blank := false
	switch r.Kind {
	case view.KindMember:
		val = map[string]string{"name": in + memberIndent + r.Prefix + r.Name + gateTagOf(r.Gate), "role": r.Role}
	case view.KindSolo:
		val = map[string]string{"name": soloLead + r.Name}
	case view.KindTeam: // " team " is the pill's width and padding; here plain, dim
		word := " team  "
		if r.Taskforce {
			word = " taskforce  "
		}
		name := in + mark + word + r.Name
		if r.Closed != "" {
			name += "  " + r.Closed
		}
		val, blank = map[string]string{"name": name}, true
	case view.KindGone:
		val, blank = map[string]string{"name": in + memberIndent + mark + r.Name}, true
	}
	val["state"], val["since"] = r.StateText, r.Since
	if !blank {
		val["harness"], val["model"], val["ctx"], val["turns"] = r.Harness, r.Model, r.Ctx, r.Turns
		val["unacked"], val["age"], val["cwd"] = fmt.Sprint(r.Unacked), r.Age, cmp.Or(r.Cwd, " ")
	}
	cells := make([]string, len(cols))
	for c, name := range cols {
		cells[c] = val[name]
		if !blank {
			cells[c] = cmp.Or(val[name], "-")
		}
	}
	return trow{id: r.ID, state: r.State, dim: r.Dim, cells: cells}
}

// teamTitle is the pill (team or taskforce), the name, its template in brackets, a taskforce's caller, `no gate` when it has none, and the held count; in room cells (0: no limit), the
// part that does not fit ends in an ellipsis and what follows it is left out.
func (m *topModel) teamTitle(t view.TeamHead, bg func(lipgloss.Style) lipgloss.Style, room int) string {
	warn := lipgloss.NewStyle().Foreground(colWarning)
	type seg struct {
		text  string
		style lipgloss.Style
	}
	segs := []seg{{t.Name, stTitle}} // the gate is tagged on its member's row, not here
	if t.Template != "" {
		segs = append(segs, seg{" [" + t.Template + "]", stMuted})
	}
	if t.Caller != "" { // a taskforce says whom it works for
		segs = append(segs, seg{" for " + t.Caller, stMuted})
	}
	for _, f := range t.Flags {
		segs = append(segs, seg{" · " + f, warn})
	}
	// a pill tells it from the directory line above, which is its root
	word := "team"
	if t.Taskforce {
		word = "taskforce"
	}
	title := pill(word, colPrimary, colOnMain) + bg(stPlain).Render(" ")
	limit := room > 0
	room -= lipgloss.Width(title)
	for _, s := range segs {
		text := s.text
		if limit {
			text = truncate(text, room)
			room -= lipgloss.Width(text)
		}
		title += bg(s.style).Render(text)
	}
	return title
}

// teamLine is a live team's line in All: a selection bar and a ▾/▸ mark, then its title (gate, held
// amber); folded, also the counts of its members by state and its unacked mail. A selected line
// is filled to width like a row.
func (m *topModel) teamLine(t view.TeamHead, sel bool, width, depth int) string {
	bg := func(s lipgloss.Style) lipgloss.Style {
		if sel {
			return s.Background(colSurface)
		}
		return s
	}
	bar, mark := "  ", "▸ " // the pill's own padding is on its background, so a space sets it off
	if sel {
		bar = "▌ "
	}
	if t.Open {
		mark = "▾ "
	}
	in := strings.Repeat(stepIndent, depth) // a taskforce is a step in under its caller, as its rows are
	room := width - 1 - 4 - len(in)         // as a row: the bar and the mark, and a cell of margin
	line := bg(lipgloss.NewStyle().Foreground(colPrimary)).Render(bar) + bg(stPlain).Render(in) + bg(stMuted).Render(mark) + m.teamTitle(t, bg, room)
	if !t.Open {
		parts := t.Counts // held is in the title, amber
		// the counts that fit, whole, the last ones dropped first; none at all if the title takes the room
		left := room - lipgloss.Width(m.teamTitle(t, func(s lipgloss.Style) lipgloss.Style { return s }, 0)) - 3
		for len(parts) > 0 && lipgloss.Width(strings.Join(parts, " · ")) > left {
			parts = parts[:len(parts)-1]
		}
		if len(parts) > 0 {
			line += bg(stMuted).Render("   " + strings.Join(parts, " · "))
		}
	}
	if pad := width - lipgloss.Width(line); sel && pad > 0 {
		line += bg(stPlain).Render(strings.Repeat(" ", pad))
	}
	return line
}

// The list's levels, in cells of the NAME cell (which starts under the NAME header): the directory
// line, a team's mark, a closed team's line and a solo at 0; a team's members (their tree, and the
// gone row) one step in, under the team pill's left edge. The indent is part of the NAME cell, so
// no column moves.
const (
	stepIndent   = "  "
	memberIndent = stepIndent
)

// trow is one row of the list's table: its cells, and what the row is (a member, a solo, a team's
// folded gone members) for its selection and colours.
type trow struct {
	cells []string
	id    string
	state string
	dim   bool
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
// overviewLabel is the Overview's label cell: wider than its longest label (`last turn`), so a gap
// always separates the label from its value.
const overviewLabel = 10

// selMember is the selected member and its team, nil when a solo or a row of another kind is selected.
func (m *topModel) selMember() (*core.MemberState, *core.TeamState) {
	for i := range m.ps.Teams {
		for j := range m.ps.Teams[i].Members {
			if m.ps.Teams[i].Members[j].ID == m.sel {
				return &m.ps.Teams[i].Members[j], &m.ps.Teams[i]
			}
		}
	}
	return nil, nil
}

func (m *topModel) sidebar(w, h int, now time.Time) []string {
	m.modelRow = -1
	d := view.Describe(m.ps.State, m.sel, m.stats(), now)
	switch d.Kind {
	case view.DetailNone:
		return []string{" " + stMuted.Render("Nothing selected.")}
	case view.DetailParticipant:
		if m.sideTab == sideTail {
			return m.tailLines(w, h)
		}
	}
	kv := func(k, v string) string { return " " + stMuted.Render(fmt.Sprintf("%-*s", overviewLabel, k)) + v }
	head := " " + lipgloss.NewStyle().Bold(true).Render(d.Title) + stMuted.Render(" · "+d.Sub)
	if d.State != "" {
		icon, col := stateIcon(d.State)
		head = " " + lipgloss.NewStyle().Bold(true).Render(d.Title) + stMuted.Render(" · "+d.Sub+" ") + pill(icon, col, colInk)
	}
	out := []string{head, ""}
	out = append(out, assignmentRows(d.Task, w-1-overviewLabel)...)
	for _, f := range d.Facts {
		v := f.Value
		if f.Pick { // its model can be changed here: blue with a ▾, tinted under the mouse
			v += " ▾"
			m.modelRow, m.modelOf, m.modelW = len(out), d.ID, lipgloss.Width(v)
			style := lipgloss.NewStyle().Foreground(colLink)
			if m.hover == d.ID {
				style = style.Background(colSurface)
			}
			v = style.Render(v)
		}
		if f.Note != "" {
			v += "  " + stMuted.Render(f.Note)
		}
		if f.Muted {
			v = stMuted.Render(v)
		}
		out = append(out, kv(f.Label, v))
	}
	if d.Hint != "" {
		if d.HintGap {
			out = append(out, "")
		}
		out = append(out, " "+stMuted.Render(d.Hint))
	}
	return out
}

// tailLines is the selected participant's Tail: the latest lines of its log, in w x h.
func (m *topModel) tailLines(w, h int) []string {
	if !m.tailable(m.sel) {
		return []string{" " + stMuted.Render("No tail: piggery has no log of it to read.")}
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

// assignmentRows is the member's current task in the sidebar (width n after the 9-cell label):
// its title on at most two lines, who gave it and when, how its reply chain stands (muted), and,
// once the member handed back, the newer mail from the assigner that is not part of the chain
// (a note, or a task sent without op assign). Nothing when it has none.
func assignmentRows(t *view.Task, n int) []string {
	if t == nil {
		return nil
	}
	n = max(n, 10)
	indent := strings.Repeat(" ", 1+overviewLabel)
	lines := wrapTwo(t.Title, n)
	out := []string{" " + stMuted.Render(fmt.Sprintf("%-*s", overviewLabel, "task")) + lines[0]}
	for _, l := range lines[1:] {
		out = append(out, indent+l)
	}
	out = append(out, indent+t.From)
	if t.Chain != "" {
		out = append(out, indent+stMuted.Render(t.Chain))
	}
	if x := t.Mail; x != nil {
		out = append(out, " "+stMuted.Render(fmt.Sprintf("%-*s", overviewLabel, "mail")+x.Head+truncate(x.Title, n-lipgloss.Width(x.Head+x.Tail))+x.Tail))
	}
	return out
}

// wrapTwo breaks s at a space into at most two lines of n cells; the second is cut with … if needed.
func wrapTwo(s string, n int) []string {
	if lipgloss.Width(s) <= n {
		return []string{s}
	}
	w, at, space := 0, 0, 0
	for i, r := range s {
		if w += lipgloss.Width(string(r)); w > n {
			break
		}
		if r == ' ' {
			space = i
		}
		at = i + len(string(r))
	}
	if space > 0 {
		at = space
	}
	return []string{strings.TrimSpace(s[:at]), truncate(strings.TrimSpace(s[at:]), n)}
}

func (m *topModel) eventRows(n int, now time.Time) (rows [][]string, tones []view.Tone) {
	for _, ev := range view.EventRows(m.ps.State, n, now) {
		rows = append(rows, []string{ev.Time, ev.Who, ev.Type, ev.Target})
		tones = append(tones, ev.Tone)
	}
	return rows, tones
}

// eventStyle is an event row's colour: muted, amber for a refusal, red for an exit.
func eventStyle(tone view.Tone) lipgloss.Style {
	switch tone {
	case view.ToneWarning:
		return lipgloss.NewStyle().Foreground(colWarning)
	case view.ToneDanger:
		return lipgloss.NewStyle().Foreground(colError)
	}
	return stMuted
}

// sideSplit is the width of the Overview panel and whether it is beside the list: the one split the
// list | Overview columns and the band below them (notices | events) share.
func (m *topModel) sideSplit(width int) (sw int, beside bool) {
	return max(sideMin, width/3), m.sideShown() && m.sel != "" && !m.narrow()
}

// band is the bottom of the screen above the keys: the notices (psResult.Notices: what the notify
// hooks received) left, at the list's width, and the events right, at the Overview's, split where the
// list and the Overview are. Each is a box or, folded, one line at the top of its side; the band is as
// tall as the taller side. With no notices the events take the width, and where the Overview is not
// beside the list the two are stacked.
func (m *topModel) band(width, height, sw int, beside bool, now time.Time) []string {
	if len(m.ps.Notices) == 0 {
		return m.eventsBox(width, height, now, 0)
	}
	if !beside {
		return append(m.noticesBox(width, height, now, 0), m.eventsBox(width, height, now, 0)...)
	}
	lw := width - sw
	left, right := m.noticesBox(lw, height, now, 0), m.eventsBox(sw, height, now, 0)
	n := max(len(left), len(right))
	if m.notices {
		left = m.noticesBox(lw, height, now, n)
	}
	if m.events {
		right = m.eventsBox(sw, height, now, n)
	}
	pad := func(lines []string, i, w int) string {
		if i >= len(lines) {
			return strings.Repeat(" ", w)
		}
		return lines[i] + strings.Repeat(" ", max(w-lipgloss.Width(lines[i]), 0))
	}
	out := make([]string, n)
	for i := range out {
		out[i] = pad(left, i, lw) + pad(right, i, sw)
	}
	return out
}

// noticesBox is the latest notices in a box of width w (2 rows in a short window, else up to 5), at
// least h lines tall, or, folded, one line with the newest.
func (m *topModel) noticesBox(w, height int, now time.Time, h int) []string {
	rows := view.NoticeRows(m.ps.Notices, now)
	warn := func(r view.NoticeRow) bool { return r.Kind == "failed" || r.Kind == "gate_lost" }
	if !m.notices { // folded: one line of text, indented like the key lines
		line := " " + stTitle.Render("● Notices")
		if len(rows) > 0 {
			latest := truncate(rows[0].Age+" "+rows[0].Kind+" "+rows[0].Body, max(w-lipgloss.Width(" ● Notices · "), 0))
			st := stMuted
			if warn(rows[0]) {
				st = eventStyle(view.ToneWarning)
			}
			line += stRule.Render(" · ") + st.Render(latest)
		}
		return []string{line}
	}
	n := 5
	if height < 30 {
		n = 2
	}
	rows = rows[:min(len(rows), n)]
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = []string{r.Age, r.Where, r.Kind, r.Body}
	}
	inner := w - 2
	cw := fitCols(noticeCols, cells, inner-3, noticeFit)
	lines := make([]string, len(rows))
	for i, r := range rows {
		kind := stMuted
		if warn(r) {
			kind = eventStyle(view.ToneWarning)
		}
		lines[i] = row(cells[i], cw, nil, func(c int) lipgloss.Style {
			switch c {
			case 2:
				return kind
			case 3:
				return stPlain
			}
			return stMuted
		}, false, inner)
	}
	return box(stTitle.Render("Notices"), lines, w, max(len(lines)+2, h))
}

// eventsBox is the latest events in a box like the Overview's (3 rows in a short window, 5, or 8 in
// a tall one), at least h lines tall, or, collapsed, one rule line with the title and the latest event.
func (m *topModel) eventsBox(width, height int, now time.Time, h int) []string {
	n := 8
	switch {
	case height < 30:
		n = 3
	case height < 40:
		n = 5
	}
	if !m.events { // folded: one line of text, no rule, indented like the key lines
		rows, tones := m.eventRows(1, now)
		line := " " + stTitle.Render("● Events")
		if len(rows) > 0 {
			latest := truncate(strings.TrimSpace(strings.Join(rows[0], " ")), max(width-lipgloss.Width(" ● Events · "), 0))
			line += stRule.Render(" · ") + eventStyle(tones[0]).Render(latest)
		}
		return []string{line}
	}
	rows, tones := m.eventRows(n, now)
	inner := width - 2
	w := fitCols(eventCols, rows, inner-3, eventFit)
	lines := make([]string, len(rows))
	if len(rows) == 0 {
		lines = []string{" " + stMuted.Render("No events yet.")}
	}
	for i, r := range rows {
		st := eventStyle(tones[i])
		lines[i] = row(r, w, nil, func(int) lipgloss.Style { return st }, false, inner)
	}
	return box(stTitle.Render("Events"), lines, width, max(max(len(rows), 1)+2, h))
}

// styleTail colors a tail line (view.ParseTail's kinds) cut to n cells: tool calls secondary with
// dim arguments, results muted (errors red), problems amber, assistant text plain.
func styleTail(l string, n int) string {
	t := view.ParseTail(l)
	switch t.Kind {
	case view.TailTool:
		head := truncate(t.Text, n)
		return lipgloss.NewStyle().Foreground(colTool).Render(head) + stMuted.Render(truncate(t.Rest, n-lipgloss.Width(head)))
	case view.TailError:
		return lipgloss.NewStyle().Foreground(colError).Render(truncate(t.Text, n))
	case view.TailResult, view.TailUser:
		return stMuted.Render(truncate(t.Text, n))
	case view.TailWarning:
		return lipgloss.NewStyle().Foreground(colWarning).Render(truncate(t.Text, n))
	case view.TailRule:
		return stRule.Render(truncate(t.Text, n))
	}
	return truncate(t.Text, n)
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

// withVersion right-aligns the first of notes that fits on the footer's last line, muted (amber
// for a mismatch); when none fits beside the key hints, the version goes and the hints stay whole.
func withVersion(line string, notes []string, mismatch bool, width int) string {
	style := stMuted
	if mismatch {
		style = lipgloss.NewStyle().Foreground(colWarning)
	}
	for _, note := range notes {
		if pad := width - lipgloss.Width(line) - lipgloss.Width(note) - 1; pad >= 2 {
			return line + strings.Repeat(" ", pad) + style.Render(note)
		}
	}
	return line
}

// keyLines is the key footer: two lines by purpose (move and look, act and toggle), or the full
// list under `?`.
func (m *topModel) keyLines(w int) []string {
	if m.help.ShowAll {
		return strings.Split(m.help.View(m.keys), "\n")
	}
	act := m.keys.act()
	mem, team := m.selMember()
	act[1].SetEnabled(mem != nil && view.Pickable(*mem, team.ID, m.ps.Closed)) // M model is always listed, dim where it does nothing
	return keyGrid(w, m.keys.look(), act)
}

// keyGrid lays the short key lines out as columns: the i-th entry of every line starts at the
// same cell, so the lines read as one block. Keys muted, descriptions subtle, as helpStyles; each
// line is cut to w.
func keyGrid(w int, lines ...[]key.Binding) []string {
	cell := func(b key.Binding) string { return b.Help().Key + " " + b.Help().Desc }
	var cols []int
	for _, l := range lines {
		for i, b := range l {
			if i == len(cols) {
				cols = append(cols, 0)
			}
			cols[i] = max(cols[i], lipgloss.Width(cell(b)))
		}
	}
	out := make([]string, len(lines))
	for li, l := range lines {
		for len(l) > 1 { // the entries that fit, whole: the last ones go before one is cut mid-word
			end := 0
			for i, k := range l {
				end += lipgloss.Width(cell(k)) + 3
				if i < len(l)-1 {
					end += cols[i] - lipgloss.Width(cell(k))
				}
			}
			if end-3 <= w {
				break
			}
			l = l[:len(l)-1]
		}
		var b strings.Builder
		for i, k := range l {
			if i > 0 {
				b.WriteString("   ")
			}
			keyStyle := stMuted
			if !k.Enabled() { // a key that does nothing here: dimmer than the others, key and description alike
				keyStyle = stRule
			}
			b.WriteString(keyStyle.Render(k.Help().Key) + " " + stRule.Render(k.Help().Desc))
			if i < len(l)-1 {
				b.WriteString(strings.Repeat(" ", cols[i]-lipgloss.Width(cell(k))))
			}
		}
		out[li] = lipgloss.NewStyle().MaxWidth(w).Render(b.String())
	}
	return out
}
