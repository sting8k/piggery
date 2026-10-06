package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	ompext "github.com/sting8k/piggery/extensions/omp"
)

// A worker runs pi with the human's pi setup minus a blacklist: its
// PI_CODING_AGENT_DIR is a directory built for the run from the human's agent dir. Every entry
// is a symlink except settings.json (a filtered copy) and extensions/ (a real directory of
// symlinks to the entries not blacklisted). A run's directory is new and only removed once
// the run has ended: a live worker's directory is never modified.
//
// Path and source rules follow pi 0.87 (utils/paths.js normalizePath, package-manager.js
// isLocalPath). omp (a pi fork) reads the same variable and layout; its settings are config.yml
// (YAML, extensions and disabledExtensions) beside the legacy settings.json.

// alwaysBlacklisted are extensions a worker must never load, whatever pi.json says: the peer
// chat and the interactive question UI assume a human at the terminal.
var alwaysBlacklisted = []string{"pi-peer", "pi-askuserquestion"}

// piggeryPackages are the package.json names of piggery's pi and omp extensions: a copy of one
// anywhere in the human's setup is skipped (the worker loads piggery with -e, never twice).
var piggeryPackages = []string{piggeryPackage, ompext.PackageName}

// piggeryPackage is the package.json name of piggery's pi extension: the one IsPiggeryExtension,
// which `setup pi` reads, recognizes.
const piggeryPackage = "piggery-pi"

// AgentDirRoot is where the per-run agent dirs live: <dir>/run/pi/<participant>/<run>.
func AgentDirRoot(dir string) string { return RunRoot(dir, "pi") }

// HumanAgentDir is the agent dir the human's pi uses: PI_CODING_AGENT_DIR, else ~/.pi/agent.
// A PI_CODING_AGENT_DIR under root (a daemon started from inside a worker inherits the
// worker's generated dir) is ignored: a worker dir is never built from another.
func HumanAgentDir(root string) string {
	home, _ := os.UserHomeDir()
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		d = normalizePath(d, home, home)
		if !within(d, root) {
			return d
		}
	}
	return filepath.Join(home, ".pi", "agent")
}

// OmpSessionsDir is where an omp worker's sessions are kept (omp --session-dir): piggery's own
// and per participant, so they outlive the run's agent dir and `--resume <id>` finds them in the
// next run's.
func OmpSessionsDir(dir, participantID string) string {
	return filepath.Join(SessionsRoot(dir, "omp"), participantID)
}

// moveOmpSessions carries a worker's sessions from where the first builds kept them
// (<dir>/harness/omp-sessions/<participant>) to OmpSessionsDir, once: when the new place has
// none yet. Rename, so a resume finds its session id. Drop it once no such build is in use.
func moveOmpSessions(dir, participantID string) {
	old := filepath.Join(dir, "harness", "omp-sessions", participantID)
	if _, err := os.Stat(OmpSessionsDir(dir, participantID)); err == nil {
		return
	}
	if _, err := os.Stat(old); err != nil {
		return
	}
	if err := os.MkdirAll(SessionsRoot(dir, "omp"), 0o700); err == nil {
		os.Rename(old, OmpSessionsDir(dir, participantID))
	}
}

// IsPiggeryExtension reports whether an extension entry of pi's settings is piggery's: a local
// path whose package.json is piggeryPackage, or, when the path is gone (a moved checkout, a
// removed copy), one ending in piggery/extensions/pi or extensions/piggery. home and base resolve "~" and relative entries as pi does.
func IsPiggeryExtension(entry, home, base string) bool {
	if !isLocal(entry) {
		return false
	}
	p := normalizePath(entry, home, base)
	fi, err := os.Stat(p)
	if err != nil {
		p = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(p)), "/index.ts")
		return strings.HasSuffix(p, "/piggery/extensions/pi") || strings.HasSuffix(p, "/extensions/piggery")
	}
	if !fi.IsDir() {
		p = filepath.Dir(p)
	}
	var pkg struct {
		Name string `json:"name"`
	}
	raw, err := os.ReadFile(filepath.Join(p, "package.json"))
	return err == nil && json.Unmarshal(raw, &pkg) == nil && pkg.Name == piggeryPackage
}

// isLocal is pi's isLocalPath: a source is remote only with one of these prefixes.
func isLocal(s string) bool {
	for _, p := range []string{"npm:", "git:", "github:", "http:", "https:", "ssh:"} {
		if strings.HasPrefix(s, p) {
			return false
		}
	}
	return true
}

// normalizePath is pi's normalizePath for a local source: trimmed, ~ expanded, file:// made a
// path, and a relative path resolved against base (the agent dir it was written in).
func normalizePath(p, home, base string) string {
	p = strings.TrimSpace(p)
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "file://"):
		p = strings.TrimPrefix(p, "file://")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

var (
	npmVersion = regexp.MustCompile(`@[^@/]+$`)
	gitRef     = regexp.MustCompile(`@[^@/]*$`)
)

// sourceName is the name a blacklist matches: the npm package name (no scope, no version),
// the repo name of another remote source, or the basename of a local path (no .ts/.js).
func sourceName(s string) string {
	switch {
	case strings.HasPrefix(s, "npm:"):
		s = npmVersion.ReplaceAllString(strings.TrimPrefix(s, "npm:"), "")
		return s[strings.LastIndex(s, "/")+1:]
	case !isLocal(s):
		return filepath.Base(strings.TrimSuffix(gitRef.ReplaceAllString(s, ""), ".git"))
	}
	return strings.TrimSuffix(strings.TrimSuffix(filepath.Base(s), ".ts"), ".js")
}

