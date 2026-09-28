package core

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// Manifest roles v1: instructions inlined at team up, per-role spawn model, declarative tools
// ("send a message of kind K to X"), routing cc.

// builtinTools are the harness tools every role may list.
var builtinTools = map[string]bool{"send": true, "inbox": true, "who": true, "agent": true}

// toolSpec is a declarative tool: calling it sends one message of kind Send.Kind, whose body is
// the JSON of the arguments, to Send.To (spawned_by, reports_to, or a role).
type toolSpec struct {
	Description string            `yaml:"description"`
	Params      map[string]string `yaml:"params"` // field -> "string"; all required
	Send        struct {
		Kind string `yaml:"kind"`
		To   string `yaml:"to"`
	} `yaml:"send"`
}

// ToolSpec is a declarative tool as the harness registers it (identify).
type ToolSpec struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Params      map[string]string `json:"params"`
}

// ToolArgs is the `tool` verb.
type ToolArgs struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

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
			if _, ok := m.Tools[ph[1]]; !ok && !builtinTools[ph[1]] {
				return errf(CodeInvalid, "manifest: roles.%s.instructions: %s names no tool (neither built-in nor declared)", name, ph[0])
			}
		}
		for _, tool := range r.Tools {
			if _, ok := m.Tools[tool]; !ok && !builtinTools[tool] {
				return errf(CodeInvalid, "manifest: roles.%s.tools: unknown tool %q", name, tool)
			}
		}
	}
	for name, t := range m.Tools {
		if builtinTools[name] {
			return errf(CodeInvalid, "manifest: tools.%s shadows a built-in tool", name)
		}
		for field, typ := range t.Params {
			if typ != "string" {
				return errf(CodeInvalid, "manifest: tools.%s.params.%s: only string is supported", name, field)
			}
		}
		if t.Send.Kind == "" {
			return errf(CodeInvalid, "manifest: tools.%s.send.kind is required", name)
		}
		if _, ok := m.Roles[t.Send.To]; !ok && t.Send.To != "spawned_by" && t.Send.To != "reports_to" {
			return errf(CodeInvalid, "manifest: tools.%s.send.to must be spawned_by, reports_to, or a role", name)
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

// toolSpecs returns the declarative tools granted to role, sorted by name.
func (m manifest) toolSpecs(role string) []ToolSpec {
	out := []ToolSpec{}
	for _, name := range m.Roles[role].Tools {
		if t, ok := m.Tools[name]; ok {
			out = append(out, ToolSpec{Name: name, Description: t.Description, Params: t.Params})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// workerSettings resolves a worker's harness (harness given for a resume, else the role's
// spawn.harness, else the nearest non-headless ancestor's when a driver runs it, else the default),
// then its model and thinking level, each through its own chain: the role's spawn field, else that
// harness's profile, else what the ancestor session reports when it runs the same harness, else ""
// (the harness default). Values pass through as written.
func (e *Engine) workerSettings(t *txn, m manifest, role, spawner, harness string) (h, model, thinking string, err error) {
	// The nearest non-headless ancestor: the session a human opened that started the chain.
	var anc struct{ harness, model, thinking string }
	for id, hops := spawner, 0; id != "" && hops < 64; hops++ {
		var mode, up string
		if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(mode,''), COALESCE(harness,''), COALESCE(session_model,''),
			COALESCE(session_thinking,''), COALESCE(spawned_by,'') FROM participants WHERE id=?`, id).Scan(
			&mode, &anc.harness, &anc.model, &anc.thinking, &up); err != nil {
			return "", "", "", internal(err)
		}
		if mode != modeHeadless {
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

// Model verbs (send, who, agent, inbox with a view, declarative tools) need the tool in the
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

// Tool runs a declarative tool of the caller's role: it checks the grant and the arguments,
// resolves the target, then sends through the normal Send path (gate, limits, events).
func (e *Engine) Tool(ctx context.Context, c Caller, a ToolArgs) (SendResult, error) {
	var send SendArgs
	err := e.inTx(ctx, func(t *txn) error {
		p, err := t.caller(c, "tool")
		if err != nil {
			return err
		}
		m, err := t.teamManifest(p.team)
		if err != nil {
			return err
		}
		spec, declared := m.Tools[a.Name]
		if !declared || !contains(m.Roles[p.role].Tools, a.Name) {
			return deny(&p, "tool", "permission", "tools.not_granted", fmt.Sprintf("role %s has no tool %q", p.role, a.Name), nil,
				map[string]any{"tool": a.Name})
		}
		for field := range spec.Params {
			if _, ok := a.Args[field].(string); !ok {
				return errf(CodeInvalid, "tool %s: argument %q (string) is required", a.Name, field)
			}
		}
		for field := range a.Args {
			if _, ok := spec.Params[field]; !ok {
				return errf(CodeInvalid, "tool %s: unknown argument %q", a.Name, field)
			}
		}
		to, err := t.toolTarget(p, spec.Send.To)
		if err != nil {
			return err
		}
		body, err := json.Marshal(a.Args)
		if err != nil {
			return internal(err)
		}
		send = SendArgs{To: to, Kind: spec.Send.Kind, Body: string(body)}
		return nil
	})
	if err != nil {
		return SendResult{}, err
	}
	return e.send(ctx, c, send, a.Name)
}

// toolTarget resolves a declarative tool's send.to for caller p to a participant id.
func (t *txn) toolTarget(p participant, to string) (string, error) {
	if to == "spawned_by" || to == "reports_to" { // validated names, safe as a column
		var id sql.NullString
		if err := t.QueryRowContext(t.ctx, `SELECT `+to+` FROM participants WHERE id=?`, p.id).Scan(&id); err != nil {
			return "", internal(err)
		}
		if !id.Valid || id.String == p.id {
			// E.g. a worker that became its team's gate when its lead left: it reports to no one.
			return "", deny(&p, "tool", "target", "tool.no_"+to,
				fmt.Sprintf("you report to no one (no %s): there is no one for this tool to send to", to), nil,
				map[string]any{"to": to})
		}
		return id.String, nil
	}
	members, err := t.teamRole(p.team, to)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, q := range members {
		if q.id != p.id {
			ids = append(ids, q.id)
		}
	}
	if len(ids) != 1 {
		return "", errf(CodeInvalid, "%d other participants have role %s; the tool needs exactly one", len(ids), to)
	}
	return ids[0], nil
}
