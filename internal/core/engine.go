package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"
	"gopkg.in/yaml.v3"
)

// txn is one SQLite transaction plus the request's timestamp.
type txn struct {
	*sql.Tx
	ctx  context.Context
	now  int64    // unix ms
	solo manifest // the implicit manifest of a solo (teamManifest(""))
}

// evt is one row for the events table.
type evt struct {
	typ, participant, team, run, ref string
	payload                          any
}

// denial is a gate refusal: the request's work is rolled back, then ev is committed.
type denial struct {
	err *Error
	ev  evt
}

func (d *denial) Error() string { return d.err.Error() }

// inTx runs fn in one transaction. A denial rolls back fn's work and commits only its
// denied event, so a refused request never leaves partial state.
func (e *Engine) inTx(ctx context.Context, fn func(t *txn) error) error {
	now := e.now().UnixMilli()
	run := func(fn func(t *txn) error) error {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			return internal(err)
		}
		defer tx.Rollback()
		if err := fn(&txn{Tx: tx, ctx: ctx, now: now, solo: e.soloManifest()}); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return internal(err)
		}
		return nil
	}
	err := run(fn)
	var d *denial
	if !errors.As(err, &d) {
		return err
	}
	if werr := run(func(t *txn) error { return t.event(d.ev) }); werr != nil {
		return werr
	}
	return d.err
}

