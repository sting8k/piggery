package core

import (
	"cmp"
	"fmt"
	"regexp"
)

// Manifest roles v1: instructions inlined at team up, per-role spawn model, routing cc.

// builtinTools are the harness tools every role may list.
var builtinTools = map[string]bool{"send": true, "inbox": true, "who": true, "agent": true}

// toolPlaceholder is {tool:<name>} in instructions: the reader's real name of tool <name>, so
// prompts do not name a harness.
var toolPlaceholder = regexp.MustCompile(`\{tool:([^{}]*)\}`)

// WithToolNames replaces each {tool:<name>} in text with prefix+name (also the adapters' texts of
// the built-in tools, extensions/pi/tools.json).
func WithToolNames(text, prefix string) string {
	return toolPlaceholder.ReplaceAllStringFunc(text, func(ph string) string {
		return prefix + toolPlaceholder.FindStringSubmatch(ph)[1]
	})
}

// validateRoles checks the roles v1 parts of a manifest being brought up.
func validateRoles(m manifest) error {
	if _, ok := m.Roles[m.AutoJoinRole]; m.AutoJoinRole != "" && !ok {
		return errf(CodeInvalid, "manifest: auto_join_role: unknown role %q", m.AutoJoinRole)
	}
	for name, r := range m.Roles {
		if r.InstructionsFile != "" {
			return errf(CodeInvalid, "manifest: roles.%s.instructions_file must be inlined into instructions before team up", name)
		}
		for _, ph := range toolPlaceholder.FindAllStringSubmatch(r.Instructions, -1) {
			if !builtinTools[ph[1]] {
				return errf(CodeInvalid, "manifest: roles.%s.instructions: %s names no tool (send, inbox, who or agent)", name, ph[0])
			}
		}
		for _, tool := range r.Tools {
			if !builtinTools[tool] {
				return errf(CodeInvalid, "manifest: roles.%s.tools: unknown tool %q (send, inbox, who or agent)", name, tool)
			}
		}
	}
	for i, r := range m.Routing {
		for _, cc := range r.CC {
			if _, ok := m.Roles[cc]; !ok {
				return errf(CodeInvalid, "manifest: routing[%d].cc: unknown role %q", i, cc)
			}
		}
	}
	return nil
}

// workerSettings resolves a worker's harness (harness given for a resume, else the role's
// spawn.harness, else the nearest non-headless ancestor's when a driver runs it, else the default),
// then its model and thinking level, each through its own chain: the role's spawn field, else that
// harness's profile, else what the ancestor session reports when it runs the same harness, else ""
// (the harness default). Values pass through as written.
func (e *Engine) workerSettings(t *txn, m manifest, role, spawner, harness string) (h, model, thinking string, err error) {
	// The nearest ancestor that is a person's session: the session a human opened that started the chain.
	var anc struct{ harness, model, thinking string }
	for id, hops := spawner, 0; id != "" && hops < 64; hops++ {
		var person bool
		var up string
		if err := t.QueryRowContext(t.ctx, `SELECT person, COALESCE(harness,''), COALESCE(session_model,''),
			COALESCE(session_thinking,''), COALESCE(spawned_by,'') FROM participants WHERE id=?`, id).Scan(
			&person, &anc.harness, &anc.model, &anc.thinking, &up); err != nil {
			return "", "", "", internal(err)
		}
		if person {
			break
		}
		anc = struct{ harness, model, thinking string }{}
		id = up
	}
	// Harness: the worker's own (resume), the role's, the main session's when a driver runs it,
	// else the default.
	h = cmp.Or(harness, m.Roles[role].Spawn.Harness)
	if h == "" {
		h = e.defaultHarnessName()
		if e.driverNamed(anc.harness) != nil {
			h = anc.harness
		}
	}
	d := e.runtimeFor(h)
	if d == nil {
		return "", "", "", errf(CodeInvalid, "role %s: no runtime driver for harness %q", role, h)
	}
	_, model, thinking = d.Defaults()
	model = cmp.Or(m.Roles[role].Spawn.Model, model)
	thinking = cmp.Or(m.Roles[role].Spawn.Thinking, thinking)
	if anc.harness == h { // model names are the harness's own: inherit only within it
		model, thinking = cmp.Or(model, anc.model), cmp.Or(thinking, anc.thinking)
	}
	return h, model, thinking, nil
}

// Model verbs (send, who, agent, inbox with a view) need the tool in the
// caller's role; driver verbs (inbox pull/batch, completion, presence, identify) do not.

// granted denies p's verb unless tool is in p's role tools.
func (m manifest) granted(p participant, verb, tool string) error {
	if contains(m.Roles[p.role].Tools, tool) {
		return nil
	}
	return deny(&p, verb, "permission", "tools.not_granted", fmt.Sprintf("role %s has no tool %q", p.role, tool), nil,
		map[string]any{"tool": tool})
}

// callerGranted is caller (gate layer 1) plus the role's grant of tool.
func (t *txn) callerGranted(c Caller, verb, tool string) (participant, error) {
	p, err := t.caller(c, verb)
	if err != nil {
		return p, err
	}
	m, err := t.teamManifest(p.team)
	if err != nil {
		return p, err
	}
	return p, m.granted(p, verb, tool)
}
