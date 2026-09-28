package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
)

// ps prints the operator snapshot: daemon, teams and members, solos, pending
// mail. --json prints the verb's result as is, with "projects" added: the same grouping.
func (e *env) ps(args []string) error {
	pos, err := parse(e.flags("ps"), args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%w: ps takes no arguments", errUsage)
	}
	cols, err := e.columns()
	if err != nil {
		return err
	}
	if e.json {
		return e.psJSON()
	}
	return do(e, proto.VerbPs, core.StateArgs{}, func(w io.Writer, r proto.PsResult) {
		for _, l := range psLines(r, time.Now(), nil, cols) {
			fmt.Fprintln(w, l.text)
		}
	})
}

// columns is display.columns from config.yaml, read on every run; an unknown column is a
// warning on stderr and the default columns.
func (e *env) columns() ([]string, error) {
	set, err := server.LoadSettings(e.dir)
	if err != nil {
		return nil, err
	}
	cols, warn := server.ColumnsOf(set, e.dir)
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warning:", warn)
	}
	return cols, nil
}

// psLine is one row of ps/top. Worker is the participant id when the row is a headless
// member (top selects it to show its tail), else "".
type psLine struct {
	text   string
	worker string
	kind   string // daemon, team, member, solo
}

// psLines is ps's text, by project directory as top lists (groupByDir): each directory, then its
// teams (a line, then its members' tree) and solos, oldest first; closed teams are only counted.
// stats is context and turns per worker (participant id), nil for ps (no ctx or turns then).
// cols are display.columns: the columns after the name, in order, the same for every row; a
// column a row does not have is "-", its cwd blank when it is the directory.
func psLines(r proto.PsResult, now time.Time, stats map[string]workerStats, cols []string) []psLine {
	shownCols := shown(cols, server.DisplayColumns)[1:]
	// fields writes a row's columns in cols order, each as ps writes it; val has the row's.
	fields := func(val map[string]string) string {
		format := map[string]string{"role": "%-10s", "state": "%-19s", "harness": "%-13s", "model": "%-22s",
			"ctx": "ctx %-6s", "turns": "turns %-4s", "unacked": "unacked=%-3s", "age": "age %-8s", "since": "for %-8s",
			"cwd": "%-16s"}
		var b strings.Builder
		for _, c := range shownCols {
			if stats == nil && (c == "ctx" || c == "turns") {
				continue // ps has no worker logs read
			}
			v, ok := val[c]
			if !ok {
				v = "-"
			}
			b.WriteString(" " + fmt.Sprintf(format[c], v))
		}
		return b.String()
	}
	out := []psLine{{kind: "daemon", text: fmt.Sprintf("piggery pid=%d up=%s  held=%d unacked=%d",
		r.PID, since(r.StartedAt, now), r.Held, r.Unacked)}}
	groups, short := psGroups(r)
	for gi, g := range groups {
		out = append(out, psLine{kind: "dir", text: dirLabel(short[gi])})
		for _, u := range g.units {
			if s := u.solo; s != nil {
				val := map[string]string{"state": s.State, "harness": harnessLabel(s.Harness, false), "model": modelID(s.Model),
					"unacked": fmt.Sprint(s.Unacked), "age": since(s.CreatedAt, now), "since": since(s.StateSince, now),
					"cwd": relCwd(g.dir, s.Cwd)}
				out = append(out, psLine{kind: "solo", text: strings.TrimRight(fmt.Sprintf("    %-22s%s %s", "solo "+s.Name, fields(val),
					protocolTag(s.ProtocolVersion, r.ProtocolVersion)), " ")})
				continue
			}
			t := u.team
			gate := t.Gate
			if gate == "" {
				gate = "(none)"
			}
			out = append(out, psLine{kind: "team", text: fmt.Sprintf("  team %s  gate=%s  held=%d unacked=%d",
				t.Name, gate, t.Held, t.Unacked)})
			for _, row := range memberTree(t.Members) {
				m := row.m
				var tags []string
				if m.Gate {
					tags = append(tags, "gate")
				}
				if tag := protocolTag(m.ProtocolVersion, r.ProtocolVersion); tag != "" {
					tags = append(tags, tag)
				}
				turn := "-"
				if m.LastTurnEnd > 0 {
					turn = since(m.LastTurnEnd, now) + " ago"
				}
				worker := ""
				if m.Headless {
					worker = m.ID
				}
				val := map[string]string{"role": m.Role, "state": m.State, "harness": harnessLabel(m.Harness, m.Headless),
					"model": modelID(m.Model), "unacked": fmt.Sprint(m.Unacked), "age": since(m.CreatedAt, now),
					"since": since(m.StateSince, now), "cwd": relCwd(g.dir, m.Cwd)}
				if ws, ok := stats[m.ID]; ok {
					if ws.hasCtx {
						val["ctx"] = tokens(ws.ctx)
					}
					val["turns"] = fmt.Sprint(ws.turns)
				}
				out = append(out, psLine{kind: "member", worker: worker, text: strings.TrimRight(fmt.Sprintf("    %-22s%s last turn %-9s %s",
					row.prefix+m.Name, fields(val), turn, strings.Join(tags, " ")), " ")})
			}
		}
	}
	if n := len(r.Closed); n > 0 { // listed in --json and top
		out = append(out, psLine{kind: "closed", text: fmt.Sprintf("%d closed %s (gc to remove)", n,
			map[bool]string{true: "team", false: "teams"}[n == 1])})
	}
	return out
}

