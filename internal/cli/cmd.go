package cli

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"charm.land/fang/v2"
	"github.com/spf13/cobra"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
)

// The CLI is for humans first: `piggery --help` lists the human commands grouped
// by task; the participant (agent) commands are hidden but run. Every command hands its raw
// arguments to its handler, which parses its own flags (-a/--admin and --json anywhere).

// Help groups.
const (
	grpStart    = "start"
	grpTeams    = "teams"
	grpWatch    = "watch"
	grpStepIn   = "stepin"
	grpMaintain = "maintain"
)

// How a command authenticates.
type auth int

const (
	authLocal       auth = iota // no daemon verb (setup, template, archive)
	authAdmin                   // an admin verb
	authParticipant             // a participant verb (hidden from help)
)

// Main runs one CLI command against the daemon in dir and returns the exit code.
func Main(dir string, args []string, stdout, stderr io.Writer) int {
	e := &env{dir: dir, stdout: stdout}
	root := e.root()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := fang.Execute(context.Background(), root,
		fang.WithoutCompletions(), fang.WithoutManpage(), fang.WithVersion(Version),
		fang.WithErrorHandler(e.printError))
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errFindings):
		return 1
	case errors.As(err, new(*core.Error)):
		return 1
	case errors.Is(err, errUsage) || e.cur == nil:
		return 2 // a usage mistake, or one cobra found (unknown command or flag)
	default:
		return 1
	}
}

// printError prints err styled (plain when not a terminal or NO_COLOR); a usage mistake is
// followed by the usage of that command only.
func (e *env) printError(w io.Writer, st fang.Styles, err error) {
	var ce *core.Error
	switch {
	case errors.Is(err, errFindings):
		return
	case errors.As(err, &ce) && e.json:
		b, _ := json.Marshal(ce)
		fmt.Fprintln(w, string(b))
		return
	case errors.As(err, &ce):
		msg := formatErr(ce)
		if s := e.suggestion(ce); s != "" {
			msg += "\n" + s
		}
		err = errors.New(msg)
	}
	msg := err.Error()
	if errors.Is(err, errUsage) && e.cur != nil {
		// A handler's "usage: <its usage line>" says no more than the usage printed below.
		msg = strings.TrimPrefix(msg, errUsage.Error()+": ")
		if strings.Contains(fullUseLine(e.cur), msg) {
			msg = "missing or wrong arguments"
		}
	}
	fmt.Fprintln(w, st.ErrorHeader.String())
	fmt.Fprintln(w, st.ErrorText.UnsetWidth().UnsetTransform().Render(msg))
	fmt.Fprintln(w)
	if errors.Is(err, errUsage) && e.cur != nil {
		fmt.Fprintln(w, usageOf(e.cur))
		if e.cur.CommandPath() == "piggery team up" {
			fmt.Fprintf(w, "\ntemplates: %s (piggery template list)\n", e.templateNames())
		}
	}
}

// usageOf is a command's usage line and examples.
func usageOf(c *cobra.Command) string {
	s := "usage: " + fullUseLine(c)
	if c.Example != "" {
		s += "\n\nexamples:\n" + c.Example
	}
	return s
}

