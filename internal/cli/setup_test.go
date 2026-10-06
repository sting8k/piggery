package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/driver/local"
)

// setup keeps a profile the human already has unless --force.
func TestWriteProfileKeepsExistingWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := local.ProfilePath(dir)
	if ok, err := writeJSON(path, local.DefaultProfile("/x/extensions/pi/index.ts"), false); err != nil || !ok {
		t.Fatalf("first write: %v %v", ok, err)
	}
	if st, _ := os.Stat(path); !permIs(st.Mode().Perm(), 0o600) {
		t.Fatalf("profile mode %v", st.Mode().Perm())
	}
	if err := os.WriteFile(path, []byte(`{"cmd":"mine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := writeJSON(path, local.DefaultProfile("/y/index.ts"), false); err != nil || ok {
		t.Fatalf("second write without --force: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"cmd":"mine"}` {
		t.Fatalf("profile overwritten: %s", b)
	}
	if ok, err := writeJSON(path, local.DefaultProfile("/y/index.ts"), true); err != nil || !ok {
		t.Fatalf("--force: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "/y/index.ts") {
		t.Fatalf("--force did not write: %s", b)
	}
}

// A harness is flagged only when older than the oldest tested version: a newer major is not, a
// pre-release is older than its release and compares by number, an unparsable version is not.
func TestOlderThanTested(t *testing.T) {
	for _, c := range []struct {
		version string
		tested  []string
		want    bool
	}{
		{"0.87.0", []string{"0.87.1"}, true},
		{"1.0.4", []string{"0.87.1"}, false},
		{"0.2.0-rc.1", []string{"0.2.0"}, true},
		{"0.2.0-rc.2", []string{"0.2.0-rc.10"}, true},
		{"nightly", []string{"1.0.0"}, false},
	} {
		if got := olderThanTested(c.version, c.tested); got != c.want {
			t.Errorf("olderThanTested(%q, %v) = %v, want %v", c.version, c.tested, got, c.want)
		}
	}
}