// realPath resolves symlinks where the path exists.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// within: p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// blacklist decides which sources a worker skips.
type blacklist struct {
	names   map[string]bool
	piggery []string // real dirs of the -e extensions (a file's parent)
}

func newBlacklist(names, extPaths []string, home, cwd string) blacklist {
	b := blacklist{names: map[string]bool{}}
	for _, n := range append(append([]string{}, alwaysBlacklisted...), names...) {
		b.names[n] = true
	}
	for _, p := range extPaths {
		p = realPath(normalizePath(p, home, cwd))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			p = filepath.Dir(p)
		}
		b.piggery = append(b.piggery, p)
	}
	return b
}

// blocked reports whether a normalized source is blacklisted: by name, or (local) as a copy of
// piggery: at, inside, or containing an -e extension dir, or a dir whose package.json is
// one of piggeryPackages.
func (b blacklist) blocked(source string) bool {
	if b.names[sourceName(source)] {
		return true
	}
	if !isLocal(source) {
		return false
	}
	p := realPath(source)
	for _, d := range b.piggery {
		if within(p, d) || within(d, p) {
			return true
		}
	}
	dir := p
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		dir = filepath.Dir(p)
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil && json.Unmarshal(raw, &pkg) == nil {
		return slices.Contains(piggeryPackages, pkg.Name)
	}
	return false
}

// settingsFile is a settings file of an agent dir that a worker gets a filtered copy of: filter
// writes to (from the human's copy at from, whose dir is src) without the blacklisted extensions.
type settingsFile struct {
	name   string
	filter func(from, to, src, home string, bl blacklist) error
}

var (
	piSettings  = []settingsFile{{"settings.json", filterSettings}}
	ompSettings = []settingsFile{{"config.yml", filterOmpConfig}, {"settings.json", filterSettings}}
)

// buildAgentDir is buildAgentDirWith for pi.
func buildAgentDir(src, dst, home string, bl blacklist) error {
	return buildAgentDirWith(src, dst, home, bl, piSettings)
}

// buildAgentDirWith creates dst (it must not exist) from the human agent dir src. With no src (a
// fresh machine) dst holds only an empty extensions/.
func buildAgentDirWith(src, dst, home string, bl blacklist, settings []settingsFile) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err // never reuse a directory: it may belong to a live run
	}
	if err := os.Mkdir(filepath.Join(dst, "extensions"), 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	filtered := map[string]bool{"extensions": true}
	for _, f := range settings {
		filtered[f.name] = true
	}
	for _, e := range entries {
		if filtered[e.Name()] {
			continue
		}
		if err := symlinkOrCopy(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	exts, err := os.ReadDir(filepath.Join(src, "extensions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range exts {
		p := filepath.Join(src, "extensions", e.Name())
		if bl.blocked(p) || e.Name() == "piggery" { // `piggery setup pi`'s copy: the worker has its -e
			continue
		}
		if err := symlinkOrCopy(p, filepath.Join(dst, "extensions", e.Name())); err != nil {
			return err
		}
	}
	for _, f := range settings {
		if err := f.filter(filepath.Join(src, f.name), filepath.Join(dst, f.name), src, home, bl); err != nil {
			return err
		}
	}
	return nil
}

// filterSettings copies settings.json with blacklisted extensions removed and blacklisted
// packages kept without their extensions (their other fields, e.g. skills, stay). Local paths
// are normalized (they resolved from src, not from the new dir). Other keys pass through.
func filterSettings(from, to, src, home string, bl blacklist) error {
	b, err := os.ReadFile(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	norm := func(p string) string {
		if !isLocal(p) {
			return p
		}
		return normalizePath(p, home, src)
	}
	if list, ok := s["extensions"].([]any); ok {
		kept := []any{}
		for _, x := range list {
			p, ok := x.(string)
			if !ok {
				continue // pi reads only strings here
			}
			if p = norm(p); !bl.blocked(p) {
				kept = append(kept, p)
			}
		}
		s["extensions"] = kept
	}
	if list, ok := s["packages"].([]any); ok {
		for i, x := range list {
			switch p := x.(type) {
			case string:
				list[i] = norm(p)
				if bl.blocked(norm(p)) {
					list[i] = map[string]any{"source": norm(p), "extensions": []any{}}
				}
			case map[string]any:
				source, _ := p["source"].(string)
				p["source"] = norm(source)
				if bl.blocked(norm(source)) {
					p["extensions"] = []any{}
				}
			}
		}
	}
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(to, out, 0o600)
}

// filterOmpConfig copies omp's config.yml with blacklisted extensions removed from its
// `extensions` list (local paths normalized: they resolved from src, not from the new dir).
// Every other key passes through; the copy is data, so YAML comments and order are not kept.
func filterOmpConfig(from, to, src, home string, bl blacklist) error {
	b, err := os.ReadFile(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var c map[string]any
	if err := yaml.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	if list, ok := c["extensions"].([]any); ok {
		kept := []any{}
		for _, x := range list {
			p, ok := x.(string)
			if !ok {
				continue // omp reads only strings here
			}
			if isLocal(p) {
				p = normalizePath(p, home, src)
			}
			if !bl.blocked(p) {
				kept = append(kept, p)
			}
		}
		c["extensions"] = kept
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(to, out, 0o600)
}
