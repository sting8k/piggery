// Package cli is the thin `piggery <cmd>` client: it parses flags, picks admin or participant
// auth explicitly (never falling back between them), calls the daemon, and prints the result.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
	"github.com/sting8k/piggery/internal/view"
	"github.com/sting8k/piggery/manifests"
)

// errUsage marks a command-line mistake (exit 2).
var errUsage = errors.New("usage")

// errFindings makes doctor exit 1 after printing its findings, with nothing on stderr.
var errFindings = errors.New("findings")

type env struct {
	dir   string
	admin bool
	json  bool
	// noStart: fail when no daemon runs instead of starting one (--no-start, for read-only
	// callers that poll, e.g. the Paseo plugin: they must not bring back a stopped daemon).
	noStart bool
	stdout  io.Writer
	cur     *cobra.Command // the command being run (its usage follows a usage error)
}

func formatErr(ce *core.Error) string {
	if ce.Code == core.CodeStartFailed {
		// The worker and its task message exist; only its process did not start.
		d, _ := ce.Details.(map[string]any)
		return fmt.Sprintf("%s: %s\nworker participant=%v run=%v task=%v was created and its task is kept; "+
			"fix the cause, then run `piggery agent resume <worker>`", ce.Code, ce.Message,
			d["participant_id"], d["run_id"], d["task_id"])
	}
	s := ce.Code + ": " + ce.Message
	if ce.RuleID != "" {
		s += " rule_id=" + ce.RuleID
	}
	if ce.Layer != "" {
		s += " layer=" + ce.Layer
	}
	if ce.Details != nil {
		b, _ := json.Marshal(ce.Details)
		s += "\ndetails: " + string(b)
	}
	return s
}

// flags returns a FlagSet carrying the global flags, so they are accepted anywhere.
func (e *env) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&e.admin, "admin", e.admin, "use admin.token")
	fs.BoolVar(&e.admin, "a", e.admin, "short for --admin")
	fs.BoolVar(&e.json, "json", e.json, "print raw JSON result")
	fs.BoolVar(&e.noStart, "no-start", e.noStart, "fail if the daemon is not running instead of starting it")
	return fs
}

// parse parses flags interleaved with positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", errUsage, fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// connect dials the daemon (starting it if needed) with the explicitly chosen auth.
// admin.token is read only after the daemon is up, because the daemon creates it on first start.
func (e *env) connect() (*Client, error) {
	if e.noStart {
		c, err := Dial(e.dir, false)
		if err != nil {
			return nil, errors.New("the piggery daemon is not running")
		}
		c.Close()
	}
	return e.dial(!e.noStart)
}

func (e *env) dial(autostart bool) (*Client, error) {
	var id, ptok string
	if !e.admin {
		id, ptok = os.Getenv("PIGGERY_ID"), os.Getenv("PIGGERY_TOKEN")
		if id == "" || ptok == "" {
			return nil, errors.New("PIGGERY_ID and PIGGERY_TOKEN must be set (or use --admin for admin commands)")
		}
	}
	c, err := Dial(e.dir, autostart)
	if err != nil {
		return nil, err
	}
	if !e.admin {
		c.AsParticipant(id, ptok)
		return c, nil
	}
	b, err := os.ReadFile(server.AdminTokenPath(e.dir))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("--admin: read admin token: %w", err)
	}
	c.AsAdmin(strings.TrimSpace(string(b)))
	return c, nil
}

// do calls verb and prints either the raw JSON or print(decoded result).
func do[T any](e *env, verb string, args any, print func(io.Writer, T)) error {
	c, err := e.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	var out T
	raw, err := c.CallInto(verb, args, &out)
	if err != nil {
		return err
	}
	if e.json {
		fmt.Fprintln(e.stdout, string(raw))
		return nil
	}
	print(e.stdout, out)
	return nil
}

func simple[T any](e *env, name string, args []string, verb string, print func(io.Writer, T)) error {
	pos, err := parse(e.flags(name), args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%w: %s takes no arguments", errUsage, name)
	}
	return do(e, verb, nil, print)
}

