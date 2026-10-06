//go:build windows

package local

import (
	"os"
	"path/filepath"
	"testing"
)

// Without the privilege to make a symlink, a worker's agent dir still shares the human's entries
// instead of copying them: what is written through the stand-in of a directory or of a file lands
// in the original, and removing the run's directory (as the end of a run does) removes the
// stand-ins only.
func TestStandInSharesAndIsRemovedAlone(t *testing.T) {
	src, run := t.TempDir(), filepath.Join(t.TempDir(), "run")
	sessions := filepath.Join(src, "sessions")
	auth := filepath.Join(src, "auth.json")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sessions, "old.jsonl"), []byte("old"), 0o600)
	os.WriteFile(auth, []byte("token-1"), 0o600)
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sessions", "auth.json"} {
		if err := standIn(filepath.Join(src, name), filepath.Join(run, name)); err != nil {
			t.Fatalf("stand in for %s: %v", name, err)
		}
	}

	if b, err := os.ReadFile(filepath.Join(run, "sessions", "old.jsonl")); err != nil || string(b) != "old" {
		t.Fatalf("read through the directory's stand-in = %q, %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(run, "sessions", "new.jsonl"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(sessions, "new.jsonl")); err != nil || string(b) != "new" {
		t.Fatalf("a session the worker wrote is not in the human's directory: %q, %v (a copy was made)", b, err)
	}
	f, err := os.OpenFile(filepath.Join(run, "auth.json"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("token-2")
	f.Close()
	if b, _ := os.ReadFile(auth); string(b) != "token-2" {
		t.Fatalf("a login the worker refreshed is not the human's: %q (a copy was made)", b)
	}

	if err := os.RemoveAll(run); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{auth, filepath.Join(sessions, "old.jsonl"), filepath.Join(sessions, "new.jsonl")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("removing the run's directory removed the human's %s: %v", p, err)
		}
	}
}
