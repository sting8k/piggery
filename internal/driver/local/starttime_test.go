//go:build unix

package local

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A localized LC_TIME changes how ps prints lstart ("T4 Thg 11  4 16:32:04 2026" under vi_VN).
// Start times must still be read: a Claude session's host key and worker recovery depend on them.
// The rest of the locale stays: a program whose name is not ASCII still matches the recorded one.
func TestStartTimeUnderALocalizedLCTime(t *testing.T) {
	loc := ""
	for _, l := range []string{"vi_VN.UTF-8", "fr_FR.UTF-8", "de_DE.UTF-8"} {
		cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid()))
		cmd.Env = append(os.Environ(), "LC_ALL=", "LC_TIME="+l)
		out, _ := cmd.Output()
		if _, err := time.Parse(lstartLayout, strings.Join(strings.Fields(string(out)), " ")); err != nil {
			loc = l
			break
		}
	}
	if loc == "" {
		t.Skip("no localized locale installed: ps prints lstart in English anyway")
	}
	t.Setenv("LC_ALL", loc)
	prog := filepath.Join(t.TempDir(), "café")
	if b, err := os.ReadFile("/bin/sleep"); err != nil || os.WriteFile(prog, b, 0o755) != nil {
		t.Skip("cannot copy /bin/sleep")
	}
	cmd := exec.Command(prog, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if ProcessStartTime(cmd.Process.Pid) == 0 {
		t.Errorf("ProcessStartTime: 0 under LC_ALL=%s", loc)
	}
	start, command, alive, err := psInfo(context.Background(), cmd.Process.Pid)
	if err != nil || !alive || start == 0 || !sameProgram(command, prog) {
		t.Errorf("psInfo under LC_ALL=%s: start=%d alive=%v err=%v command %q; want %s's start and name", loc, start, alive, err, command, prog)
	}
}