func (e *env) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "piggery",
		Short: "🐷 The farm where your agents meet, team up, and get things done.",
		Long: "🐷 The farm where your agents meet, team up, and get things done.\n" +
			"Agents use it through their harness; these commands are for you.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          nil, // cobra's default: an unknown command gets "did you mean"
		RunE: func(c *cobra.Command, args []string) error {
			e.welcome()
			return nil
		},
	}
	// Declared for help and so cobra can find the command after them; each handler parses them.
	root.PersistentFlags().BoolP("admin", "a", false, "run as admin (admin.token)")
	root.PersistentFlags().Bool("json", false, "print the raw JSON result")
	root.PersistentFlags().Bool("no-start", false, "do not start the daemon; fail if it is down")
	root.AddGroup(
		&cobra.Group{ID: grpStart, Title: "Get started"},
		&cobra.Group{ID: grpTeams, Title: "Teams and templates"},
		&cobra.Group{ID: grpWatch, Title: "Watch"},
		&cobra.Group{ID: grpStepIn, Title: "Step in"},
		&cobra.Group{ID: grpMaintain, Title: "Maintain"},
	)

	root.SetHelpCommandGroupID(grpStart)
	team := &cobra.Command{Use: "team", Short: "Close a team", GroupID: grpTeams}
	// team up still works (the private flow and the tests use it) but is not the user's way: a team
	// made from a shell has no member, and a session joins only by founding or by being admitted.
	up := e.cmd("up <template|path.yaml> [--cwd D] [--name N]", "Start a team from a template in ~/.piggery/templates", "",
		"piggery team up lead-peer --cwd .\npiggery team up ./team.yaml --cwd . --name demo", authAdmin, e.teamUp)
	up.Hidden = true
	team.AddCommand(
		up,
		e.cmd("down <team>", "Close a team: stop its workers; nothing is acked", "",
			"piggery team down demo", authAdmin, e.teamDown),
	)
	template := &cobra.Command{Use: "template", Short: "Make your own team template", GroupID: grpTeams}
	template.AddCommand(e.cmd("list", "The templates a team can be founded from, with their summaries", "",
		"piggery template list", authLocal, e.templateList))
	template.AddCommand(e.cmd("new <name> [--from <built-in>]", "Copy a built-in (default p2p) to ~/.piggery/templates/<name>", "",
		"piggery template new review --from lead-peer", authLocal,
		func(args []string) error { return e.template(append([]string{"new"}, args...)) }))
	archive := &cobra.Command{Use: "archive", Short: "Read a gc archive", GroupID: grpMaintain}
	archive.AddCommand(e.cmd("show <file> [--table T]", "Print a gc archive (local, no daemon)", "",
		"piggery archive show ~/.piggery/archive/demo-1790000000.jsonl --table messages", authLocal, e.archiveShow))

	root.AddCommand(
		e.cmd("setup [--outdated] | setup <pi|claude|codex|omp|dsh|opencode|paseo> | setup remove <harness> | setup notify [add|remove <desktop|herdr|ntfy:TOPIC>] [--ext PATH] [--force]", "Add piggery to a harness or take it out; alone: profiles, templates, and where each harness stands; --outdated: update every installed integration that is outdated", grpStart,
			"piggery setup", authLocal, e.setup),
		e.cmd("skills", "Print the guide for agents (a SKILL.md)", grpStart,
			"piggery skills", authLocal, func(args []string) error {
				if len(args) != 0 {
					return fmt.Errorf("%w: skills takes no arguments", errUsage)
				}
				fmt.Fprint(e.stdout, skillsMD)
				return nil
			}),
		team, template,
		e.cmd("ps [--json|--view]", "Daemon, teams, members, solos and pending mail, once", grpWatch,
			"piggery ps", authAdmin, e.ps),
		e.cmd("top", "The same, live, with the latest events and a worker's tail", grpWatch,
			"piggery top", authAdmin, e.top),
		e.cmd("tail <worker> [-n N] [-f|--view] [--team T]", "A worker's rpc log, readable (--json raw)", grpWatch,
			"piggery tail w1 -n 50 -f", authAdmin, e.tail),
		e.cmd("log [--after SEQ] [--team T] [--limit N]", "Decisions and lifecycle events", grpWatch,
			"piggery log --after 100 --limit 50", authAdmin, e.logCmd),
		e.cmd("abort <x> [--team T]", "Cancel x's current turn; it stays alive", grpStepIn,
			"piggery abort w1", authAdmin, e.abort),
		alias(e.cmd("kill <worker> [--team T]", "Kill a headless worker's process group now (SIGKILL); x is the short form", grpStepIn,
			"piggery kill w1\npiggery x w1", authAdmin, e.killCmd), "x"),
		e.cmd("resume <worker> [--team T]", "Start a stopped worker again in its session (its model, thinking, harness)", grpStepIn,
			"piggery resume w1", authAdmin, e.resumeCmd),
		e.cmd("model <worker> [<provider/model>] [--thinking L] [--team T]", "Switch a worker's model or thinking level now, or at its resume", grpStepIn,
			"piggery model w1 HP/kimi-k3 --thinking high", authAdmin, e.model),
		e.cmd("release <msg_id>", "Deliver a message held by a limit", grpStepIn,
			"piggery release 01M3FQZWT33PEDH9Z3TVX0YGV6", authAdmin, e.release),
		e.cmd("why <from> <to> [--team T]", "Every gate check for a send, nothing sent", grpStepIn,
			"piggery why w1 lead --team demo", authAdmin, e.why),
		e.cmd("shutdown", "Stop the daemon: workers stopped, exits recorded", grpMaintain,
			"piggery shutdown", authAdmin, e.shutdown),
		e.cmd("restart", "Shutdown, then start the daemon again from this binary", grpMaintain,
			"piggery restart", authAdmin, e.restart),
		e.cmd("gc --closed-before DURATION [--dry-run]", "Archive, verify and delete closed teams", grpMaintain,
			"piggery gc --closed-before 168h --dry-run", authAdmin, e.gc),
		archive,
		e.cmd("doctor", "Findings about the daemon's state (exit 1 when any)", grpMaintain,
			"piggery doctor", authAdmin, e.doctor),
		e.cmd("check", "Errors and ignored lines in config.yaml, harness profiles, templates and prompts (local; exit 1 when any error)", grpMaintain,
			"piggery check", authLocal, e.check),
		e.cmd("update [--check] [--force]", "Install the latest release (a running daemon restarts on next use)", grpMaintain,
			"piggery update --check\npiggery update", authLocal, e.update),
		e.cmd("stop", "", "", "", authLocal, func([]string) error {
			return fmt.Errorf("%w: no admin stop: abort a turn, kill a worker, shutdown the daemon", errUsage)
		}),

		// Participant (agent) commands and join: hidden, still run.
		e.cmd("join --team T --role R --name N [--cwd D]", "", "", "", authAdmin, e.join),
		e.cmd("send <to> [body] [--kind K] [--reply-to ID] [--client-msg-id ID] [--op assign|replace|remove] [--target ID]",
			"", "", "", authParticipant, e.send),
		e.cmd("inbox [--batch N] [--view V]", "", "", "", authParticipant, e.inbox),
		e.cmd("completion --batch N", "", "", "", authParticipant, e.completion),
		e.cmd("who", "", "", "", authParticipant, func(a []string) error { return simple(e, "who", a, proto.VerbWho, printWho) }),
		e.cmd("board", "", "", "", authParticipant, func(a []string) error { return simple(e, "board", a, proto.VerbBoard, printBoard) }),
		e.cmd("watch add|list", "", "", "", authParticipant, e.watch),
		e.cmd("agent spawn|stop|resume|tail|templates|close|reopen", "", "", "", authParticipant, e.agent),
		// The Claude Code adapter: Claude runs these, never a person.
		e.cmd("mcp", "", "", "", authLocal, e.mcp),
		e.cmd("hook claude <HookEvent>", "", "", "", authLocal, e.hook),
	)
	shortRootForms(root)
	return root
}