func absDir(d string) (string, error) {
	if d == "" {
		d = "."
	}
	return filepath.Abs(d)
}

func (e *env) teamUp(args []string) error {
	fs := e.flags("team up")
	cwd := fs.String("cwd", ".", "project dir")
	name := fs.String("name", "", "team name (default: model name)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: team up <template|path.yaml>", errUsage)
	}
	dir, err := absDir(*cwd)
	if err != nil {
		return err
	}
	// A path is read as is; a template name is ~/.piggery/templates/<name> (the daemon unpacks
	// the built-ins there when it starts, so connect first).
	var manifest string
	if ext := filepath.Ext(pos[0]); ext == ".yaml" || ext == ".yml" {
		manifest, err = manifests.Inline(pos[0])
	} else {
		var c *Client
		if c, err = e.connect(); err != nil {
			return err
		}
		c.Close()
		manifest, err = manifests.Resolve(pos[0], e.dir)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no template %q; templates: %s (piggery template list)", pos[0], e.templateNames())
		}
	}
	if err != nil {
		return err
	}
	return do(e, proto.VerbTeamUp, core.TeamUpArgs{Name: *name, Manifest: manifest, Cwd: dir},
		func(w io.Writer, t core.Team) {
			tpl := ""
			if s := view.ShownTemplate(t.Name, t.Template); s != "" {
				tpl = " [" + s + "]"
			}
			fmt.Fprintf(w, "team %s%s up in %s\n", t.Name, tpl, t.RootCwd)
			for _, warn := range t.Warnings {
				fmt.Fprintf(w, "warning: %s\n", warn)
			}
		})
}

func (e *env) join(args []string) error {
	fs := e.flags("join")
	team := fs.String("team", "", "team id or name")
	role := fs.String("role", "", "role in the manifest")
	name := fs.String("name", "", "participant name")
	cwd := fs.String("cwd", ".", "participant dir")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *team == "" || *role == "" || *name == "" {
		return fmt.Errorf("%w: join --team T --role R --name N", errUsage)
	}
	dir, err := absDir(*cwd)
	if err != nil {
		return err
	}
	return do(e, proto.VerbJoin, core.JoinArgs{Team: *team, Role: *role, Name: *name, Cwd: dir},
		func(w io.Writer, r core.JoinResult) {
			fmt.Fprintf(w, "export PIGGERY_ID=%s\nexport PIGGERY_TOKEN=%s\n", r.ID, r.Token)
		})
}

func (e *env) logCmd(args []string) error {
	fs := e.flags("log")
	var a core.LogArgs
	fs.Int64Var(&a.After, "after", 0, "events with seq > after")
	fs.StringVar(&a.Team, "team", "", "team id")
	fs.IntVar(&a.Limit, "limit", 0, "max events")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%w: log takes no arguments", errUsage)
	}
	return do(e, proto.VerbLog, a, func(w io.Writer, evs []core.Event) { e.printEvents(evs) })
}

func (e *env) printEvents(evs []core.Event) {
	var lines []string
	for _, ev := range evs {
		if e.json {
			b, _ := json.Marshal(ev)
			fmt.Fprintln(e.stdout, string(b))
			continue
		}
		line := fmt.Sprintf("%d %s %-10s", ev.Seq, clock(ev.Ts), ev.Type)
		for _, f := range [][2]string{{"participant", ev.Participant}, {"run", ev.RunID}, {"ref", ev.RefID}} {
			if f[1] != "" {
				line += " " + f[0] + "=" + f[1]
			}
		}
		lines = append(lines, line+" "+string(ev.Payload))
	}
	for _, l := range e.relabel(lines) {
		fmt.Fprintln(e.stdout, l)
	}
}

