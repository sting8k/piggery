//go:build unix

package cli

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// parentAndName is pid's parent and its command's base name (ps); ok false when pid is not there.
func parentAndName(pid int) (ppid int, name string, ok bool) {
	out, err := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
	f := strings.Fields(string(out))
	if err != nil || len(f) < 2 {
		return 0, "", false
	}
	ppid, _ = strconv.Atoi(f[0])
	return ppid, filepath.Base(strings.Join(f[1:], " ")), true
}

// processArgs is pid's command line split at spaces (ps), nil when unknown.
func processArgs(pid int) []string {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}