// rootForm is how a command shows in `piggery --help`: one short line each, so a row fits 80
// columns (fang pads every row to the longest usage and does not wrap). The command's own
// --help and its usage errors keep the full form and the full description.
type rootForm struct{ use, short string }

var rootForms = map[string]rootForm{
	"setup":    {"setup [<harness>]", "Set up piggery for a harness; alone: status"},
	"template": {"template [command]", "List and copy team templates"},
	"log":      {"log [--team T]", "Decisions and lifecycle events"},
	"ps":       {"ps", "Daemon, teams, members and mail, once"},
	"top":      {"top", "Live ps, with events and a worker's tail"},
	"tail":     {"tail <worker> [-f]", "A worker's rpc log, readable"},
	"abort":    {"abort <x>", "Cancel x's current turn; it stays alive"},
	"kill":     {"kill <worker>", "Kill a headless worker now (alias: x)"},
	"model":    {"model <worker> [model]", "Switch a worker's model or thinking"},
	"resume":   {"resume <worker>", "Start a stopped worker again"},
	"why":      {"why <from> <to>", "Every gate check for a send, nothing sent"},
	"check":    {"check", "Check config, profiles, templates, prompts"},
	"doctor":   {"doctor", "Findings about the daemon's state"},
	"gc":       {"gc --closed-before D", "Archive, verify and delete closed teams"},
	"restart":  {"restart", "Stop the daemon, then start it again"},
	"shutdown": {"shutdown", "Stop the daemon and its workers"},
	"update":   {"update [--check]", "Install the latest release"},
}

