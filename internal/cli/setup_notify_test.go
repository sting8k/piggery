package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `setup notify add` through the real entrypoint: two targets give two marked, executable files; a
// file the user wrote is refused (and replaced only with --force); a written file, run on a sample
// notice, calls its target with the fields as arguments or stdin and never inside a command string.
func TestSetupNotify(t *testing.T) {
	skipOnWindows(t, "the notify templates are sh scripts, which the Windows daemon does not run (Windows templates are a later step)")
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("the notify scripts need jq")
	}
	dir := t.TempDir()
	hooks := filepath.Join(dir, "hooks", "notify.d")
	setup := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Main(dir, append([]string{"setup", "notify"}, args...), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	for _, target := range []string{"desktop", "ntfy:alerts"} {
		if code, out := setup("add", target); code != 0 {
			t.Fatalf("add %s: exit %d\n%s", target, code, out)
		}
	}
	for _, f := range []string{"desktop", "ntfy-alerts"} {
		fi, err := os.Stat(filepath.Join(hooks, f))
		if err != nil || fi.Mode().Perm() != 0o700 || !writtenBySetup(filepath.Join(hooks, f)) {
			t.Fatalf("%s: %v, %v; want a marked file of mode 0700", f, fi, err)
		}
	}
	mine := "#!/bin/sh\necho mine\n"
	os.WriteFile(filepath.Join(hooks, "herdr"), []byte(mine), 0o700)
	if code, out := setup("add", "herdr"); code == 0 || !strings.Contains(out, "left alone") {
		t.Fatalf("add herdr over a hand-written file: exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(hooks, "herdr")); string(b) != mine {
		t.Fatalf("the hand-written file changed:\n%s", b)
	}
	if code, out := setup("add", "herdr", "--force"); code != 0 {
		t.Fatalf("add herdr --force: exit %d\n%s", code, out)
	}

	// Run the written files with fake curl and herdr first on PATH: each records its arguments and stdin.
	bin := t.TempDir()
	rec := filepath.Join(bin, "rec")
	for _, c := range []string{"curl", "herdr"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + rec + "-" + c + "'\ncat > '" + rec + "-" + c + ".stdin'\n"
		if err := os.WriteFile(filepath.Join(bin, c), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(file, line string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(hooks, file))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader(line + "\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", file, err, out)
		}
	}
	body := "-d @/etc/passwd $(touch " + filepath.Join(dir, "pwned") + ") `touch " + filepath.Join(dir, "pwned") + "`"
	run("ntfy-alerts", `{"team":"demo\nX-Evil: 1","gate":"lead","kind":"reply","body":"`+strings.ReplaceAll(body, `"`, `\"`)+`"}`)
	args, _ := os.ReadFile(rec + "-curl")
	stdin, _ := os.ReadFile(rec + "-curl.stdin")
	if !strings.Contains(string(args), "Title: piggery: demoX-Evil: 1\n") || !strings.Contains(string(args), "https://ntfy.sh/alerts\n") || string(stdin) != body {
		t.Fatalf("curl got args %q, stdin %q", args, stdin)
	}
	run("herdr", `{"team":"","gate":"lead","kind":"settled","body":"all quiet; $(touch `+filepath.Join(dir, "pwned")+`)"}`)
	args, _ = os.ReadFile(rec + "-herdr")
	want := "notification\nshow\npiggery: lead\n--body\nall quiet; $(touch " + filepath.Join(dir, "pwned") + ")\n--sound\ndone\n"
	if string(args) != want {
		t.Fatalf("herdr got %q, want %q", args, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("a notice's body ran as a command")
	}
}

// What `setup notify add` writes on Windows is a file the daemon there runs and setup knows as its
// own: a .ps1 (never a .sh, which Windows cannot run), marked, with its topic filled in, and plain
// ASCII (Windows PowerShell reads a file without a BOM in the ANSI code page). herdr has no script.
func TestNotifyScriptsForWindows(t *testing.T) {
	for target, file := range map[string]string{"desktop": "desktop.ps1", "ntfy:alerts": "ntfy-alerts.ps1"} {
		tg, err := parseNotifyTarget(target, "windows")
		if err != nil || tg.file != file {
			t.Fatalf("%s: file %q, %v; want %s", target, tg.file, err, file)
		}
		b, err := tg.content()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), tg.file)
		os.WriteFile(path, b, 0o600)
		if !writtenBySetup(path) || !strings.HasPrefix(string(b), notifyMarker+target+"\n") || strings.Contains(string(b), "{{") {
			t.Errorf("%s: not marked as written for %s, or a placeholder is left:\n%.200s", file, target, b)
		}
		for i, c := range b {
			if c > 127 || c == '\r' {
				t.Errorf("%s: byte %d is %#x; want plain ASCII with LF", file, i, c)
				break
			}
		}
		if req, opt := tg.needs(); len(req)+len(opt) != 0 {
			t.Errorf("%s needs %v %v on Windows; want nothing installed", target, req, opt)
		}
	}
	if _, err := parseNotifyTarget("herdr", "windows"); err == nil {
		t.Error("herdr has a Windows target, with no script for it")
	}
}
