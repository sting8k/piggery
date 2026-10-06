package server

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/manifests"
)

// EnsureFiles gives an old profile and a template the user wrote the keys they lack, keeping
// what they have (a comment too), and writes a profile that is missing; a second run changes nothing; a template its parser refuses is
// left as it is and named.
func TestEnsureFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "harness"), 0o700)
	os.WriteFile(local.CodexProfilePath(dir), []byte("{\n  \"cmd\": \"codex\"\n}\n"), 0o600)
	mine := filepath.Join(manifests.Dir(dir), "mine", manifests.ManifestFile)
	os.MkdirAll(filepath.Dir(mine), 0o700)
	tmpl := "template: mine   # my own\nroles:\n  lead:\n    instructions: \"Lead.\"\n"
	os.WriteFile(mine, []byte(tmpl), 0o600)
	bad := filepath.Join(manifests.Dir(dir), "bad", manifests.ManifestFile)
	os.MkdirAll(filepath.Dir(bad), 0o700)
	os.WriteFile(bad, []byte("roles: {}\n"), 0o600)

	filled, errs := EnsureFiles(dir)
	if _, err := os.Stat(local.ProfilePath(dir)); err != nil {
		t.Fatalf("a missing profile was not written: %v", err)
	}
	got := map[string]string{}
	for _, f := range filled {
		got[f.Path] = strings.Join(f.Added, ",")
	}
	b, _ := os.ReadFile(mine)
	if !strings.Contains(got[local.CodexProfilePath(dir)], "blacklist") || !strings.Contains(got[mine], "roles.lead.spawn") ||
		!strings.HasPrefix(string(b), tmpl) || len(errs) != 1 || !strings.Contains(errs[0].Error(), "bad") {
		t.Fatalf("filled %v, errs %v, template:\n%s", got, errs, b)
	}
	if c, _ := os.ReadFile(local.CodexProfilePath(dir)); !strings.HasPrefix(string(c), "{\n  \"cmd\": \"codex\",\n  \"args\"") {
		t.Fatalf("a present profile lost its bytes:\n%s", c)
	}
	if again, errs := EnsureFiles(dir); len(again) != 0 || len(errs) != 1 {
		t.Fatalf("second run: %v %v", again, errs)
	}
}

// The shared prompt piggery ships: a first run writes its file and its config entry; once the user
// removes the entry, later runs leave it removed. Into a list the user has, the entry goes after
// the last item and every byte of the user's stays.
func TestEnsureFilesBuiltinPrompt(t *testing.T) {
	dir := t.TempDir()
	if _, errs := EnsureFiles(dir); len(errs) != 0 {
		t.Fatal(errs)
	}
	want := []PromptEntry{{File: "rules/general-policy.md", Roles: []string{"lead-peer/*", "slp/*"}}}
	set, err := LoadSettings(dir)
	if _, ferr := os.Stat(filepath.Join(dir, "rules", "general-policy.md")); err != nil || ferr != nil || !reflect.DeepEqual(set.Prompts, want) {
		t.Fatalf("first run: prompts %+v, %v, file %v", set.Prompts, err, ferr)
	}
	b, _ := os.ReadFile(ConfigPath(dir))
	os.WriteFile(ConfigPath(dir), []byte(strings.Replace(string(b), "\n  - file: rules/general-policy.md\n    roles: [\"lead-peer/*\", \"slp/*\"]", " []", 1)), 0o600)
	if set, _ := LoadSettings(dir); len(set.Prompts) != 0 {
		t.Fatalf("entry not removed: %+v", set.Prompts)
	}
	if filled, _ := EnsureFiles(dir); len(filled) != 0 {
		t.Fatalf("an entry the user removed came back: %v", filled)
	}

	mine := "prompts:\n  - file: rules/mine.md   # mine\n    roles: [\"*\"]\n\n# update\nupdate:\n  check: true\n"
	os.WriteFile(ConfigPath(dir), []byte(mine), 0o600)
	if added, err := AddPrompt(dir, want[0]); !added || err != nil {
		t.Fatalf("AddPrompt = %v, %v", added, err)
	}
	got, _ := os.ReadFile(ConfigPath(dir))
	if exp := strings.Replace(mine, "\n\n", "\n  - file: rules/general-policy.md\n    roles: [\"lead-peer/*\", \"slp/*\"]\n\n", 1); string(got) != exp {
		t.Fatalf("config:\n%s\nwant:\n%s", got, exp)
	}
}
