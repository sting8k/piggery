package cli

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
	"github.com/sting8k/piggery/internal/view"
)

// ps prints the operator snapshot: daemon, teams and members, solos, pending
// mail. --json prints the verb's result as is, with "projects" added: the same grouping.
func (e *env) ps(args []string) error {
	fs := e.flags("ps")
	asView := fs.Bool("view", false, "print what top shows, as versioned JSON (for the Paseo plugin)")
	pos, err := parse(fs, args)
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
	if *asView {
		return e.psView()
	}
	if e.json {
		return e.psJSON()
	}
	return do(e, proto.VerbPs, core.StateArgs{}, func(w io.Writer, r proto.PsResult) {
		for _, l := range psLines(r, time.Now(), nil, cols) {
			fmt.Fprintln(w, l.text)
			if l.kind == "daemon" { // the header: the daemon's line, then what setup has to bring up
				if n := proto.Notice(r.Outdated); n != "" {
					fmt.Fprintln(w, n)
				}
			}
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
	kind   string // daemon, team, member, gone (a team's gone members, one line), solo
}

// psLines is ps's text, by project directory as top lists (view.BuildList, without closed teams):
// each directory, then its teams (a line, then its members' tree, the gone ones with nobody live
// below them as one line) and solos, oldest first; closed teams are only counted. The rows are
// view's; what stays here is ps's own drawing: its formats, the team line's gate/held/unacked, the
// protocol tag, the worker id. stats is context and turns per worker (participant id), nil for ps
// (no ctx or turns then). cols are display.columns: the columns after the name, in order, the same
// for every row; a column a row does not have is "-", its cwd blank when it is the directory.
func psLines(r proto.PsResult, now time.Time, stats map[string]view.Stats, cols []string) []psLine {
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
	// vals is a row's columns as ps words them (ages as since writes them, from the row's own times).
	vals := func(row view.Row) map[string]string {
		return map[string]string{"state": row.State, "harness": row.Harness, "model": row.Model, "unacked": fmt.Sprint(row.Unacked),
			"age": since(row.CreatedAt, now), "since": since(row.SinceAt, now), "cwd": row.Cwd, "ctx": row.Ctx, "turns": row.Turns}
	}
	teams, members, solos := map[string]core.TeamState{}, map[string]core.MemberState{}, map[string]core.SoloState{}
	for _, t := range r.Teams {
		teams[t.ID] = t
		for _, m := range t.Members {
			members[m.ID] = m
		}
	}
	for _, s := range r.Solos {
		solos[s.ID] = s
	}
	out := []psLine{{kind: "daemon", text: fmt.Sprintf("piggery pid=%d up=%s  held=%d unacked=%d",
		r.PID, since(r.StartedAt, now), r.Held, r.Unacked)}}
	for _, d := range view.BuildList(view.ListInput{State: r.State, Tab: view.TabOpen, Now: now, Stats: stats}).Dirs {
		out = append(out, psLine{kind: "dir", text: d.Label})
		for _, b := range d.Blocks {
			in := strings.Repeat("    ", b.Depth) // a taskforce is drawn a level in under its caller: its line under the caller's row, its members one more in
			if h := b.Head; h != nil {
				t := teams[h.ID]
				tpl := ""
				if h.Template != "" {
					tpl = " [" + h.Template + "]"
				}
				word, caller := "team", ""
				if h.Taskforce {
					word = "taskforce"
					if h.Caller != "" {
						caller = "  caller=" + h.Caller
					}
				}
				out = append(out, psLine{kind: "team", text: fmt.Sprintf("  %s%s %s%s  gate=%s  held=%d unacked=%d%s",
					in, word, t.Name, tpl, cmp.Or(t.Gate, "(none)"), t.Held, t.Unacked, caller)})
			}
			for _, row := range b.Rows {
				switch row.Kind {
				case view.KindSolo:
					s := solos[row.ID]
					out = append(out, psLine{kind: "solo", text: strings.TrimRight(fmt.Sprintf("    %-22s%s %s", "solo "+row.Name, fields(vals(row)),
						protocolTag(s.ProtocolVersion, r.ProtocolVersion)), " ")})
				case view.KindMember:
					m := members[row.ID]
					var tags []string
					if row.Gate {
						tags = append(tags, "gate")
					}
					if tag := protocolTag(m.ProtocolVersion, r.ProtocolVersion); tag != "" {
						tags = append(tags, tag)
					}
					turn := "-"
					if row.LastTurnEnd > 0 {
						turn = since(row.LastTurnEnd, now) + " ago"
					}
					worker := ""
					if m.Headless {
						worker = m.ID
					}
					val := vals(row)
					val["role"] = row.Role
					out = append(out, psLine{kind: "member", worker: worker, text: strings.TrimRight(fmt.Sprintf("    %-22s%s last turn %-9s %s",
						in+row.Prefix+row.Name, fields(val), turn, strings.Join(tags, " ")), " ")})
				case view.KindGone:
					out = append(out, psLine{kind: "gone", text: fmt.Sprintf("    %-22s✗ gone  for %s", in+row.Name, since(row.SinceAt, now))})
				}
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

// psProject is one project of ps --json: the grouping of the text ps, by id; a row's data is in
// teams[].members and solos[].
type psProject struct {
	Label string   `json:"label"`
	Path  string   `json:"path"`
	Units []psUnit `json:"units"`
}

type psUnit struct {
	Kind    string     `json:"kind"`            // team, closed (a team closed recently: see closed[]), solo
	ID      string     `json:"id"`              // the team's id, or the solo's participant id
	Under   string     `json:"under,omitempty"` // a taskforce: the participant that called it up, whose unit it follows
	Cwd     string     `json:"cwd,omitempty"`   // solo: as ps shows it ("" the directory, ./sub, ~/…)
	Ctx     *int       `json:"ctx,omitempty"`   // solo: as a member's, from its transcript
	Turns   *int       `json:"turns,omitempty"`
	Members []psMember `json:"members,omitempty"`
}

// psMember is a team member in its reports_to tree, in display order. Ctx (context now) and
// Turns (a worker's over every run, a session's in its transcript) are top's, for a member top
// shows them for; ctx nil until known.
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
func psProjects(r proto.PsResult, stats map[string]view.Stats) []psProject {
	groups := view.Groups(r.State, "", time.Now())
	dirs := make([]string, len(groups))
	for i, g := range groups {
		dirs[i] = g.Dir
	}
	short := view.ShortPaths(dirs)
	out := make([]psProject, len(groups))
	for gi, g := range groups {
		pr := psProject{Label: short[gi], Path: g.Dir, Units: []psUnit{}}
		for _, u := range g.Units {
			if s := u.Solo; s != nil {
				su := psUnit{Kind: "solo", ID: s.ID, Cwd: view.RelCwd(g.Dir, s.Cwd)}
				if ws, ok := stats[s.ID]; ok {
					su.Turns = &ws.Turns
					if ws.HasCtx {
						su.Ctx = &ws.Ctx
					}
				}
				pr.Units = append(pr.Units, su)
				continue
			}
			tu := psUnit{Kind: "team", ID: u.Team.ID, Under: u.Under, Members: []psMember{}}
			if u.Closed != nil {
				tu.Kind = "closed"
			}
			for _, row := range view.MemberTree(u.Team.Members) {
				pm := psMember{ID: row.M.ID, Depth: row.Depth, Prefix: row.Prefix, Cwd: view.RelCwd(g.Dir, row.M.Cwd)}
				if ws, ok := stats[row.M.ID]; ok {
					pm.Turns = &ws.Turns
					if ws.HasCtx {
						pm.Ctx = &ws.Ctx
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

// psView prints what top shows of the snapshot (internal/view) as JSON, with the daemon's identity
// as the header draws it. The version is view.Version.
func (e *env) psView() error {
	c, err := e.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	var r proto.PsResult
	if _, err := c.CallInto(proto.VerbPs, core.StateArgs{}, &r); err != nil {
		return err
	}
	top := &topModel{dir: e.dir, ps: r, logs: readLogs(e.dir, r, loadLogCache(e.dir))}
	saveLogCache(e.dir, top.logs)
	out, err := json.Marshal(viewDoc(r, top.stats(), time.Now()))
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%s\n", out)
	return nil
}

// psViewDoc is view.Doc with the daemon's own fields the header shows: since when it runs, its build
// version and what to say when this binary differs, and the notice for outdated installs.
type psViewDoc struct {
	view.Doc
	Notices []view.NoticeRow `json:"notices"` // the latest notices to the operator, newest first
	Daemon  struct {
		PID          int      `json:"pid"`
		StartedAt    int64    `json:"started_at"`
		Age          string   `json:"age"`
		Version      string   `json:"version,omitempty"`
		VersionNotes []string `json:"version_notes,omitempty"`
		Mismatch     bool     `json:"version_mismatch,omitempty"`
		Outdated     string   `json:"outdated,omitempty"`
		Update       string   `json:"update,omitempty"` // "v0.7.0 available: piggery update": the daily check found a newer release
	} `json:"daemon"`
}

func viewDoc(r proto.PsResult, stats map[string]view.Stats, now time.Time) psViewDoc {
	d := psViewDoc{Doc: view.Snapshot(r.State, view.SnapshotInput{Now: now, Stats: stats, Readable: hasReader})}
	d.Daemon.PID, d.Daemon.StartedAt, d.Daemon.Age, d.Daemon.Version = r.PID, r.StartedAt, view.Ago(r.StartedAt, now), r.Version
	d.Daemon.VersionNotes, d.Daemon.Mismatch = view.VersionNotes(r.Version, Version)
	d.Daemon.Outdated = proto.Notice(r.Outdated)
	d.Daemon.Update = proto.UpdateNotice(r.Update)
	d.Notices = view.NoticeRows(r.Notices, now)
	return d
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
	top := &topModel{dir: e.dir, ps: r, logs: readLogs(e.dir, r, loadLogCache(e.dir))}
	saveLogCache(e.dir, top.logs)
	out, err := withProjects(raw, r, top.stats())
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%s\n", out)
	return nil
}

// withProjects is raw (the ps result r as the daemon sent it) with "projects" appended: every
// field the daemon wrote stays byte for byte.
func withProjects(raw []byte, r proto.PsResult, stats map[string]view.Stats) ([]byte, error) {
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
