package cli

import (
	"fmt"
	"io"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/view"
)

// Admin quick actions on one participant: target by name or id, --team T when the
// name is in several teams.

// target parses <x> [--team T] plus n more positionals.
func (e *env) target(name string, args []string, n int, usage string) (core.AdminTarget, []string, error) {
	fs := e.flags(name)
	var a core.AdminTarget
	fs.StringVar(&a.Team, "team", "", "team, when the name is in more than one")
	pos, err := parse(fs, args)
	if err != nil {
		return a, nil, err
	}
	if len(pos) != 1+n {
		return a, nil, fmt.Errorf("%w: %s", errUsage, usage)
	}
	a.Target = pos[0]
	return a, pos[1:], nil
}

func (e *env) abort(args []string) error {
	a, _, err := e.target("abort", args, 0, "abort <x> [--team T]")
	if err != nil {
		return err
	}
	return do(e, proto.VerbAbort, a, func(w io.Writer, r core.AbortResult) {
		fmt.Fprintf(w, "aborted %s (%s)\n", a.Target, r.How)
	})
}

func (e *env) resumeCmd(args []string) error {
	a, _, err := e.target("resume", args, 0, "resume <worker> [--team T]")
	if err != nil {
		return err
	}
	return do(e, proto.VerbResume, a, func(w io.Writer, r core.AgentResult) {
		fmt.Fprintf(w, "resumed %s (run %s)\n", a.Target, r.RunID)
	})
}

func (e *env) reloadCmd(args []string) error {
	fs := e.flags("reload")
	var a core.ReloadArgs
	fs.StringVar(&a.Team, "team", "", "team, when the name is in more than one")
	fs.BoolVar(&a.Now, "now", false, "stop the turn in progress instead of waiting for its end")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: reload <worker> [--now] [--team T]", errUsage)
	}
	a.Target = pos[0]
	return do(e, proto.VerbReload, a, func(w io.Writer, r core.AgentResult) {
		if r.Pending {
			fmt.Fprintln(w, r.Text+" (--now stops the turn)")
			return
		}
		fmt.Fprintf(w, "reloaded %s (run %s)\n", a.Target, r.RunID)
	})
}

func (e *env) killCmd(args []string) error {
	a, _, err := e.target("kill", args, 0, "kill <worker> [--team T]")
	if err != nil {
		return err
	}
	return do(e, proto.VerbKill, a, func(w io.Writer, r core.AgentResult) {
		fmt.Fprintf(w, "killed %s", a.Target)
		if r.Exit != nil && r.Exit.Signal != "" {
			fmt.Fprintf(w, " (%s)", r.Exit.Signal)
		}
		fmt.Fprintln(w)
	})
}

func (e *env) model(args []string) error {
	const usage = "model <worker> [<provider/model>] [--thinking L] [--team T]"
	fs := e.flags("model")
	var a core.ModelArgs
	fs.StringVar(&a.Team, "team", "", "team, when the name is in more than one")
	fs.StringVar(&a.Thinking, "thinking", "", "thinking level, as the worker's harness names it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 || len(pos) == 1 && a.Thinking == "" {
		return fmt.Errorf("%w: %s", errUsage, usage)
	}
	a.Target = pos[0]
	if len(pos) == 2 {
		a.Model = pos[1]
	}
	what := view.ModelLabel(a.Model, a.Thinking)
	return do(e, proto.VerbModel, a, func(w io.Writer, r core.ModelResult) {
		if r.Live {
			fmt.Fprintf(w, "%s now runs %s (from its next turn)\n", a.Target, what)
		} else {
			fmt.Fprintf(w, "%s will run %s when resumed\n", a.Target, what)
		}
	})
}