func (e *env) send(args []string) error {
	fs := e.flags("send")
	var a core.SendArgs
	fs.StringVar(&a.Kind, "kind", "", "opaque kind")
	fs.StringVar(&a.ReplyTo, "reply-to", "", "the #N (or id) of the message answered")
	fs.StringVar(&a.ClientMsgID, "client-msg-id", "", "idempotency key")
	fs.StringVar(&a.Op, "op", "", "assign (a task for a member that reports to you) | board: replace|remove")
	fs.StringVar(&a.Target, "target", "", "board: pin id")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("%w: send <to> [body]", errUsage)
	}
	a.To = pos[0]
	if len(pos) == 2 {
		a.Body = pos[1]
	}
	return do(e, proto.VerbSend, a, func(w io.Writer, r core.SendResult) {
		dup := ""
		if r.Duplicate {
			dup = " (duplicate: existing message)"
		}
		if r.Held {
			dup += fmt.Sprintf(" HELD by %s (not delivered; an admin can `piggery release #%d`)", r.RuleID, r.Seq)
		}
		fmt.Fprintf(w, "sent #%d%s\n", r.Seq, dup)
	})
}

func (e *env) why(args []string) error {
	fs := e.flags("why")
	team := fs.String("team", "", "team of <from> when its name is in several teams")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("%w: why <from> <to>", errUsage)
	}
	return do(e, proto.VerbWhy, core.WhyArgs{From: pos[0], To: pos[1], Team: *team}, func(w io.Writer, r core.WhyResult) {
		printVerdict(w, r.WhyVerdict)
	})
}

// printVerdict prints one path through the send gate: a line per check, then the verdict.
func printVerdict(w io.Writer, v core.WhyVerdict) {
	for _, c := range v.Checks {
		rule := ""
		if c.RuleID != "" {
			rule = " [" + c.RuleID + "]"
		}
		fmt.Fprintf(w, "%-10s %-5s %s%s\n", c.Check, c.Result, c.Detail, rule)
	}
	switch v.Verdict {
	case "deny":
		fmt.Fprintf(w, "verdict: deny rule_id=%s layer=%s\n", v.RuleID, v.Layer)
	case "hold":
		fmt.Fprintf(w, "verdict: hold rule_id=%s\n", v.RuleID)
	default:
		fmt.Fprintf(w, "verdict: %s\n", v.Verdict)
	}
}

func (e *env) doctor(args []string) error {
	pos, err := parse(e.flags("doctor"), args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%w: doctor takes no arguments", errUsage)
	}
	c, err := e.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	var r core.DoctorResult
	raw, err := c.CallInto(proto.VerbDoctor, nil, &r)
	if err != nil {
		return err
	}
	switch {
	case e.json:
		fmt.Fprintln(e.stdout, string(raw))
	}
	if !e.json {
		var lines []string
		for _, f := range r.Findings {
			lines = append(lines, fmt.Sprintf("%-20s %s  ids=%s", f.Kind, f.Detail, strings.Join(f.IDs, ",")))
		}
		for _, l := range e.relabel(lines) {
			fmt.Fprintln(e.stdout, l)
		}
		// Harnesses set up wrong or at an untested version: warnings, not findings (exit unchanged).
		warnings := harnessWarnings(e.dir)
		if len(r.Findings) == 0 && len(warnings) == 0 {
			fmt.Fprintln(e.stdout, "no findings")
		}
		for _, w := range warnings {
			fmt.Fprintln(e.stdout, "warning: "+w)
		}
	}
	if len(r.Findings) > 0 {
		return errFindings
	}
	return nil
}

func (e *env) release(args []string) error {
	pos, err := parse(e.flags("release"), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: release <msg_id>", errUsage)
	}
	return do(e, proto.VerbRelease, core.ReleaseArgs{ID: pos[0]}, func(w io.Writer, _ json.RawMessage) {
		fmt.Fprintf(w, "released %s\n", pos[0])
	})
}

