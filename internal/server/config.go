package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/driver/local"
)

// ConfigPath is the daemon's optional settings file.
func ConfigPath(dir string) string { return filepath.Join(dir, "config.yaml") }

// Settings are the daemon's settings. A duration of 0 means off.
type Settings struct {
	GCClosedAfter time.Duration // automatic gc of teams closed longer than this
	GCArchiveKeep time.Duration // gc archives are deleted after this
	Harness       string        // workers of no role or session harness; default pi
	UpdateCheck   bool          // the daily check for a newer release (a release build only)
	// Columns are the columns top and ps show after the name, in order, as written (the CLI
	// checks them: ColumnsOf). The CLI reads them on every run; the daemon does not use them.
	Columns []string
	// AllowedRoots are directories (absolute) outside a team's root where a worker may be spawned;
	// the team root and its git worktrees always are.
	AllowedRoots []string
	// Prompts are the Human's shared prompt files by role (see PromptEntry), without the entries
	// that cannot work (CheckPrompts drops more at daemon start). Warnings say what was left out:
	// the daemon logs them; a mistake there never stops it.
	Prompts  []PromptEntry
	Warnings []string
}

// DisplayColumns are the columns top and ps can show after the name, in their default order. A
// column that does not apply to a row (role and ctx for a solo, cwd for a member) is skipped.
var DisplayColumns = []string{"role", "state", "harness", "model", "ctx", "turns", "unacked", "age", "since", "cwd"}

// gcEvery is how often the daemon runs gc (and once at start, after reconcile).
const gcEvery = 24 * time.Hour

// defaultSettings are the settings with no config file, and the values setup writes.
func defaultSettings() Settings {
	return Settings{GCClosedAfter: 14 * 24 * time.Hour, GCArchiveKeep: 30 * 24 * time.Hour, Harness: local.Harness, UpdateCheck: true,
		Columns: slices.Clone(DisplayColumns), AllowedRoots: []string{}, Prompts: []PromptEntry{}}
}

// configFile is the settings file's keys (every key LoadSettings takes).
type configFile struct {
	Harness string `yaml:"harness"`
	GC      struct {
		ClosedAfter *string `yaml:"closed_after"`
		ArchiveKeep *string `yaml:"archive_keep"`
	} `yaml:"gc"`
	Display struct {
		Columns *[]string `yaml:"columns"`
	} `yaml:"display"`
	Spawn struct {
		AllowedRoots *[]string `yaml:"allowed_roots"`
	} `yaml:"spawn"`
	Update struct {
		Check *bool `yaml:"check"`
	} `yaml:"update"`
	Prompts []PromptEntry `yaml:"prompts"`
}

// LoadSettings reads ConfigPath(dir); a missing file gives the defaults. An unknown key or a
// value that is not a duration (`36h`, `14d`) or `off` is an error: the daemon does not start.
func LoadSettings(dir string) (Settings, error) {
	set := defaultSettings()
	b, err := os.ReadFile(ConfigPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return set, nil
	}
	if err != nil {
		return set, err
	}
	var raw configFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return set, fmt.Errorf("%s: %w", ConfigPath(dir), err)
	}
	if raw.Harness != "" {
		if !slices.Contains(local.Harnesses(), raw.Harness) {
			return set, fmt.Errorf("%s: harness: %q: want one of %s", ConfigPath(dir), raw.Harness, strings.Join(local.Harnesses(), ", "))
		}
		set.Harness = raw.Harness
	}
	for _, f := range []struct {
		key string
		val *string
		dst *time.Duration
	}{{"gc.closed_after", raw.GC.ClosedAfter, &set.GCClosedAfter}, {"gc.archive_keep", raw.GC.ArchiveKeep, &set.GCArchiveKeep}} {
		if f.val == nil {
			continue
		}
		d, err := parseRetention(*f.val)
		if err != nil {
			return set, fmt.Errorf("%s: %s: %q: want a duration like 14d or 36h, or off", ConfigPath(dir), f.key, *f.val)
		}
		*f.dst = d
	}
	if roots := raw.Spawn.AllowedRoots; roots != nil {
		for _, r := range *roots {
			if !filepath.IsAbs(r) {
				return set, fmt.Errorf("%s: spawn.allowed_roots: %q: want an absolute path", ConfigPath(dir), r)
			}
		}
		set.AllowedRoots = *roots
	}
	if raw.Update.Check != nil {
		set.UpdateCheck = *raw.Update.Check
	}
	if raw.Prompts != nil {
		kept, warns := checkPromptShape(ConfigPath(dir), raw.Prompts)
		set.Prompts, set.Warnings = append([]PromptEntry{}, kept...), warns
	}
	if cols := raw.Display.Columns; cols != nil { // a display setting: never stops the daemon
		set.Columns = *cols
	}
	return set, nil
}

