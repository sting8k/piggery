package cli

import (
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/sting8k/piggery/internal/server"
)

// The scripts `setup notify add` writes into ~/.piggery/hooks/notify.d, one template per target:
// sh scripts, and on Windows PowerShell ones (the daemon there runs a hook by its extension, a .ps1
// with Windows PowerShell, and never a .sh).
//
//go:embed notifyhooks/*.sh notifyhooks/*.ps1
var notifyHooks embed.FS

// notifyMarker starts the line (the first or second of a script, after a shebang) that says
// setup wrote the file: setup rewrites or removes only a file that has it.
const notifyMarker = "# written by piggery setup notify add "

var ntfyTopic = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// notifyTarget is a target as written on the command line: desktop, herdr or ntfy:<topic>.
type notifyTarget struct {
	name   string // the target as written
	file   string // the file in notify.d
	script string // the template
	topic  string
	goos   string // the OS the file is written for
}

// notifyTargets are the targets setup has a script for on goos (herdr has none for Windows).
func notifyTargets(goos string) []string {
	if goos == "windows" {
		return []string{"desktop", "ntfy:<topic>"}
	}
	return []string{"desktop", "herdr", "ntfy:<topic>"}
}

// parseNotifyTarget reads target s for goos (runtime.GOOS; a parameter for the tests).
func parseNotifyTarget(s, goos string) (notifyTarget, error) {
	ext, script := "", ".sh"
	if goos == "windows" {
		ext, script = ".ps1", ".ps1"
	}
	known := strings.Join(notifyTargets(goos), ", ")
	switch topic, isNtfy := strings.CutPrefix(s, "ntfy:"); {
	case s == "herdr" && goos == "windows":
		return notifyTarget{}, fmt.Errorf("%w: notify target herdr has no script for Windows (%s)", errUsage, known)
	case s == "desktop" || s == "herdr":
		return notifyTarget{name: s, file: s + ext, script: s + script, goos: goos}, nil
	case isNtfy && ntfyTopic.MatchString(topic):
		return notifyTarget{name: s, file: "ntfy-" + topic + ext, script: "ntfy" + script, topic: topic, goos: goos}, nil
	case isNtfy:
		return notifyTarget{}, fmt.Errorf("%w: ntfy topic %q: want letters, digits, - and _ only", errUsage, topic)
	}
	return notifyTarget{}, fmt.Errorf("%w: unknown notify target %q (%s)", errUsage, s, known)
}

// content is the file setup writes for the target.
func (t notifyTarget) content() ([]byte, error) {
	b, err := notifyHooks.ReadFile("notifyhooks/" + t.script)
	return []byte(strings.ReplaceAll(string(b), "{{TOPIC}}", t.topic)), err
}

// needs are the commands the target's script runs, and which of them it can do without.
func (t notifyTarget) needs() (required []string, optional []string) {
	switch {
	case t.goos == "windows":
		return nil, nil // Windows PowerShell, which runs the script, does all of it
	case t.name == "herdr":
		return []string{"jq", "herdr"}, nil
	case t.topic != "":
		return []string{"jq", "curl", "tr"}, nil
	case t.goos == "darwin":
		return []string{"jq", "osascript"}, []string{"terminal-notifier"}
	}
	return []string{"jq", "notify-send"}, nil
}

// missing says which of the target's commands are not on PATH ("" when none is).
func (t notifyTarget) missing() string {
	req, opt := t.needs()
	var out []string
	for _, c := range req {
		if _, err := exec.LookPath(c); err != nil {
			out = append(out, c)
		}
	}
	for _, c := range opt {
		if _, err := exec.LookPath(c); err != nil {
			out = append(out, c+" (optional)")
		}
	}
	return strings.Join(out, ", ")
}

// writtenBySetup reports whether the file at path is one setup notify wrote (it has the marker).
func writtenBySetup(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := strings.SplitN(string(b), "\n", 3)
	for _, l := range lines[:min(len(lines), 2)] {
		if strings.HasPrefix(l, notifyMarker) {
			return true
		}
	}
	return false
}

// setupNotify is `setup notify [add|remove <target>] [--force]`: the notify hooks setup writes into
// hooks/notify.d. Alone, it lists what is there and what each target needs.
func (e *env) setupNotify(dir string, args []string, force bool) error {
	targets := strings.ReplaceAll(strings.Join(notifyTargets(runtime.GOOS), "|"), "<topic>", "TOPIC")
	usage := fmt.Errorf("%w: setup notify | setup notify add <%s> [--force] | setup notify remove <%s>", errUsage, targets, targets)
	hooksDir := server.NotifyHooksDir(dir)
	switch {
	case len(args) == 0:
		e.listNotify(dir)
		return nil
	case len(args) != 2 || (args[0] != "add" && args[0] != "remove"):
		return usage
	}
	t, err := parseNotifyTarget(args[1], runtime.GOOS)
	if err != nil {
		return err
	}
	path := filepath.Join(hooksDir, t.file)
	_, statErr := os.Stat(path)
	exists := statErr == nil
	if exists && !writtenBySetup(path) && (args[0] == "remove" || !force) {
		return fmt.Errorf("%s is not one piggery setup notify wrote: left alone (setup notify add %s --force replaces it)", path, t.name)
	}
	if args[0] == "remove" {
		if !exists {
			fmt.Fprintf(e.stdout, "no %s\n", path)
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "removed %s\n", path)
		return nil
	}
	b, err := t.content()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hooksDir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "hooks", ".notify-"+t.file+".tmp") // not in notify.d: the daemon would run it
	if err := os.WriteFile(tmp, b, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "wrote %s (each notice runs it from now on; no restart)\n", path)
	if m := t.missing(); m != "" {
		fmt.Fprintf(e.stdout, "%s: not on PATH: %s\n", t.name, m)
	}
	return nil
}

// listNotify prints the files of notify.d (which setup wrote), and what each target needs.
func (e *env) listNotify(dir string) {
	files, legacy := server.NotifyHooks(dir)
	if len(files) == 0 {
		fmt.Fprintf(e.stdout, "no notify hooks in %s\n", server.NotifyHooksDir(dir))
	}
	for _, f := range files {
		who := "yours"
		if writtenBySetup(f) {
			who = "written by setup notify"
		}
		fmt.Fprintf(e.stdout, "%s (%s)\n", f, who)
	}
	if legacy {
		fmt.Fprintf(e.stdout, "! %s is no longer run: move it into %s\n", filepath.Join(dir, "hooks", "notify"), server.NotifyHooksDir(dir))
	}
	fmt.Fprintln(e.stdout, "targets: setup notify add <target>")
	for _, name := range notifyTargets(runtime.GOOS) {
		t, _ := parseNotifyTarget(strings.Replace(name, "<topic>", "topic", 1), runtime.GOOS)
		req, opt := t.needs()
		line := fmt.Sprintf("  %s: needs %s", name, strings.Join(req, ", "))
		if len(req) == 0 {
			line = fmt.Sprintf("  %s: needs nothing installed", name)
		}
		if len(opt) > 0 {
			line += "; better with " + strings.Join(opt, ", ")
		}
		if m := t.missing(); m != "" {
			line += " (not on PATH: " + m + ")"
		}
		fmt.Fprintln(e.stdout, line)
	}
}