// protocolTag marks a session whose adapter speaks another protocol version than the daemon;
// doctor says what to do.
func protocolTag(adapter *int, daemon int) string {
	if adapter == nil || *adapter == daemon {
		return ""
	}
	return fmt.Sprintf("⚠ protocol %d (daemon %d)", *adapter, daemon)
}

// psGroups is how the text ps lists r: open teams and solos by project directory (groupByDir),
// with each directory's short label. --json lists top's All tab, recently closed teams too.
func psGroups(r proto.PsResult) ([]dirGroup, []string) {
	var roots []string
	for _, t := range teamsOf(r) {
		roots = append(roots, t.Root)
	}
	groups := groupByDir(r.Teams, nil, r.Solos, roots)
	dirs := make([]string, len(groups))
	for i, g := range groups {
		dirs[i] = g.dir
	}
	return groups, shortPaths(dirs)
}

// psProject is one project of ps --json: the grouping of the text ps, by id; a row's data is in
// teams[].members and solos[].
type psProject struct {
	Label string   `json:"label"`
	Path  string   `json:"path"`
	Units []psUnit `json:"units"`
}

type psUnit struct {
	Kind    string     `json:"kind"`          // team, closed (a team closed recently: see closed[]), solo
	ID      string     `json:"id"`            // the team's id, or the solo's participant id
	Cwd     string     `json:"cwd,omitempty"` // solo: as ps shows it ("" the directory, ./sub, ~/…)
	Members []psMember `json:"members,omitempty"`
}

// psMember is a team member in its reports_to tree, in display order. Ctx (context now) and
// Turns (over every run) are top's, for a worker top shows them for; ctx nil until known.
type psMember struct {
	ID     string `json:"id"`
	Depth  int    `json:"depth"`
	Prefix string `json:"prefix"` // ├─ └─ │ as ps draws it
	Cwd    string `json:"cwd"`
	Ctx    *int   `json:"ctx,omitempty"`
	Turns  *int   `json:"turns,omitempty"`
}

// psProjects is top's All tab by id (its groups: open teams, teams closed recently, solos, by
// project directory), with top's stats of each worker.
func psProjects(r proto.PsResult, stats map[string]workerStats) []psProject {
	groups := (&topModel{ps: r}).groups()
	dirs := make([]string, len(groups))
	for i, g := range groups {
		dirs[i] = g.dir
	}
	short := shortPaths(dirs)
	out := make([]psProject, len(groups))
	for gi, g := range groups {
		pr := psProject{Label: short[gi], Path: g.dir, Units: []psUnit{}}
		for _, u := range g.units {
			if s := u.solo; s != nil {
				pr.Units = append(pr.Units, psUnit{Kind: "solo", ID: s.ID, Cwd: relCwd(g.dir, s.Cwd)})
				continue
			}
			tu := psUnit{Kind: "team", ID: u.team.ID, Members: []psMember{}}
			if u.closed != nil {
				tu.Kind = "closed"
			}
			for _, row := range memberTree(u.team.Members) {
				pm := psMember{ID: row.m.ID, Depth: row.depth, Prefix: row.prefix, Cwd: relCwd(g.dir, row.m.Cwd)}
				if ws, ok := stats[row.m.ID]; ok {
					pm.Turns = &ws.turns
					if ws.hasCtx {
						pm.Ctx = &ws.ctx
					}
				}
				tu.Members = append(tu.Members, pm)
			}
			pr.Units = append(pr.Units, tu)
		}
		out[gi] = pr
	}
	return out
}