// notifyAfterCommit tells the notify hook about new mail for a participant. Call only after
// the message's transaction committed.
// A participant in a turn the daemon counts (adapter events) is not woken: the turn gets the
// mail at its next tool call or at its end, and a wake now would queue a turn of its own. One
// waiting on a permission prompt is not woken either; the wake comes when the prompt ends. With
// no mail waiting there is nothing to wake for.
func (e *Engine) notifyAfterCommit(participantID string) {
	var hold int
	if err := e.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM batches b WHERE b.run_id=p.run_id AND b.prompt_id IS NOT NULL
			AND b.ended_at IS NULL AND b.completed_at IS NULL)
		+ (p.state='awaiting_permission')
		+ NOT EXISTS (SELECT 1 FROM messages m WHERE m.to_id=p.id AND m.acked_at IS NULL AND m.held_reason IS NULL)
		FROM participants p WHERE p.id=?`, participantID).Scan(&hold); err == nil && hold > 0 {
		return
	}
	var mode, harness sql.NullString
	e.db.QueryRow(`SELECT mode, harness FROM participants WHERE id=?`, participantID).Scan(&mode, &harness)
	if mode.String == modeHeadless {
		if d := e.runtimeFor(harness.String); d != nil {
			if delivers(d) {
				e.deliver(participantID, d)
				return
			}
			if err := d.Wake(participantID); !errors.Is(err, ErrNoWake) {
				return // the driver woke it, or it is not running (nothing to wake)
			}
		}
	}
	if e.notify != nil {
		e.notify(participantID)
	}
}

func (t *txn) event(ev evt) error {
	p := ev.payload
	if p == nil {
		p = struct{}{}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return internal(err)
	}
	_, err = t.ExecContext(t.ctx,
		`INSERT INTO events(ts, type, participant, team_id, run_id, ref_id, payload) VALUES (?,?,?,?,?,?,?)`,
		t.now, ev.typ, nullStr(ev.participant), nullStr(ev.team), nullStr(ev.run), nullStr(ev.ref), string(b))
	return internal(err)
}

// deny builds a gate refusal for participant p (may be nil) with its denied event.
func deny(p *participant, verb, layer, rule, reason string, details any, extra map[string]any) error {
	code := CodeDenied
	if layer == "token" {
		code = CodeUnauthorized
	}
	return denyAs(code, p, verb, layer, rule, reason, details, extra)
}

// denyAs is deny with an explicit wire code (e.g. batch_closed) for refusals whose code
// is part of the client contract.
func denyAs(code string, p *participant, verb, layer, rule, reason string, details any, extra map[string]any) error {
	payload := map[string]any{"verb": verb, "rule_id": rule, "layer": layer, "reason": reason}
	for k, v := range extra {
		payload[k] = v
	}
	ev := evt{typ: "denied", payload: payload}
	if p != nil {
		ev.participant, ev.team, ev.run = p.id, p.team, p.run
	}
	return &denial{
		err: &Error{Code: code, Message: reason, RuleID: rule, Layer: layer, Details: details},
		ev:  ev,
	}
}

func errf(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// internal wraps a non-contract error; nil and *Error/denial pass through.
func internal(err error) error {
	if err == nil {
		return nil
	}
	var ce *Error
	var d *denial
	if errors.As(err, &ce) || errors.As(err, &d) {
		return err
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func newID(now int64) string {
	return ulid.MustNew(uint64(now), ulid.DefaultEntropy()).String()
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// normalizeCwd: absolute, symlinks resolved, no trailing '/'.
func normalizeCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", errf(CodeInvalid, "cwd is required")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", errf(CodeInvalid, "cwd: %v", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errf(CodeInvalid, "cwd: %v", err)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", errf(CodeInvalid, "cwd is not a directory: %s", cwd)
	}
	return filepath.Clean(real), nil
}

func isReserved(name string) bool {
	switch strings.ToLower(name) {
	case AddrNotify, AddrBoard, AddrEngine:
		return true
	}
	return false
}

// ---- manifest ----

type manifest struct {
	Model   string              `yaml:"model"`
	Summary string              `yaml:"summary"` // one line: when to use the template (templates action)
	Roles   map[string]roleSpec `yaml:"roles"`
	Routing []routeRule         `yaml:"routing"`
	Limits  limitMap            `yaml:"limits"`
	Tools   map[string]toolSpec `yaml:"tools"` // declarative tools (roles.go)
	// AutoJoinRole is the role join.auto gives a session in a team with several roles.
	AutoJoinRole string `yaml:"auto_join_role"`
}

type roleSpec struct {
	Description      string   `yaml:"description"` // one line: what the role does (templates action)
	Instructions     string   `yaml:"instructions"`
	InstructionsFile string   `yaml:"instructions_file"` // must be inlined before team up
	Tools            []string `yaml:"tools"`
	CanSpawn         []string `yaml:"can_spawn"`
	CanPin           bool     `yaml:"can_pin"`
	CanSetCwd        bool     `yaml:"can_set_cwd"` // spawn with a cwd other than the spawner's
	Spawn            struct {
		Harness  string `yaml:"harness"` // the worker's harness; "" or inherit = the main session's
		Model    string `yaml:"model"`
		Thinking string `yaml:"thinking"` // passed to the harness as written
		// AllowTools names native tools the harness profile disallows that this role's workers
		// keep, e.g. Agent.
		AllowTools []string `yaml:"allow_tools"`
	} `yaml:"spawn"`
}

type routeRule struct {
	From  string   `yaml:"from"`
	To    string   `yaml:"to"`
	Allow bool     `yaml:"allow"`
	CC    []string `yaml:"cc"`
}

func parseManifest(text string) (manifest, error) {
	var m manifest
	if err := yaml.Unmarshal([]byte(text), &m); err != nil {
		return m, errf(CodeInvalid, "manifest: %v", err)
	}
	if m.Model == "" {
		return m, errf(CodeInvalid, "manifest: model is required")
	}
	if len(m.Roles) == 0 {
		return m, errf(CodeInvalid, "manifest: at least one role is required")
	}
	// inherit is written out in the built-in templates: the same as leaving it empty.
	for name, r := range m.Roles {
		for _, v := range []*string{&r.Spawn.Harness, &r.Spawn.Model, &r.Spawn.Thinking} {
			if *v == inherit {
				*v = ""
			}
		}
		m.Roles[name] = r
	}
	return m, nil
}

// inherit is the written value of a spawn setting that follows the chain.
const inherit = "inherit"

// limitMap is the manifest's limits: a number each, or none (no limit, as if the key were not
// written). A key set to none is left out, so it is never enforced.
type limitMap map[string]int

func (l *limitMap) UnmarshalYAML(n *yaml.Node) error {
	var raw map[string]yaml.Node
	if err := n.Decode(&raw); err != nil {
		return err
	}
	out := limitMap{}
	for k, v := range raw {
		if v.Kind == yaml.ScalarNode && v.Value == "none" {
			if !knownLimits[k] { // the known-limits check below never sees a key left out
				return fmt.Errorf("limits.%s: unknown limit", k)
			}
			continue
		}
		var x int
		if err := v.Decode(&x); err != nil {
			return fmt.Errorf("limits.%s: want a number or none, not %q", k, v.Value)
		}
		out[k] = x
	}
	*l = out
	return nil
}

// route returns the id of the first routing rule matching (from, to) and whether it allows.
// No matching rule denies.
func (m manifest) route(from, to string) (rule string, allow bool) {
	for i, r := range m.Routing {
		if r.From == from && r.To == to {
			return fmt.Sprintf("routing[%d]", i), r.Allow
		}
	}
	return "routing.no_rule", false
}

func (t *txn) teamManifest(teamID string) (manifest, error) {
	if teamID == "" {
		return t.solo, nil
	}
	var text string
	if err := t.QueryRowContext(t.ctx, `SELECT manifest FROM teams WHERE id=?`, teamID).Scan(&text); err != nil {
		return manifest{}, internal(err)
	}
	return parseManifest(text)
}

// ---- participants ----

type participant struct {
	id, run, team, role, name, kind, reportsTo string
	state                                      string
	stateSince, lastActivity                   int64
	toolPrefix                                 string // how its driver names piggery's tools for the model
	harness                                    string // what it runs ("" unknown)
}

const participantCols = `id, run_id, COALESCE(team_id,''), COALESCE(role,''), name, kind, COALESCE(reports_to,''), state, state_since, last_activity, COALESCE(tool_prefix,''), COALESCE(harness,'')`

// scanParticipant scans participantCols, then any extra selected columns.
func scanParticipant(row interface{ Scan(...any) error }, extra ...any) (participant, error) {
	var p participant
	dest := append([]any{&p.id, &p.run, &p.team, &p.role, &p.name, &p.kind, &p.reportsTo, &p.state, &p.stateSince, &p.lastActivity, &p.toolPrefix, &p.harness}, extra...)
	err := row.Scan(dest...)
	return p, err
}

// participantByID returns (p, false, nil) when the id does not exist.
func (t *txn) participantByID(id string) (participant, bool, error) {
	p, err := scanParticipant(t.QueryRowContext(t.ctx, `SELECT `+participantCols+` FROM participants WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, internal(err)
}