// ColumnsOf is the columns top and ps show: set.Columns, or the default ones with a warning
// (naming the valid columns) when it names a column that does not exist.
func ColumnsOf(set Settings, dir string) (cols []string, warning string) {
	for _, c := range set.Columns {
		if !slices.Contains(DisplayColumns, c) {
			return slices.Clone(DisplayColumns), fmt.Sprintf("%s: display.columns: %q is not a column (valid: %s); showing the default columns",
				ConfigPath(dir), c, strings.Join(DisplayColumns, ", "))
		}
	}
	return set.Columns, ""
}

// configKey is one key of the settings file as setup writes it: its default and a comment.
type configKey struct{ key, value, comment string }

// configKeys are the settings file's keys with their defaults, in the order setup writes them.
func configKeys() []configKey {
	d := defaultSettings()
	return []configKey{
		{"harness", d.Harness, "harness of a worker whose role and spawner name none: " + strings.Join(local.Harnesses(), ", ")},
		{"gc.closed_after", formatRetention(d.GCClosedAfter), "gc a team closed longer than this, with its run logs (14d, 36h, or off)"},
		{"gc.archive_keep", formatRetention(d.GCArchiveKeep), "delete gc archives older than this (30d, or off)"},
		{"display.columns", "[" + strings.Join(d.Columns, ", ") + "]",
			"columns top and ps show, in order; remove one to hide it (name is always shown); read on every run, no restart"},
		{"spawn.allowed_roots", "[" + strings.Join(d.AllowedRoots, ", ") + "]",
			"absolute directories outside a team's root where a worker may be spawned with a cwd (the root and its repo's git worktrees always may)"},
		{"update.check", "true", "ask GitHub once a day whether a newer piggery release is out, and say so in top, setup and update --check; nothing is installed (a dev build never asks)"},
		{"prompts", "[]", "your prompt files by role, added to those roles' cards: - {file: rules/code.md, roles: [peer, solo, lead-peer/lead]} (file: relative to this directory or absolute; the file is read at each session start, the list needs a restart)"},
	}
}

