//go:build unix

package local

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// psInfo reads the OS start time (unix ms) and command of pid. alive=false when ps reports no
// such process; err when the process table could not be read.
func psInfo(ctx context.Context, pid int) (start int64, command string, alive bool, err error) {
	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "lstart=,command=", "-p", strconv.Itoa(pid)).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && len(strings.TrimSpace(string(out))) == 0 {
		return 0, "", false, nil // ps: no such process
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("read process table: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) < 6 {
		return 0, "", false, fmt.Errorf("read process table: unexpected ps output %q", out)
	}
	t, err := time.ParseInLocation(lstartLayout, strings.Join(f[:5], " "), time.Local)
	if err != nil {
		return 0, "", false, fmt.Errorf("read process table: %w", err)
	}
	return t.UnixMilli(), strings.Join(f[5:], " "), true, nil
}

// programName is how a program is compared with the recorded one: the name as it is.
func programName(s string) string { return s }

// sameProgram: command (what ps shows) is the recorded program: the basename of its first token, or
// of its second when an interpreter runs a script.
func sameProgram(command, recorded string) bool {
	name := filepath.Base(recorded)
	tok := strings.Fields(command)
	name = programName(name)
	return len(tok) > 0 && (programName(filepath.Base(tok[0])) == name || (len(tok) > 1 && programName(filepath.Base(tok[1])) == name))
}

// startedProgram is the program recorded for a worker started as cmd: the command itself.
func startedProgram(_ int, cmd string) string { return cmd }
