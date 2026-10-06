//go:build windows

package local

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// A harness installed with npm (pi, claude, codex, omp, opencode) is a .cmd shim on Windows, and
// CreateProcess runs a shim through cmd.exe, which reads the line it is given by rules of its own:
// it ends the line at the first newline and expands %VAR%, so a Claude worker's multi-line role card
// would arrive cut after its first line (Go does not quote for cmd.exe, os/exec says so). So a shim
// of npm's own form (cmd-shim) is not run: its target is, directly, with the arguments as they
// are. A shim of another form is run as it is, unless an argument has a newline, which it could not
// pass on.

var (
	// ... "%_prog%"  [flags]  "%dp0%\path\to\cli.js" %*
	shimNode = regexp.MustCompile(`(?m)"%_prog%"\s+(.*?)\s*"%dp0%\\([^"]+)"\s+%\*`)
	// "%dp0%\path\to\tool.exe" %*   (a bin that needs no interpreter)
	shimExe = regexp.MustCompile(`(?mi)^\s*@?"%dp0%\\([^"]+\.exe)"\s+%\*`)
)

// prepareCommand points cmd at what an npm .cmd shim runs, when cmd.Path is one.
func prepareCommand(cmd *exec.Cmd) error {
	if cmd.Err != nil {
		return nil // Start reports it
	}
	ext := strings.ToLower(filepath.Ext(cmd.Path))
	if ext != ".cmd" && ext != ".bat" {
		return nil
	}
	b, err := os.ReadFile(cmd.Path)
	if err != nil {
		return nil
	}
	dir := filepath.Dir(cmd.Path)
	user := cmd.Args[1:]
	if m := shimExe.FindSubmatch(b); m != nil {
		target := filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(string(m[1]), `\`, "/")))
		if _, err := os.Stat(target); err == nil {
			cmd.Path, cmd.Args = target, append([]string{target}, user...)
			return nil
		}
	}
	if m := shimNode.FindSubmatch(b); m != nil {
		script := filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(string(m[2]), `\`, "/")))
		node := filepath.Join(dir, "node.exe") // the shim's own: IF EXIST "%dp0%\node.exe"
		if _, err := os.Stat(node); err != nil {
			node, err = exec.LookPath("node")
			if err != nil {
				node = ""
			}
		}
		if _, err := os.Stat(script); err == nil && node != "" {
			args := []string{node}
			args = append(args, shimFlags(string(m[1]), dir)...)
			args = append(args, script)
			cmd.Path, cmd.Args = node, append(args, user...)
			return nil
		}
	}
	for _, a := range user {
		if strings.ContainsAny(a, "\r\n") {
			return fmt.Errorf("%s is a .cmd file of a form piggery cannot see through, and cmd.exe would cut an argument at its newline: run it by its .exe or node script (profile cmd)", cmd.Path)
		}
	}
	return nil
}

// shimFlags splits what the shim passes its interpreter before the script ("a" b), with %dp0% the
// shim's directory.
func shimFlags(s, dir string) []string {
	s = strings.ReplaceAll(s, "%dp0%", dir)
	var out []string
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		var tok string
		if s[0] == '"' {
			i := strings.IndexByte(s[1:], '"')
			if i < 0 {
				break
			}
			tok, s = s[1:1+i], s[i+2:]
		} else {
			i := strings.IndexAny(s, " \t")
			if i < 0 {
				i = len(s)
			}
			tok, s = s[:i], s[i:]
		}
		out = append(out, tok)
	}
	return out
}