// EnsureConfig writes ConfigPath(dir) with every key and its default when there is none;
// an existing file keeps every byte of the user's and gets only its missing keys (a comment line
// each). added names what it wrote; a key it cannot place (gc not written as an indented block)
// is left for the user, named in manual. A file LoadSettings refuses is not touched.
func EnsureConfig(dir string) (added, manual []string, err error) {
	path := ConfigPath(dir)
	cur, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var b strings.Builder
		b.WriteString("# piggery daemon settings: every key with its default. Restart the daemon after an edit.\n")
		for _, k := range configKeys() {
			added = append(added, k.key)
			top, sub, nested := strings.Cut(k.key, ".")
			switch {
			case !nested:
				fmt.Fprintf(&b, "# %s\n%s: %s\n", k.comment, k.key, k.value)
			default:
				if !strings.Contains(b.String(), "\n"+top+":\n") {
					fmt.Fprintf(&b, "%s:\n", top)
				}
				fmt.Fprintf(&b, "  # %s\n  %s: %s\n", k.comment, sub, k.value)
			}
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, err
		}
		return added, nil, os.WriteFile(path, []byte(b.String()), 0o600)
	}
	if err != nil {
		return nil, nil, err
	}
	if _, err := LoadSettings(dir); err != nil {
		return nil, nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(cur, &doc); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	var root *yaml.Node
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		root = doc.Content[0]
	}
	lines := strings.SplitAfter(string(cur), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		lines[len(lines)-1] += "\n"
	}
	var tail []string             // lines added at the end of the file
	inserts := map[int][]string{} // lines added after line n (1-based) of the file
	blocks := map[string]bool{}   // top-level blocks added at the end
	for _, k := range configKeys() {
		top, sub, nested := strings.Cut(k.key, ".")
		topNode := mapKey(root, top)
		switch {
		case !nested && topNode == nil:
			tail = append(tail, "# "+k.comment+"\n", k.key+": "+k.value+"\n")
		case !nested:
			continue
		case topNode == nil:
			if !blocks[top] {
				tail = append(tail, top+":\n")
				blocks[top] = true
			}
			tail = append(tail, "  # "+k.comment+"\n", "  "+sub+": "+k.value+"\n")
		case mapKey(topNode, sub) != nil:
			continue
		case topNode.Kind != yaml.MappingNode || topNode.Style&yaml.FlowStyle != 0 || len(topNode.Content) == 0:
			manual = append(manual, k.key)
			continue
		default: // after the block's last line, indented like its first key
			last := topNode.Content[len(topNode.Content)-1]
			indent := strings.Repeat(" ", topNode.Content[0].Column-1)
			inserts[last.Line] = append(inserts[last.Line], indent+"# "+k.comment+"\n", indent+sub+": "+k.value+"\n")
		}
		added = append(added, k.key)
	}
	if len(added) == 0 {
		return nil, manual, nil
	}
	var b strings.Builder
	for i, l := range lines {
		b.WriteString(l)
		for _, ins := range inserts[i+1] {
			b.WriteString(ins)
		}
	}
	for _, l := range tail {
		b.WriteString(l)
	}
	out := []byte(b.String())
	var check configFile // never write a file the daemon would refuse
	dec := yaml.NewDecoder(bytes.NewReader(out))
	dec.KnownFields(true)
	if err := dec.Decode(&check); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%s: cannot add %s: %w", path, strings.Join(added, ", "), err)
	}
	return added, manual, os.WriteFile(path, out, 0o600)
}

// AddPrompt appends e to config.yaml's prompts as a block item, keeping every byte of the file
// (`prompts: []` becomes a block; a missing key is added at the end). A list it cannot place an
// item in (written as a flow list with items) is left alone: added is false, for the user to add.
func AddPrompt(dir string, e PromptEntry) (added bool, err error) {
	path := ConfigPath(dir)
	cur, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(cur, &doc); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return false, nil
	}
	root := doc.Content[0]
	roles := make([]string, len(e.Roles))
	for i, r := range e.Roles {
		roles[i] = strconv.Quote(r)
	}
	item := func(indent string) string {
		return fmt.Sprintf("%s- file: %s\n%s  roles: [%s]\n", indent, e.File, indent, strings.Join(roles, ", "))
	}
	lines := strings.SplitAfter(string(cur), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		lines[len(lines)-1] += "\n"
	}
	seq, next := mapKey(root, "prompts"), 0 // next: line of the key after prompts (0: none)
	for i := 0; i+2 < len(root.Content); i += 2 {
		if root.Content[i].Value == "prompts" {
			next = root.Content[i+2].Line
		}
	}
	switch {
	case seq == nil:
		lines = append(lines, "prompts:\n"+item("  "))
	case seq.Kind != yaml.SequenceNode || seq.Style&yaml.FlowStyle != 0 && len(seq.Content) > 0:
		return false, nil
	case seq.Style&yaml.FlowStyle != 0: // prompts: [] (a comment after it stays)
		l := lines[seq.Line-1]
		end := seq.Column - 1 + strings.Index(l[seq.Column-1:], "]") + 1
		lines[seq.Line-1] = strings.TrimRight(l[:seq.Column-1], " ") + l[end:] + item("  ")
	default: // after the list's last line: before the next key and the comments over it
		at := len(lines)
		if next > 0 {
			at = next - 1
		}
		for at > 0 && (strings.TrimSpace(lines[at-1]) == "" || strings.HasPrefix(lines[at-1], "#")) {
			at--
		}
		first := lines[seq.Content[0].Line-1]
		ins := item(first[:len(first)-len(strings.TrimLeft(first, " "))])
		lines = append(lines[:at], append([]string{ins}, lines[at:]...)...)
	}
	out := []byte(strings.Join(lines, ""))
	var check configFile // never write a file the daemon would refuse
	dec := yaml.NewDecoder(bytes.NewReader(out))
	dec.KnownFields(true)
	if err := dec.Decode(&check); err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("%s: cannot add a prompts entry for %s: %w", path, e.File, err)
	}
	return true, os.WriteFile(path, out, 0o600)
}