// shortRootForms gives each listed root command its short form and keeps what that drops: the
// full usage in Annotations (usage errors print it) and in the command's own help, over the
// full description.
func shortRootForms(root *cobra.Command) {
	for _, c := range root.Commands() {
		f, ok := rootForms[c.Name()]
		if !ok {
			continue
		}
		full, long := c.Use, cmp.Or(c.Long, c.Short)
		c.Annotations = map[string]string{"usage": full}
		c.Long = long + "\n\nusage: piggery " + full
		c.Use, c.Short = f.use, f.short
	}
}

// fullUseLine is a command's usage line with its full form (see shortRootForms).
func fullUseLine(c *cobra.Command) string {
	if full := c.Annotations["usage"]; full != "" {
		return c.Parent().CommandPath() + " " + full
	}
	return c.UseLine()
}

//go:embed skills.md
var skillsMD string

// welcome is `piggery` with no command: whether the daemon runs (never starting it), how many
// teams are open, and where to go next.
func (e *env) welcome() {
	state := "the farm is asleep (it wakes with the first command that needs it)"
	if c, err := Dial(e.dir, false); err == nil {
		c.Close()
		state = "the farm is up"
		e.admin = true
		if c, err := e.dial(false); err == nil {
			var r proto.PsResult
			if _, err := c.CallInto(proto.VerbPs, core.StateArgs{}, &r); err == nil {
				state = fmt.Sprintf("the farm is up (pid %d): %d open team(s), %d solo session(s)", r.PID, len(r.Teams), len(r.Solos))
			}
			c.Close()
		}
	}
	fmt.Fprintf(e.stdout, `🐷 The farm where your agents meet, team up, and get things done.

%s

  piggery setup      first time: worker profile and templates
  piggery ps         what is running now
  piggery top        watch it live

piggery --help lists every command.
`, state)
}

// cmd is one command whose handler parses its own arguments. A command with no summary is
// hidden from help.
func (e *env) cmd(use, short, group, example string, a auth, run func([]string) error) *cobra.Command {
	return &cobra.Command{
		Use:                   use,
		Short:                 short,
		GroupID:               group,
		Example:               example,
		Hidden:                short == "",
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
		RunE: func(c *cobra.Command, args []string) error {
			if slices.Contains(args, "-h") || slices.Contains(args, "--help") {
				return c.Help()
			}
			e.cur = c
			if a == authAdmin && !adminFlag(args) {
				// An admin-only verb needs no -a, but a participant's shell is never
				// turned into admin silently.
				if os.Getenv("PIGGERY_ID") != "" || os.Getenv("PIGGERY_TOKEN") != "" {
					return fmt.Errorf("%w: %s is an admin command and PIGGERY_ID/PIGGERY_TOKEN are set "+
						"(a participant's shell): add -a to run it as admin", errUsage, c.CommandPath())
				}
				e.admin = true
			}
			return run(args)
		},
	}
}

// adminFlag reports whether args ask for admin explicitly (-a or --admin, before any "--").
func adminFlag(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "-a", "--admin", "-a=true", "--admin=true":
			return true
		}
	}
	return false
}

func (e *env) watch(args []string) error {
	if len(args) > 0 && args[0] == "add" {
		return e.watchAdd(args[1:])
	}
	if len(args) > 0 && args[0] == "list" {
		return simple(e, "watch list", args[1:], proto.VerbWatchList, printTimers)
	}
	return fmt.Errorf("%w: watch add|list", errUsage)
}

// alias gives c another name.
func alias(c *cobra.Command, names ...string) *cobra.Command {
	c.Aliases = names
	return c
}
