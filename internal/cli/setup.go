package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
	"github.com/sting8k/piggery/manifests"
)

// setup:
//   - `piggery setup` writes the worker profiles (~/.piggery/harness/{pi,claude,codex,omp,dsh,opencode}.json, an
//     existing one kept unless --force), the templates and the daemon's config.yaml (only its
//     missing keys, with their defaults), then shows where each harness and the config stand;
//   - `piggery setup <pi|claude|codex|omp|dsh|opencode|paseo>` adds piggery to that harness, or its plugin to
//     Paseo (a second run changes nothing);
//   - `piggery setup remove <name>` takes out what setup added;
//   - `piggery setup notify [add|remove <target>]` writes or removes a notify hook in hooks/notify.d.
func (e *env) setup(args []string) error {
	fs := e.flags("setup")
	ext := fs.String("ext", "", "a checkout's pi extension (extensions/pi) instead of the one in this binary")
	force := fs.Bool("force", false, "overwrite an existing profile")
	paseoHome := fs.String("paseo-home", "", "the Paseo daemon home setup paseo installs into (default: Paseo's own)")
	outdated := fs.Bool("outdated", false, "bring every installed piggery integration that is outdated up to date (takes no other argument)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *outdated && len(pos) != 0 {
		return fmt.Errorf("%w: setup --outdated takes no argument (only --paseo-home)", errUsage)
	}
	usage := fmt.Errorf("%w: setup [pi|claude|codex|omp|dsh|opencode|paseo] | setup notify [add|remove <desktop|herdr|ntfy:TOPIC>] | setup remove <pi|claude|codex|omp|dsh|opencode|paseo> [--ext PATH] [--paseo-home PATH] [--force]", errUsage)
	self, err := selfPath()
	if err != nil {
		return err
	}
	extDir := "" // the binary's copy of the pi extension, unless --ext names a checkout
	if *ext != "" {
		if extDir, err = extensionDir(*ext); err != nil {
			return err
		}
	}
	o := setupOpts{dir: e.dir, self: self, ext: extDir, paseoHome: *paseoHome}
	if o.paseoHome != "" {
		if o.paseoHome, err = filepath.Abs(o.paseoHome); err != nil {
			return err
		}
	}
	// Windows: the file the Node clients read the daemon's pipe name from (nothing on unix).
	if err := server.WritePipeFile(e.dir); err != nil {
		return err
	}
	switch {
	case *outdated:
		return e.updateOutdated(o)
	case len(pos) > 0 && pos[0] == "notify":
		return e.setupNotify(o.dir, pos[1:], *force)
	case len(pos) == 1:
		if t, ok := targetNamed(pos[0]); ok {
			return e.say(t.install(o))
		}
		return usage
	case len(pos) == 2 && pos[0] == "remove":
		if t, ok := targetNamed(pos[1]); ok {
			return e.say(t.remove(o))
		}
		return usage
	case len(pos) != 0:
		return usage
	}
	index := filepath.Join(local.PiExtDir(e.dir), "index.ts")
	if extDir != "" {
		index = filepath.Join(extDir, "index.ts")
	}
	// Profiles: a missing one is written (every one with --force); an existing one only gets the
	// keys it lacks, below, like every config file piggery owns.
	for _, p := range []struct {
		path, what string
		def        any
	}{
		{local.ProfilePath(e.dir), "pi workers", local.DefaultProfile(index)},
		{local.ClaudeProfilePath(e.dir), "Claude Code workers", local.DefaultClaudeProfile},
		{local.CodexProfilePath(e.dir), "Codex workers", local.DefaultCodexProfile},
		{local.OmpProfilePath(e.dir), "omp workers", local.DefaultOmpProfile},
		{local.DshProfilePath(e.dir), "dsh workers", local.DefaultDshProfile},
		{local.OpencodeProfilePath(e.dir), "opencode workers", local.DefaultOpencodeProfile},
	} {
		if w, err := writeJSON(p.path, p.def, *force); err != nil {
			return err
		} else if w {
			fmt.Fprintf(e.stdout, "wrote %s (%s)\n", p.path, p.what)
		}
	}
	if err := manifests.Unpack(e.dir); err != nil {
		return fmt.Errorf("templates: %w", err)
	}
	fmt.Fprintf(e.stdout, "templates in %s\n", manifests.Dir(e.dir))
	filled, errs := server.EnsureFiles(e.dir)
	for _, f := range filled {
		fmt.Fprintf(e.stdout, "added to %s: %s (defaults)\n", f.Path, strings.Join(f.Added, ", "))
	}
	for _, err := range errs {
		fmt.Fprintf(e.stdout, "! %v\n", err)
	}
	fmt.Fprintln(e.stdout)
	for _, st := range harnessStates(o) {
		fmt.Fprintln(e.stdout, st.line())
		for _, p := range st.Problems {
			fmt.Fprintf(e.stdout, "  ! %s\n", p)
		}
	}
	fmt.Fprintln(e.stdout, server.ConfigStatus(e.dir))
	if set, err := server.LoadSettings(e.dir); err == nil {
		if n := proto.UpdateNotice(server.UpdateAvailable(e.dir, set, Version)); n != "" {
			fmt.Fprintln(e.stdout, n)
		}
	}
	return nil
}

