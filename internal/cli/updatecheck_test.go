package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/server"
)

// The daily update check, against a fake release endpoint: a newer tag shows in ps --view, a
// restart within the day makes no second call, and a dev-<sha> build or update.check: false makes none.
func TestDailyUpdateCheck(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"tag_name":"v9.9.9"}`)
	}))
	defer api.Close()
	dir, err := os.MkdirTemp(shortTmp(), "pg") // short: unix socket paths are limited on macOS
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// run is a daemon of build v0.1.0; the notice is what ps --view says of the check.
	run := func(version string, wait func(notice string) bool) (notice string) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- server.Run(ctx, server.Config{Dir: dir, Version: version, Latest: latestTag(api.URL)})
		}()
		defer func() { cancel(); <-done }()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			var out, errOut bytes.Buffer
			if Main(dir, []string{"ps", "--view"}, &out, &errOut) == 0 {
				var d struct{ Daemon struct{ Update string } }
				if err := json.Unmarshal(out.Bytes(), &d); err != nil {
					t.Fatal(err)
				}
				notice = d.Daemon.Update
				if wait(notice) || time.Now().After(deadline) {
					return notice
				}
			} else if time.Now().After(deadline) {
				t.Fatalf("ps --view: %s", errOut.String())
			}
		}
	}
	if got := run("v0.1.0", func(n string) bool { return n != "" }); got != "v9.9.9 available: piggery update" || calls.Load() != 1 {
		t.Fatalf("first run: notice %q after %d calls", got, calls.Load())
	}
	start := time.Now()
	if got := run("v0.1.0", func(string) bool { return time.Since(start) > 2*time.Second }); got != "v9.9.9 available: piggery update" || calls.Load() != 1 {
		t.Fatalf("restart within the day: notice %q after %d calls", got, calls.Load())
	}
	// A local deploy (dev-<sha>) is no release: with a stale cache it asks nothing and says nothing.
	start = time.Now()
	if got := run("dev-8e4b891", func(string) bool { return time.Since(start) > 2*time.Second }); got != "" || calls.Load() != 1 {
		t.Fatalf("dev build: notice %q after %d calls", got, calls.Load())
	}
	os.Remove(filepath.Join(dir, "cache", "update.json"))
	os.WriteFile(server.ConfigPath(dir), []byte("update:\n  check: false\n"), 0o600)
	start = time.Now()
	if got := run("v0.1.0", func(string) bool { return time.Since(start) > 2*time.Second }); got != "" || calls.Load() != 1 {
		t.Fatalf("update.check false: notice %q after %d calls", got, calls.Load())
	}
}