// caller is gate layer 1 inside the request's transaction: the participant exists and
// c.RunID is still its current run.
func (t *txn) caller(c Caller, verb string) (participant, error) {
	p, ok, err := t.participantByID(c.ParticipantID)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, deny(nil, verb, "token", "token.invalid", "unknown participant", nil,
			map[string]any{"claimed_id": c.ParticipantID})
	}
	if p.run != c.RunID {
		return p, deny(&p, verb, "token", "run.stale", "run is not the participant's current run", nil,
			map[string]any{"stale_run": c.RunID})
	}
	return p, nil
}

// resolveInView resolves a participant name or id visible to p (step 1: same team).
func (t *txn) resolveInView(p participant, to, verb string) (participant, error) {
	q, err := scanParticipant(t.QueryRowContext(t.ctx,
		`SELECT `+participantCols+` FROM participants WHERE team_id=? AND (id=? OR name=?)`, p.team, to, to))
	if errors.Is(err, sql.ErrNoRows) {
		return q, deny(&p, verb, "visibility", "visibility.unknown_target",
			fmt.Sprintf("no participant %q in view", to), nil, map[string]any{"to": to})
	}
	return q, internal(err)
}

// label is the engine-stamped sender header as seen by reader.
func label(reader, sender participant) string {
	switch {
	case sender.id == AddrEngine:
		return sender.id
	case sender.team == "":
		return sender.name + " (solo)"
	case reader.reportsTo != "" && reader.reportsTo == sender.id:
		return sender.name + " (your lead)"
	case sender.reportsTo != "" && sender.reportsTo == reader.id:
		return sender.name + " (your report)"
	case sender.role == reader.role:
		return sender.name + " (your peer)"
	default:
		return sender.name + " (" + sender.role + ")"
	}
}
