package cli

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/server"
)

// An admin-only verb runs as admin without -a; in a participant's shell (PIGGERY_ID/TOKEN set) it
// is refused unless -a is given, never silently turned into admin.
func TestAdminOnlyVerbWithoutA(t *testing.T) {
	dir, err := os.MkdirTemp(shortTmp(), "pg") // short: unix socket paths are limited on macOS
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, server.Config{Dir: dir}) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err := Dial(dir, false); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
	}
	ps := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Main(dir, append(args, "ps"), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	t.Setenv("PIGGERY_ID", "")
	t.Setenv("PIGGERY_TOKEN", "")
	if code, out := ps(); code != 0 {
		t.Fatalf("ps without -a: exit %d\n%s", code, out)
	}
	t.Setenv("PIGGERY_ID", "p1")
	t.Setenv("PIGGERY_TOKEN", "t1")
	if code, out := ps(); code != 2 {
		t.Fatalf("ps in a participant's shell: exit %d, want a refusal\n%s", code, out)
	}
	if code, out := ps("-a"); code != 0 {
		t.Fatalf("-a ps in a participant's shell: exit %d\n%s", code, out)
	}
}

// A mistyped worker or team name gets the nearest existing names; nothing close, no suggestion.
func TestClosestNames(t *testing.T) {
	names := []string{"w1", "w12", "lead", "grumpy-linus", "amber-otter"}
	for name, want := range map[string][]string{
		"w2":          {"w1", "w12"},
		"grumpy-linu": {"grumpy-linus"},
		"Lead":        {"lead"},
		"zzz":         nil,
	} {
		if got := closest(name, names); !slices.Equal(got, want) {
			t.Errorf("closest(%q) = %q, want %q", name, got, want)
		}
	}
}

// --no-start (read-only callers that poll): with no daemon running, a command fails with a clear
// message and starts none; without the flag the same command would start one.
func TestNoStart(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := Main(dir, []string{"ps", "--json", "--no-start"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "the piggery daemon is not running") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	time.Sleep(100 * time.Millisecond)
	if c, err := Dial(dir, false); err == nil {
		c.Close()
		t.Fatal("a daemon runs after --no-start")
	}
}
