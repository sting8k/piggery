//go:build unix

package local

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A localized LC_TIME changes how ps prints lstart ("T4 Thg 11  4 16:32:04 2026" under vi_VN).
// Start times must still be read: a Claude session's host key and worker recovery depend on them.
func TestStartTimeUnderALocalizedLCTime(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_TIME", "vi_VN.UTF-8")
	pid := os.Getpid()
	out, _ := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if _, err := time.Parse(lstartLayout, strings.Join(strings.Fields(string(out)), " ")); err == nil {
		t.Skip("vi_VN locale not installed: ps prints lstart in English anyway")
	}
	if ProcessStartTime(pid) == 0 {
		t.Error("ProcessStartTime: 0 under LC_TIME=vi_VN.UTF-8")
	}
	if _, _, alive, err := psInfo(context.Background(), pid); err != nil || !alive {
		t.Errorf("psInfo: alive=%v err=%v under LC_TIME=vi_VN.UTF-8", alive, err)
	}
}
