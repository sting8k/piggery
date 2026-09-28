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
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
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
