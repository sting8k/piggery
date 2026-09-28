package cli

import "github.com/sting8k/piggery/internal/core"

// harnessProfile is everything the adapter layer knows about one harness: setup, hook, piggery mcp
// and doctor read it from harnesses and never branch on a harness name. A new harness is one file
// defining its profile plus one line below.
type harnessProfile struct {
	setupTarget
	// profilePath is its worker profile (~/.piggery/harness/<name>.json), which may name another
	// cmd and the versions piggery was tested with.
	profilePath func(dir string) string
	// hookEvent maps a hook to a standard event (false: not reported) and hookOutput is what the
	// harness reads back from it; nil for a harness without hooks (pi: its extension talks to the
	// daemon itself). A harness with hooks is a session host: its process owns its session.
	hookEvent  func(hook string, in claudeHookInput) (core.HarnessEventArgs, bool)
	hookOutput func(hook string, r core.HarnessEventResult) any
	// wake is how piggery mcp wakes an idle session of this harness for new mail (ref: the wake's
	// ref), and channel the mail-channel part of its instructions; nil/"" without piggery mcp.
	wake    func(s *mcpServer, ref string)
	channel string
}

// setupTarget is one place `piggery setup` adds piggery to: a harness, or a host that only shows
// piggery (Paseo). install, remove and status: `piggery setup <name>`, `setup remove <name>`,
// `setup` alone; cmd is the command it needs on PATH.
type setupTarget struct {
	name, cmd string
	install   func(o setupOpts) (string, error)
	remove    func(o setupOpts) (string, error)
	status    func(o setupOpts) harnessState
}

// setupOpts: dir is piggery's (~/.piggery), self this executable, ext --ext (a checkout's pi
// extension, "" for the binary's own) and paseoHome --paseo-home ("" for Paseo's default).
type setupOpts struct{ dir, self, ext, paseoHome string }

// harnesses is the registry, in the order setup reports them.
var harnesses = []harnessProfile{piHarness, claudeHarness, codexHarness}

// setupTargets are the harnesses and the other places setup installs to, in the order it reports them.
var setupTargets = []setupTarget{piHarness.setupTarget, claudeHarness.setupTarget, codexHarness.setupTarget, paseoTarget}

// targetNamed is the setup target called name.
func targetNamed(name string) (setupTarget, bool) {
	for _, t := range setupTargets {
		if t.name == name {
			return t, true
		}
	}
	return setupTarget{}, false
}

// harnessNamed is the registered harness called name.
func harnessNamed(name string) (harnessProfile, bool) {
	for _, h := range harnesses {
		if h.name == name {
			return h, true
		}
	}
	return harnessProfile{}, false
}

// hostHarnesses are the names of the harnesses whose process hosts a session (those with hooks).
func hostHarnesses() []string {
	var names []string
	for _, h := range harnesses {
		if h.hookEvent != nil {
			names = append(names, h.name)
		}
	}
	return names
}