func (e *env) inbox(args []string) error {
	fs := e.flags("inbox")
	batch := fs.Int64("batch", 0, "batch number (omit to pull, not ackable)")
	var a core.InboxArgs
	fs.StringVar(&a.View, "view", "", "read-only view: board")
	fs.IntVar(&a.Limit, "limit", 0, "with --view: max messages (default 50)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%w: inbox takes no arguments", errUsage)
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "batch" {
			a.Batch = batch
		}
	})
	return do(e, proto.VerbInbox, a, func(w io.Writer, ds []core.Delivered) {
		if len(ds) == 0 {
			fmt.Fprintln(w, "no messages")
			return
		}
		for _, d := range ds {
			if d.DeliveryID != 0 { // views record no delivery
				fmt.Fprintf(w, "[delivery %d] ", d.DeliveryID)
			}
			fmt.Fprintln(w, header(d.Message))
			fmt.Fprintln(w, indent(d.Body))
		}
	})
}

func (e *env) completion(args []string) error {
	fs := e.flags("completion")
	batch := fs.Int64("batch", 0, "batch number")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "batch" })
	if len(pos) != 0 || !set {
		return fmt.Errorf("%w: completion --batch N", errUsage)
	}
	return do(e, proto.VerbCompletion, core.CompletionArgs{Batch: *batch}, func(w io.Writer, r core.CompletionResult) {
		fmt.Fprintf(w, "acked %d\n", len(r.Acked))
		for _, id := range r.Acked {
			fmt.Fprintln(w, "  "+id)
		}
	})
}

func (e *env) watchAdd(args []string) error {
	fs := e.flags("watch add")
	to := fs.String("to", "", "participant")
	in := fs.Duration("in", 0, "first fire after")
	every := fs.Duration("every", 0, "repeat interval")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *to == "" || *in <= 0 {
		return fmt.Errorf("%w: watch add --to X --in 5m [--every D] <body>", errUsage)
	}
	a := core.TimerArgs{To: *to, InMs: in.Milliseconds(), EveryMs: every.Milliseconds(), Body: pos[0]}
	return do(e, proto.VerbWatchAdd, a, func(w io.Writer, t core.Timer) { printTimers(w, []core.Timer{t}) })
}

