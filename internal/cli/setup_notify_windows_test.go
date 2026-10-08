//go:build windows

package cli

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/server"
)

// windowsPowerShell is the program the daemon runs a .ps1 hook with (server.hookCommand).
func windowsPowerShell(args ...string) *exec.Cmd {
	return exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		append([]string{"-NoProfile", "-ExecutionPolicy", "Bypass"}, args...)...)
}

// `setup notify add` on Windows, through the real entrypoint: the files it writes are hooks the
// daemon runs, Windows PowerShell reads them without a syntax error, and the ntfy one, run on a
// notice, posts the body as it is (UTF-8, nothing in it run or cut) with the title in one header.
func TestSetupNotifyOnWindows(t *testing.T) {
	dir := t.TempDir()
	for _, target := range []string{"desktop", "ntfy:alerts"} {
		var out, errOut bytes.Buffer
		if code := Main(dir, []string{"setup", "notify", "add", target}, &out, &errOut); code != 0 {
			t.Fatalf("add %s: exit %d\n%s%s", target, code, out.String(), errOut.String())
		}
	}
	hooks := server.NotifyHooksDir(dir)
	desktop, ntfy := filepath.Join(hooks, "desktop.ps1"), filepath.Join(hooks, "ntfy-alerts.ps1")
	if files, _ := server.NotifyHooks(dir); !slices.Equal(files, []string{desktop, ntfy}) {
		t.Fatalf("the daemon would run %q; want the two files setup wrote", files)
	}
	for _, f := range []string{desktop, ntfy} {
		parse := windowsPowerShell("-Command", "[void][scriptblock]::Create([IO.File]::ReadAllText($env:PIGGERY_TEST_PS1))")
		parse.Env = append(os.Environ(), "PIGGERY_TEST_PS1="+f)
		if out, err := parse.CombinedOutput(); err != nil {
			t.Fatalf("%s does not parse: %v\n%s", filepath.Base(f), err, out)
		}
	}

	var gotTitle string
	var gotBody []byte
	ntfySh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/alerts" {
			gotTitle, _ = new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
			gotBody, _ = io.ReadAll(r.Body)
		}
	}))
	defer ntfySh.Close()
	script, err := os.ReadFile(ntfy)
	if err != nil || !bytes.Contains(script, []byte("https://ntfy.sh/alerts")) {
		t.Fatalf("the ntfy script does not post to its topic: %v", err)
	}
	local := filepath.Join(t.TempDir(), "ntfy-alerts.ps1") // the same script, posting to the server above
	os.WriteFile(local, bytes.ReplaceAll(script, []byte("https://ntfy.sh/"), []byte(ntfySh.URL+"/")), 0o600)

	pwned := filepath.Join(dir, "pwned")
	body := "xong r\u1ed3i \u2713 \"quoted\" -d @C:\\secret $(New-Item '" + pwned + "') `n; New-Item '" + pwned + "' & echo x > \"" + pwned + "\""
	run := windowsPowerShell("-File", local)
	run.Stdin = strings.NewReader(mustJSON(map[string]string{"team": "demo\nX-Evil: 1", "gate": "lead", "kind": "reply", "body": body}) + "\n")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("ntfy hook: %v\n%s", err, out)
	}
	if gotTitle != "piggery: demoX-Evil: 1" || string(gotBody) != body {
		t.Fatalf("ntfy got title %q, body %q; want the notice's own", gotTitle, gotBody)
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("a notice's body ran as a command")
	}
}
