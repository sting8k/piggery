package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/x/term"
	"gopkg.in/yaml.v3"

	"github.com/sting8k/piggery/manifests"
)

// listedTemplate is a template of the home as `template list` and the unknown-name error show it.
type listedTemplate struct {
	name, summary, origin string
	taskforce             bool // it has a top-level taskforce: block: a solo or a gate can call it up with spawn template=
}

// templatesOf reads the templates of the home the way team up and the templates action do
// (manifests.List, then each through manifests.Resolve), with each manifest's summary.
func (e *env) templatesOf() ([]listedTemplate, error) {
	ls, err := manifests.List(e.dir)
	if err != nil {
		return nil, err
	}
	builtin := manifests.Builtins()
	read := func(name string) (string, error) { return manifests.Resolve(name, e.dir) }
	if len(ls) == 0 { // before the first setup or daemon start: the ones that will be unpacked
		for _, n := range builtin {
			ls = append(ls, manifests.Listed{Name: n})
		}
		read = manifests.Builtin
	}
	out := make([]listedTemplate, 0, len(ls))
	for _, l := range ls {
		t := listedTemplate{name: l.Name, origin: "yours"}
		if slices.Contains(builtin, l.Name) {
			t.origin = "built-in"
		}
		var m struct {
			Summary   string    `yaml:"summary"`
			Taskforce *struct{} `yaml:"taskforce"` // as core reads it: present with any content
		}
		if text, err := read(l.Name); err != nil {
			t.summary = "unreadable: " + err.Error()
		} else if err := yaml.Unmarshal([]byte(text), &m); err != nil {
			t.summary = "unreadable: " + err.Error()
		} else {
			t.summary, t.taskforce = strings.Join(strings.Fields(m.Summary), " "), m.Taskforce != nil
		}
		out = append(out, t)
	}
	return out, nil
}

// templateNames is `a, b, c` for the unknown-template error and the usage of team up.
func (e *env) templateNames() string {
	ts, _ := e.templatesOf()
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.name
	}
	return strings.Join(names, ", ")
}

// templateList is `piggery template list`: one row per template, name, where it is from, summary;
// a taskforce column between them when some template can be called up as one.
func (e *env) templateList(args []string) error {
	if pos, err := parse(e.flags("template list"), args); err != nil {
		return err
	} else if len(pos) != 0 {
		return fmt.Errorf("%w: template list takes no arguments", errUsage)
	}
	ts, err := e.templatesOf()
	if err != nil {
		return err
	}
	// name, origin, taskforce, summary; on a terminal the summary is cut to the width, piped it is whole
	width := 0
	if f, ok := e.stdout.(*os.File); ok && term.IsTerminal(f.Fd()) {
		width, _, _ = term.GetSize(f.Fd())
	}
	w, tf := 0, 0 // tf: the taskforce column's width with its gap, 0 when none is one
	for _, t := range ts {
		w = max(w, len(t.name))
		if t.taskforce {
			tf = len("taskforce") + 2
		}
	}
	const origin = len("built-in")
	for _, t := range ts {
		summary := t.summary
		if room := width - w - origin - tf - 4; width > 0 && room > 0 {
			summary = truncate(summary, room)
		}
		mark := ""
		if t.taskforce {
			mark = "taskforce"
		}
		fmt.Fprintf(e.stdout, "%-*s  %-*s  %-*s%s\n", w, t.name, origin, t.origin, tf, mark, summary)
	}
	return nil
}