// updateOutdated runs `setup <name>` for every integration that is installed and outdated (pi, omp
// and dsh too, in case the daemon is not running to do it), one after the other; a failure is named
// and the exit is non-zero, but the others still run. Not installed: untouched.
func (e *env) updateOutdated(o setupOpts) error {
	list := outdatedIntegrations(o.dir, false)
	if len(list) == 0 {
		fmt.Fprintln(e.stdout, "piggery integrations are up to date")
		return nil
	}
	var failed []string
	for _, i := range list {
		t, ok := targetNamed(i.Name)
		if !ok {
			continue
		}
		msg, err := t.install(o)
		if err != nil {
			fmt.Fprintf(e.stdout, "%s: NOT updated (%s): %v\n", i.Name, i.detail(), err)
			failed = append(failed, i.Name)
			continue
		}
		fmt.Fprintf(e.stdout, "%s: updated (%s)\n%s\n", i.Name, i.detail(), msg)
	}
	if len(failed) > 0 {
		return fmt.Errorf("not updated: %s", strings.Join(failed, ", "))
	}
	return nil
}

func (e *env) say(msg string, err error) error {
	if err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, msg)
	return nil
}

// selfPath is this piggery executable, symlinks resolved: what harnesses are set up to run.
func selfPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(self)
}

// extensionDir is the directory of a checkout's pi extension given with --ext (the directory or
// its index.ts).
func extensionDir(ext string) (string, error) {
	ext, err := filepath.Abs(ext)
	if err != nil {
		return "", err
	}
	if filepath.Base(ext) == "index.ts" {
		ext = filepath.Dir(ext)
	}
	if _, err := os.Stat(filepath.Join(ext, "index.ts")); err != nil {
		return "", fmt.Errorf("pi extension %s: %w", ext, err)
	}
	return ext, nil
}

// harnessState is where a harness stands, for `piggery setup` and doctor.
type harnessState struct {
	Name      string
	Cmd       string // the harness's command from its profile
	Path      string // where Cmd is on PATH ("" = not found)
	Version   string
	Tested    []string
	Installed bool
	Detail    string
	Problems  []problem
}

// problem is one thing wrong, and the command that fixes it ("" when there is none).
type problem struct{ Text, Fix string }

func (p problem) String() string {
	if p.Fix == "" {
		return p.Text
	}
	return p.Text + "; fix: " + p.Fix
}

// olderThanTested: version is older than the oldest of tested, the floor piggery was tested from (a
// newer one is not flagged). A version that does not parse, or no parsable tested one: false.
func olderThanTested(version string, tested []string) bool {
	v, ok := parseVersion(version)
	if !ok {
		return false
	}
	var oldest *semver
	for _, t := range tested {
		if tv, ok := parseVersion(t); ok && (oldest == nil || tv.compare(*oldest) < 0) {
			oldest = &tv
		}
	}
	return oldest != nil && v.compare(*oldest) < 0
}

// semver is "1.2.3-rc.1": the numbers, and the pre-release parts (none: a release).
type semver struct{ nums, pre []string }

// parseVersion reads dotted numbers and an optional "-pre.release" (a leading "v" is dropped).
func parseVersion(s string) (v semver, ok bool) {
	core, pre, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(s), "v"), "-")
	v.nums = strings.Split(core, ".")
	for _, p := range v.nums {
		if _, err := strconv.Atoi(p); err != nil {
			return semver{}, false
		}
	}
	if pre != "" {
		v.pre = strings.Split(pre, ".")
	}
	return v, true
}

// comparePart: by value when both are numbers, else as text.
func comparePart(x, y string) int {
	xi, xerr := strconv.Atoi(x)
	yi, yerr := strconv.Atoi(y)
	if xerr == nil && yerr == nil {
		return cmp.Compare(xi, yi)
	}
	return cmp.Compare(x, y)
}