func (e *env) agent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: agent spawn|stop|resume|tail", errUsage)
	}
	action, rest := args[0], args[1:]
	fs := e.flags("agent " + action)
	a := core.AgentArgs{Action: action}
	switch action {
	case core.AgentSpawn:
		fs.StringVar(&a.Role, "role", "", "worker role")
		fs.StringVar(&a.Template, "template", "", "call up a taskforce from this template instead of a worker role")
		fs.StringVar(&a.Name, "name", "", "worker name (with --template: the taskforce's team name, default the template's)")
		fs.StringVar(&a.Cwd, "cwd", "", "the worker's or taskforce's directory, relative to yours or absolute (a worker: your role needs can_set_cwd)")
	case core.AgentTail:
		fs.IntVar(&a.Lines, "lines", 20, "records to show")
	case core.AgentStop, core.AgentResume, core.AgentTemplates, core.AgentClose, core.AgentReopen:
	default:
		return fmt.Errorf("%w: agent spawn|stop|resume|tail|templates|close|reopen", errUsage)
	}
	pos, err := parse(fs, rest)
	if err != nil {
		return err
	}
	if action == core.AgentReopen {
		if len(pos) != 1 {
			return fmt.Errorf("%w: agent reopen <team>", errUsage)
		}
		a.Team = pos[0]
		return do(e, proto.VerbAgent, a, func(w io.Writer, r core.AgentResult) { fmt.Fprintln(w, r.Text) })
	}
	if action == core.AgentTemplates || action == core.AgentClose {
		if len(pos) != 0 && !(action == core.AgentClose && len(pos) == 1) {
			return fmt.Errorf("%w: agent %s takes no arguments", errUsage, action)
		}
		if action == core.AgentClose && len(pos) == 1 { // a taskforce this participant called up
			a.Team = pos[0]
		}
		return do(e, proto.VerbAgent, a, func(w io.Writer, r core.AgentResult) {
			if action == core.AgentTemplates {
				fmt.Fprintln(w, r.Text)
				return
			}
			if a.Team != "" {
				fmt.Fprintf(w, "closed taskforce %s; stopped: %s\n", r.TeamName, strings.Join(r.Stopped, ", "))
				return
			}
			fmt.Fprintf(w, "closed team %s; stopped: %s\n", r.TeamName, strings.Join(r.Stopped, ", "))
			for _, f := range r.Failed {
				fmt.Fprintf(w, "not stopped: %s\n", f)
			}
			fmt.Fprintln(w, "this participant is out of the team; its session joins again as a solo")
		})
	}
	withTask := action == core.AgentResume && len(pos) == 2
	if (len(pos) != 1 && !withTask) || (action == core.AgentSpawn && !(a.Role != "" && a.Name != "" && a.Template == "") && !(a.Template != "" && a.Role == "")) {
		switch action {
		case core.AgentSpawn:
			return fmt.Errorf("%w: agent spawn --role R --name N <task> | agent spawn --template T [--name N] <task>", errUsage)
		case core.AgentResume:
			return fmt.Errorf("%w: agent resume <worker> [task]", errUsage)
		}
		return fmt.Errorf("%w: agent %s <worker>", errUsage, action)
	}
	if action == core.AgentSpawn {
		a.Task = pos[0]
	} else {
		a.Target = pos[0]
	}
	if withTask {
		a.Task = pos[1]
	}
	return do(e, proto.VerbAgent, a, func(w io.Writer, r core.AgentResult) {
		switch action {
		case core.AgentSpawn:
			if a.Template != "" {
				fmt.Fprintf(w, "called up taskforce %s; its task is #%d; write to it as %s\n", r.TeamName, r.TaskSeq, r.TeamName)
				return
			}
			fmt.Fprintf(w, "spawned %s; its task is #%d\n", a.Name, r.TaskSeq)
		case core.AgentResume:
			if r.TaskSeq != 0 {
				fmt.Fprintf(w, "resumed %s; its task is #%d\n", a.Target, r.TaskSeq)
			} else {
				fmt.Fprintf(w, "resumed %s\n", a.Target)
			}
		case core.AgentStop:
			if r.Exit == nil {
				fmt.Fprintf(w, "stopped %s\n", a.Target)
			} else if r.Exit.Signal != "" {
				fmt.Fprintf(w, "stopped %s (%s)\n", a.Target, r.Exit.Signal)
			} else {
				fmt.Fprintf(w, "stopped %s (exit %d)\n", a.Target, r.Exit.Code)
			}
		case core.AgentTail:
			for _, rec := range r.Records {
				fmt.Fprintln(w, string(rec))
			}
		}
	})
}

// ---- printers ----

func header(m core.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d from=%q", m.Seq, m.FromLabel)
	if m.Kind != "" {
		fmt.Fprintf(&b, " kind=%s", m.Kind)
	}
	if m.ReplyTo != "" {
		fmt.Fprintf(&b, " reply_to=#%d", m.ReplyToSeq)
	}
	fmt.Fprintf(&b, " at=%s", clock(m.CreatedAt))
	return b.String()
}

// printWho lists names, which are addresses; an id is printed only to tell apart two listed
// members or solos with the same name (as the pi tool does).
func printWho(w io.Writer, ps []core.Presence) {
	seen := map[string]int{}
	for _, p := range ps {
		if p.Kind != core.WhoTeam {
			seen[p.Name]++
		}
	}
	id := func(p core.Presence) string {
		if seen[p.Name] > 1 {
			return " id=" + p.ID
		}
		return ""
	}
	for _, p := range ps {
		switch p.Kind {
		case core.WhoTeam:
			fmt.Fprintf(w, "team   %-16s gate=%s root=%s\n", p.Name, p.GateName, p.Cwd)
		case core.WhoSolo:
			admit := ""
			if p.Admittable {
				admit = " admittable"
			}
			fmt.Fprintf(w, "solo   %-16s %-20s cwd=%s%s%s\n", p.Name, p.State, p.Cwd, admit, id(p))
		default:
			gate := ""
			if p.Gate {
				gate = " gate"
			}
			fmt.Fprintf(w, "member %-16s %-10s %-20s since=%s%s%s\n", p.Name, p.Role, p.State, clock(p.StateSince), gate, id(p))
		}
	}
}

