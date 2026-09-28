package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
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
	what := modelLabel(a.Model, a.Thinking)
	return do(e, proto.VerbModel, a, func(w io.Writer, r core.ModelResult) {
		if r.Live {
			fmt.Fprintf(w, "%s now runs %s (from its next turn)\n", a.Target, what)
		} else {
			fmt.Fprintf(w, "%s will run %s when resumed\n", a.Target, what)
		}
	})
}

// modelLabel is a model with its thinking level: "HP/x · high", "HP/x", "thinking high", "-".
// modelID is a model without its provider (what comes before the first "/"; the id itself may
// hold "/": CPAv2/devin/swe-2 is devin/swe-2), for the model column; "-" unknown.
func modelID(model string) string {
	if model == "" {
		return "-"
	}
	if _, id, ok := strings.Cut(model, "/"); ok {
		return id
	}
	return model
}

func modelLabel(model, thinking string) string {
	switch {
	case model != "" && thinking != "":
		return model + " · " + thinking
	case thinking != "":
		return "thinking " + thinking
	case model != "":
		return model
	}
	return "-"
}