// mapKey is the value node of key in mapping n (nil: none, or n is not a mapping).
func mapKey(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// ConfigStatus is one line for setup: the settings file and the keys not at their default.
func ConfigStatus(dir string) string {
	set, err := LoadSettings(dir)
	if err != nil {
		return "config: " + err.Error()
	}
	d := defaultSettings()
	var diff []string
	if set.Harness != d.Harness {
		diff = append(diff, "harness "+set.Harness)
	}
	if set.GCClosedAfter != d.GCClosedAfter {
		diff = append(diff, "gc.closed_after "+formatRetention(set.GCClosedAfter))
	}
	if set.GCArchiveKeep != d.GCArchiveKeep {
		diff = append(diff, "gc.archive_keep "+formatRetention(set.GCArchiveKeep))
	}
	if set.UpdateCheck != d.UpdateCheck {
		diff = append(diff, "update.check false")
	}
	if len(set.AllowedRoots) > 0 {
		diff = append(diff, "spawn.allowed_roots ["+strings.Join(set.AllowedRoots, ", ")+"]")
	}
	if len(set.Prompts) > 0 {
		diff = append(diff, fmt.Sprintf("prompts (%d)", len(set.Prompts)))
	}
	if !slices.Equal(set.Columns, d.Columns) {
		diff = append(diff, "display.columns ["+strings.Join(set.Columns, ", ")+"]")
	}
	if len(diff) == 0 {
		return "config: " + ConfigPath(dir) + " (every key at its default)"
	}
	return "config: " + ConfigPath(dir) + " (" + strings.Join(diff, ", ") + "; the other keys default)"
}

// formatRetention writes d as parseRetention reads it.
func formatRetention(d time.Duration) string {
	switch {
	case d == 0:
		return "off"
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return d.String()
}

// parseRetention is `off` (0), `<n>d`, or a positive Go duration (`36h`).
func parseRetention(s string) (time.Duration, error) {
	if s == "off" {
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return 0, errors.New("bad days")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errors.New("bad duration")
	}
	return d, nil
}

// gcPlace is where gc archives, and what it removes with a participant: its entries in logs/, run/
// and sessions/ (the local driver's layout), by its id, nothing else in the directory.
func gcPlace(dir string) core.GCPlace {
	return core.GCPlace{ArchiveDir: ArchiveDir(dir), Own: func(key string) []string { return local.OwnPaths(dir, key) }}
}

// runGC is one automatic run: gc of teams closed longer than GCClosedAfter (with their run
// logs), then archives older than GCArchiveKeep. Off settings skip their part.
func runGC(ctx context.Context, eng *core.Engine, dir string, set Settings) (core.GCResult, error) {
	var res core.GCResult
	if set.GCClosedAfter > 0 {
		var err error
		if res, err = eng.GC(ctx, gcPlace(dir), core.GCArgs{ClosedBeforeMs: set.GCClosedAfter.Milliseconds()}); err != nil {
			return res, err
		}
	}
	expired, err := eng.ExpireArchives(ArchiveDir(dir), set.GCArchiveKeep)
	res.ExpiredArchives = expired
	return res, err
}

// autoGC runs gc at start and every gcEvery until ctx ends. It logs what it did (nothing when
// nothing was done) and its failures; neither stops the daemon.
func (s *server) autoGC(ctx context.Context) {
	defer s.wg.Done()
	for {
		res, err := runGC(ctx, s.eng, s.dir, s.settings)
		if err != nil {
			s.log.Error("auto gc", "err", err)
		}
		for _, g := range append(res.Teams, res.Solos...) {
			what := "team"
			if g.ParticipantID != "" {
				what = "solo"
			}
			switch {
			case g.Deleted:
				s.log.Info("auto gc", what, g.Name, "archive", g.Archive, "log_dirs", g.LogDirs, "log_bytes", g.LogBytes)
			case g.Skipped != "":
				s.log.Warn("auto gc skipped", what, g.Name, "why", g.Skipped)
			}
		}
		if len(res.ExpiredArchives) > 0 {
			s.log.Info("auto gc", "expired_archives", res.ExpiredArchives)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(gcEvery):
		}
	}
}
