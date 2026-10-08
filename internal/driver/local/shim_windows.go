//go:build windows

package local

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CreateProcess runs a .cmd shim through cmd.exe, which reads the line it is given by rules of its
// own: it ends the line at the first newline and expands %VAR%, so an argument of several lines
// would arrive cut after its first (Go does not quote for cmd.exe, os/exec says so). So a shim of a
// form cmdshim.go reads is not run: its target is, directly, with the arguments as they are. A shim
// of another form is run as it is, unless an argument has a newline, which it could not pass on.

// prepareCommand points cmd at what a .cmd shim runs, when cmd.Path is one.
func prepareCommand(cmd *exec.Cmd) error {
	if cmd.Err != nil {
		return nil // Start reports it
	}
	ext := strings.ToLower(filepath.Ext(cmd.Path))
	if ext != ".cmd" && ext != ".bat" {
		return nil
	}
	user := cmd.Args[1:]
	if b, err := os.ReadFile(cmd.Path); err == nil {
		if path, pre, ok := resolveShim(b, filepath.Dir(cmd.Path)); ok {
			cmd.Path, cmd.Args = path, append(append([]string{path}, pre...), user...)
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