// compare: the numbers (a missing one is 0); then a pre-release is older than its release, and
// pre-release parts go one by one, the shorter list first.
func (a semver) compare(b semver) int {
	for i := 0; i < max(len(a.nums), len(b.nums)); i++ {
		x, y := "0", "0"
		if i < len(a.nums) {
			x = a.nums[i]
		}
		if i < len(b.nums) {
			y = b.nums[i]
		}
		if c := comparePart(x, y); c != 0 {
			return c
		}
	}
	if len(a.pre) == 0 || len(b.pre) == 0 {
		return cmp.Compare(len(b.pre), len(a.pre)) // no pre-release is the newer
	}
	for i := 0; i < min(len(a.pre), len(b.pre)); i++ {
		if c := comparePart(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a.pre), len(b.pre))
}

func (st harnessState) line() string {
	where := "not on PATH"
	if st.Path != "" {
		where = st.Version
		if where == "" {
			where = "version unknown"
		} else if olderThanTested(st.Version, st.Tested) {
			where += fmt.Sprintf(" (not tested; tested: %s)", strings.Join(st.Tested, ", "))
		}
	}
	state := "not installed (piggery setup " + st.Name + ")"
	if st.Installed {
		state = "installed"
		if st.Detail != "" {
			state += ": " + st.Detail
		}
	}
	return fmt.Sprintf("%-7s %-12s %s", st.Name, where, state)
}

// harnessStates checks every setup target; a harness's worker profile may name another cmd and
// the versions piggery was tested with.
func harnessStates(o setupOpts) []harnessState {
	var sts []harnessState
	ints := map[string]integration{}
	for _, i := range installedIntegrations(o.dir) {
		ints[i.Name] = i
	}
	for _, t := range setupTargets {
		sts = append(sts, t.status(o))
		st := &sts[len(sts)-1]
		if i, ok := ints[t.name]; ok && st.Installed && i.outdated() &&
			!slices.ContainsFunc(st.Problems, func(p problem) bool { return strings.HasPrefix(p.Text, "outdated (") }) {
			st.Problems = append(st.Problems, problem{"outdated (" + i.detail() + ")", "piggery setup --outdated"})
		}
		st.Cmd = t.cmd
		var p struct {
			Cmd            string   `json:"cmd"`
			TestedVersions []string `json:"tested_versions"`
		}
		if h, ok := harnessNamed(t.name); ok {
			if b, err := os.ReadFile(h.profilePath(o.dir)); err == nil && json.Unmarshal(b, &p) == nil {
				st.Cmd, st.Tested = firstNonEmpty(p.Cmd, t.cmd), p.TestedVersions
			}
		}
		if path, err := exec.LookPath(st.Cmd); err == nil {
			st.Path = path
			version := local.HarnessVersion
			if t.version != nil {
				version = t.version
			}
			st.Version, _ = version(context.Background(), st.Cmd)
		} else if st.Installed {
			st.Problems = append([]problem{{fmt.Sprintf("`%s` is not on PATH", st.Cmd), "install it, or `piggery setup remove " + st.Name + "`"}}, st.Problems...)
		}
	}
	return sts
}

// harnessWarnings are doctor's lines: problems of an installed harness, and a harness on PATH at
// a version piggery was not tested with (it may misbehave).
func harnessWarnings(dir string) []string {
	self, err := selfPath()
	if err != nil {
		return nil
	}
	var out []string
	for _, st := range harnessStates(setupOpts{dir: dir, self: self}) {
		if st.Path != "" && olderThanTested(st.Version, st.Tested) {
			out = append(out, fmt.Sprintf("%s %s is older than the oldest tested version (tested: %s); it may misbehave",
				st.Name, st.Version, strings.Join(st.Tested, ", ")))
		}
		for _, p := range st.Problems {
			out = append(out, st.Name+": "+p.String())
		}
	}
	return out
}

// writeJSON writes v to path as indented JSON (dirs 0700, file 0600). An existing file is kept
// unless force; it reports whether it wrote.
func writeJSON(path string, v any, force bool) (bool, error) {
	if _, err := os.Stat(path); err == nil && !force {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// template new <name> [--from <built-in>]: a copy of the current built-in (default p2p) as a new
// template in ~/.piggery/templates for the user to edit. Local, no daemon; refused if it exists.
func (e *env) template(args []string) error {
	if len(args) == 0 || args[0] != "new" {
		return fmt.Errorf("%w: template new <name> [--from <built-in>]", errUsage)
	}
	fs := e.flags("template new")
	from := fs.String("from", "p2p", "built-in to copy ("+strings.Join(manifests.Builtins(), ", ")+")")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: template new <name> [--from <built-in>]", errUsage)
	}
	dir, err := manifests.New(e.dir, pos[0], *from)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "wrote %s (from built-in %s); edit %s and its prompts\n", dir, *from, manifests.ManifestFile)
	return nil
}