func printBoard(w io.Writer, ms []core.Message) {
	if len(ms) == 0 {
		fmt.Fprintln(w, "board is empty")
		return
	}
	for _, m := range ms {
		fmt.Fprintf(w, "[pin #%d] from=%q at=%s\n%s\n", m.Seq, m.FromLabel, clock(m.CreatedAt), indent(m.Body))
	}
}

func printTimers(w io.Writer, ts []core.Timer) {
	for _, t := range ts {
		every := ""
		if t.EveryMs > 0 {
			every = " every=" + (time.Duration(t.EveryMs) * time.Millisecond).String()
		}
		fmt.Fprintf(w, "timer %s to=%s fire_at=%s%s active=%v: %s\n", t.ID, t.Target, clock(t.FireAt), every, t.Active, t.Body)
	}
}

func clock(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

func indent(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") }

func (e *env) teamDown(args []string) error {
	pos, err := parse(e.flags("team down"), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: team down <team>", errUsage)
	}
	return do(e, proto.VerbTeamDown, core.TeamDownArgs{Team: pos[0]}, func(w io.Writer, r core.TeamDownResult) {
		fmt.Fprintf(w, "team %s closed; workers stopped: %d %v\n", r.TeamID, len(r.Stopped), r.Stopped)
		for _, f := range r.Failed {
			fmt.Fprintf(w, "  stop failed (run team down again to retry): %s\n", f)
		}
	})
}

func (e *env) gc(args []string) error {
	fs := e.flags("gc")
	before := fs.Duration("closed-before", 0, "teams closed, and solo sessions gone, longer ago than this (e.g. 168h)")
	dry := fs.Bool("dry-run", false, "print the counts, write and delete nothing")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "closed-before" })
	if len(pos) != 0 || !set {
		return fmt.Errorf("%w: gc --closed-before DURATION [--dry-run]", errUsage)
	}
	return do(e, proto.VerbGC, core.GCArgs{ClosedBeforeMs: before.Milliseconds(), DryRun: *dry},
		func(w io.Writer, r core.GCResult) {
			if len(r.Teams)+len(r.Solos) == 0 {
				fmt.Fprintln(w, "no closed team or gone solo session is old enough")
			}
			for _, g := range append(r.Teams, r.Solos...) {
				state := "dry run"
				switch {
				case g.Skipped != "":
					state = "SKIPPED: " + g.Skipped
				case g.Deleted:
					state = "archived and deleted -> " + g.Archive
				}
				if g.ParticipantID != "" {
					fmt.Fprintf(w, "solo %s name=%s gone=%s %s\n", g.ParticipantID, g.Name, clock(g.ClosedAt), state)
				} else {
					fmt.Fprintf(w, "team %s name=%s closed=%s %s\n", g.TeamID, g.Name, clock(g.ClosedAt), state)
				}
				if g.Counts != nil {
					fmt.Fprintf(w, "  %s\n", fmtCounts(g.Counts))
				}
				if g.LogDirs > 0 {
					fmt.Fprintf(w, "  run logs, scratch and session data deleted: %d entries, %s\n", g.LogDirs, view.Tokens(int(g.LogBytes))+"B")
				}
			}
			if n := len(r.ExpiredArchives); n > 0 {
				fmt.Fprintf(w, "expired archives deleted: %d (%s)\n", n, strings.Join(r.ExpiredArchives, ", "))
			}
		})
}

func (e *env) archiveShow(args []string) error {
	fs := e.flags("archive show")
	table := fs.String("table", "", "print this table's rows (JSON lines)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: archive show <file> [--table T]", errUsage)
	}
	lines, err := core.ReadArchive(pos[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%s: %d rows\n  %s\n", pos[0], len(lines), fmtCounts(core.CountArchive(lines)))
	for _, l := range lines {
		if l.Table == *table {
			b, err := json.Marshal(l.Row)
			if err != nil {
				return err
			}
			fmt.Fprintln(e.stdout, string(b))
		}
	}
	return nil
}

func fmtCounts(c map[string]int) string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, c[k])
	}
	return strings.Join(parts, " ")
}
