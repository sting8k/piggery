package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/manifests"
)

// EnsureFiles gives an old profile and a template the user wrote the keys they lack, keeping
// what they have (a comment too); a second run changes nothing; a template its parser refuses is
// left as it is and named.
func TestEnsureFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "harness"), 0o700)
	os.WriteFile(local.CodexProfilePath(dir), []byte("{\n  \"cmd\": \"codex\"\n}\n"), 0o600)
	mine := filepath.Join(manifests.Dir(dir), "mine", manifests.ManifestFile)
	os.MkdirAll(filepath.Dir(mine), 0o700)
	tmpl := "model: mine   # my own\nroles:\n  lead:\n    instructions: \"Lead.\"\n"
	os.WriteFile(mine, []byte(tmpl), 0o600)
	bad := filepath.Join(manifests.Dir(dir), "bad", manifests.ManifestFile)
	os.MkdirAll(filepath.Dir(bad), 0o700)
	os.WriteFile(bad, []byte("roles: {}\n"), 0o600)

	filled, errs := EnsureFiles(dir)
	got := map[string]string{}
	for _, f := range filled {
		got[f.Path] = strings.Join(f.Added, ",")
	}
	b, _ := os.ReadFile(mine)
	if !strings.Contains(got[local.CodexProfilePath(dir)], "blacklist") || !strings.Contains(got[mine], "roles.lead.spawn") ||
		!strings.HasPrefix(string(b), tmpl) || len(errs) != 1 || !strings.Contains(errs[0].Error(), "bad") {
		t.Fatalf("filled %v, errs %v, template:\n%s", got, errs, b)
	}
	if again, errs := EnsureFiles(dir); len(again) != 0 || len(errs) != 1 {
		t.Fatalf("second run: %v %v", again, errs)
	}
}