// psJSON prints the daemon's ps result as it sent it, with "projects" added at the end.
func (e *env) psJSON() error {
	c, err := e.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	var r proto.PsResult
	raw, err := c.CallInto(proto.VerbPs, core.StateArgs{}, &r)
	if err != nil {
		return err
	}
	top := &topModel{dir: e.dir, ps: r, logs: readLogs(e.dir, r, nil)}
	out, err := withProjects(raw, r, top.stats())
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%s\n", out)
	return nil
}

// withProjects is raw (the ps result r as the daemon sent it) with "projects" appended: every
// field the daemon wrote stays byte for byte.
func withProjects(raw []byte, r proto.PsResult, stats map[string]workerStats) ([]byte, error) {
	projects, err := json.Marshal(psProjects(r, stats))
	if err != nil {
		return nil, err
	}
	obj := bytes.TrimSpace(raw)
	if len(obj) < 2 || obj[len(obj)-1] != '}' {
		return nil, fmt.Errorf("ps: unexpected result from the daemon")
	}
	sep := ","
	if len(bytes.TrimSpace(obj[1:len(obj)-1])) == 0 {
		sep = ""
	}
	return fmt.Appendf(nil, "%s%s\"projects\":%s}", obj[:len(obj)-1], sep, projects), nil
}

// harnessLabel is what runs a participant: pi, claude, codex; `pi·worker` when piggery runs it
// headless (a worker it spawned); "-" unknown.
func harnessLabel(harness string, headless bool) string {
	switch {
	case harness == "":
		return "-"
	case headless:
		return harness + "·worker"
	}
	return harness
}

// treeRow is a member in tree order: prefix draws its place under its reports_to (├─ └─ │),
// parentGone says that parent is gone (top dims it).
type treeRow struct {
	m          core.MemberState
	prefix     string
	depth      int
	parentGone bool
}

// memberTree orders a team's members as the tree of reports_to: each parent before its
// children, children in the order they entered the team (the order of ms). A member whose
// parent is not in the team is a root; a gone parent keeps its children.
func memberTree(ms []core.MemberState) []treeRow {
	byID := map[string]core.MemberState{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	children := map[string][]core.MemberState{}
	var roots []core.MemberState
	for _, m := range ms {
		if _, ok := byID[m.ReportsTo]; ok && m.ReportsTo != m.ID {
			children[m.ReportsTo] = append(children[m.ReportsTo], m)
		} else {
			roots = append(roots, m)
		}
	}
	var out []treeRow
	seen := map[string]bool{}
	var walk func(m core.MemberState, indent, branch string, depth int)
	walk = func(m core.MemberState, indent, branch string, depth int) {
		if seen[m.ID] { // reports_to never loops; do not trust that here
			return
		}
		seen[m.ID] = true
		out = append(out, treeRow{m: m, prefix: indent + branch, depth: depth,
			parentGone: depth > 0 && byID[m.ReportsTo].State == "gone"})
		kids := children[m.ID]
		next := indent
		switch branch {
		case "├─ ":
			next += "│  "
		case "└─ ":
			next += "   "
		}
		for i, k := range kids {
			b := "├─ "
			if i == len(kids)-1 {
				b = "└─ "
			}
			walk(k, next, b, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, "", "", 0)
	}
	for _, m := range ms { // members only reachable through a loop
		if !seen[m.ID] {
			walk(m, "", "", 0)
		}
	}
	return out
}

// tokens is a token count for display: 857, 15.5k, 1.2M.
func tokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}

// since is the time from ms to now, rounded for display.
func since(ms int64, now time.Time) string {
	if ms == 0 {
		return "-"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Hour).String()
}
