package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// WithTemplates sets how found turns a template name into a self-contained manifest for a
// normalized cwd. An error wrapping fs.ErrNotExist means no such template.
func WithTemplates(f func(name, cwd string) (string, error)) Option {
	return func(e *Engine) { e.templates = f }
}

// found is `agent action=found`: the caller founds a team from a.Template (default p2p), rooted at
// its cwd and named after the cwd's base name (-2, -3… while an open team has it), and becomes its
// first member (the gate) as the template's auto_join_role or only role. A solo moves into the
// team: same participant, run and token (Token is empty: keep yours, identify again with the same
// run). A member leaves its team (leave.go) and gets a new participant in the new team, with a new
// run and token.
func (e *Engine) found(ctx context.Context, c Caller, a AgentArgs) (AgentResult, error) {
	if a.Template == "" {
		a.Template = "p2p"
	}
	var cwd, prefix string
	err := e.readOnly(ctx, func(t *txn) error {
		p, err := t.foundCaller(c)
		if err != nil {
			return err
		}
		prefix = p.toolPrefix
		return internal(t.QueryRowContext(t.ctx, `SELECT cwd FROM participants WHERE id=?`, p.id).Scan(&cwd))
	})
	if err != nil {
		return AgentResult{}, err
	}
	if e.templates == nil {
		return AgentResult{}, errf(CodeNotFound, "no template %q", a.Template)
	}
	text, err := e.templates(a.Template, cwd)
	if errors.Is(err, fs.ErrNotExist) {
		return AgentResult{}, errf(CodeNotFound, "no template %q in ~/.piggery/templates (%sagent action=templates lists them)", a.Template, prefix)
	}
	if err != nil {
		return AgentResult{}, errf(CodeInvalid, "template %s: %v", a.Template, err)
	}
	m, warnings, err := loadManifest(text)
	if err != nil {
		return AgentResult{}, err
	}
	role := m.sessionRole()
	if role == "" {
		return AgentResult{}, &Error{Code: CodeInvalid, RuleID: "found.no_role",
			Message: fmt.Sprintf("template %s has several roles and no auto_join_role", a.Template),
			Details: map[string]any{"template": a.Template}}
	}
	token, err := newToken()
	if err != nil {
		return AgentResult{}, internal(err)
	}
	var res AgentResult
	var wake, notice string
	err = e.inTx(ctx, func(t *txn) error {
		p, err := t.foundCaller(c)
		if err != nil {
			return err
		}
		name, err := t.freeTeamName(filepath.Base(cwd))
		if err != nil {
			return err
		}
		team, err := t.insertTeam(name, text, m, cwd)
		if err != nil {
			return err
		}
		if p.team == "" { // a solo moves in
			if _, err := t.ExecContext(t.ctx, `UPDATE participants SET team_id=?, role=?, joined_at=? WHERE id=?`,
				team.ID, role, t.now, p.id); err != nil {
				return internal(err)
			}
			res = AgentResult{ParticipantID: p.id, RunID: p.run, TeamID: team.ID, TeamName: name}
			return t.gateIfNone(team.ID, p.id) // the founder is the gate
		}
		// A member: a new participant for the same session in the new team, then leave.
		res = AgentResult{ParticipantID: newID(t.now), RunID: newID(t.now), Token: token, TeamID: team.ID, TeamName: name}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO participants
			(id, run_id, kind, harness, mode, name, cwd, team_id, role, state, state_since, last_activity,
			 token_hash, harness_ref, created_at, person)
			SELECT ?, ?, kind, harness, mode, name, cwd, ?, ?, 'idle', ?, ?, ?, harness_ref, ?, 1
			FROM participants WHERE id=?`,
			res.ParticipantID, res.RunID, team.ID, role, t.now, t.now, hashToken(token), t.now, p.id); err != nil {
			return internal(err)
		}
		if err := t.moveSession(p.id, res.ParticipantID); err != nil {
			return err
		}
		if err := t.gateIfNone(team.ID, res.ParticipantID); err != nil { // the founder is the gate
			return err
		}
		wake, notice, err = t.leave(p, team.ID)
		return err
	})
	if err != nil {
		return AgentResult{}, err
	}
	if wake != "" {
		e.notifyAfterCommit(wake)
	}
	if notice != "" {
		e.notifyHookAfterCommit(notice)
	}
	res.Warnings = warnings
	return res, nil
}

// foundCaller is the gate of found: a caller with the agent tool that is a solo or a member,
// never a headless worker.
func (t *txn) foundCaller(c Caller) (participant, error) {
	p, err := t.callerGranted(c, "agent.found", "agent")
	if err != nil {
		return p, err
	}
	var mode string
	if err := t.QueryRowContext(t.ctx, `SELECT COALESCE(mode,'') FROM participants WHERE id=?`, p.id).Scan(&mode); err != nil {
		return p, internal(err)
	}
	if mode == modeHeadless {
		return p, errf(CodeInvalid, "a headless worker does not found a team")
	}
	return p, nil
}

// freeTeamName returns name, or name-2, name-3, … while an open team has it.
func (t *txn) freeTeamName(name string) (string, error) {
	for i := 1; ; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d", name, i)
		}
		var n int
		if err := t.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM teams WHERE name=? AND closed_at IS NULL`,
			cand).Scan(&n); err != nil {
			return "", internal(err)
		}
		if n == 0 {
			return cand, nil
		}
	}
}

// TemplateRef is a template the templates action lists: its name and where it was found.
type TemplateRef struct{ Name, From string }

// WithTemplateList sets how the templates action lists templates for a cwd: found's lookup
// order, a name only once (the first). Each is then read through WithTemplates.
func WithTemplateList(f func(cwd string) ([]TemplateRef, error)) Option {
	return func(e *Engine) { e.templateList = f }
}

type TemplateInfo struct {
	Name    string `json:"name"`
	From    string `json:"from"` // its directory
	Summary string `json:"summary,omitempty"`
	// Taskforce: the template has a taskforce: block, so a solo or a gate may call it up with spawn template=.
	Taskforce bool       `json:"taskforce,omitempty"`
	Roles     []RoleInfo `json:"roles,omitempty"`
	Error     string     `json:"error,omitempty"` // the template could not be read
}

type RoleInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// listTemplates is the templates action: what found could use from the caller's cwd, with each
// template's summary and roles. Read-only.
func (e *Engine) listTemplates(ctx context.Context, c Caller) (AgentResult, error) {
	var cwd, prefix string
	err := e.readOnly(ctx, func(t *txn) error {
		p, err := t.callerGranted(c, "agent.templates", "agent")
		if err != nil {
			return err
		}
		prefix = p.toolPrefix
		return internal(t.QueryRowContext(t.ctx, `SELECT cwd FROM participants WHERE id=?`, p.id).Scan(&cwd))
	})
	if err != nil {
		return AgentResult{}, err
	}
	if e.templateList == nil || e.templates == nil {
		return AgentResult{}, errf(CodeUnsupported, "no templates configured")
	}
	refs, err := e.templateList(cwd)
	if err != nil {
		return AgentResult{}, internal(err)
	}
	var res AgentResult
	var b strings.Builder
	b.WriteString("Templates (in ~/.piggery/templates; the Human edits and adds them there):\n")
	for _, r := range refs {
		info := TemplateInfo{Name: r.Name, From: r.From}
		m, err := e.readTemplate(r.Name, cwd)
		if err != nil {
			info.Error = err.Error()
			fmt.Fprintf(&b, "- %s (%s): unreadable: %s\n", r.Name, r.From, info.Error)
			res.Templates = append(res.Templates, info)
			continue
		}
		info.Summary = m.Summary
		info.Taskforce = m.Taskforce != nil
		tag := ""
		if m.Taskforce != nil {
			tag = " [taskforce: spawn template=" + r.Name + "]"
		}
		fmt.Fprintf(&b, "- %s (%s)%s: %s\n", r.Name, r.From, tag, orNone(m.Summary))
		roles := make([]string, 0, len(m.Roles))
		for name := range m.Roles {
			roles = append(roles, name)
		}
		sort.Strings(roles)
		for _, name := range roles {
			d := m.Roles[name].Description
			info.Roles = append(info.Roles, RoleInfo{Name: name, Description: d})
			fmt.Fprintf(&b, "    %s: %s\n", name, orNone(d))
		}
		res.Templates = append(res.Templates, info)
	}
	fmt.Fprintf(&b, "Start a team from one with %sagent action=found template=<name>.", prefix)
	res.Text = b.String()
	return res, nil
}

func (e *Engine) readTemplate(name, cwd string) (manifest, error) {
	text, err := e.templates(name, cwd)
	if err != nil {
		return manifest{}, err
	}
	m, _, err := loadManifest(text)
	return m, err
}

func orNone(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return "(no description)"
	}
	return s
}
