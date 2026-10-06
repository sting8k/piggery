package manifests

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// Upgrading piggery: a built-in file the user has not changed is updated, a changed file is
// kept, a template the user deleted is not unpacked again, a new built-in is added; the home
// is the only place Resolve reads, and built-ins come back inlined.
func TestUnpackUpgrade(t *testing.T) {
	home := t.TempDir()
	v1 := fstest.MapFS{
		"sup.yaml":        {Data: []byte("template: sup\nroles:\n  lead: {instructions_file: prompts/lead.md}\n  dev: {instructions_file: prompts/dev.md}\n")},
		"prompts/lead.md": {Data: []byte("lead v1")},
		"prompts/dev.md":  {Data: []byte("dev v1")},
		"gone.yaml":       {Data: []byte("template: gone\n")},
	}
	if err := unpack(home, v1); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(Dir(home), rel))
		if err != nil {
			return "(missing)"
		}
		return string(b)
	}
	if read("sup/prompts/lead.md") != "lead v1" || read("gone/manifest.yaml") != "template: gone\n" {
		t.Fatalf("first unpack: lead=%q gone=%q", read("sup/prompts/lead.md"), read("gone/manifest.yaml"))
	}
	// The user edits one prompt and deletes a template.
	if err := os.WriteFile(filepath.Join(Dir(home), "sup/prompts/dev.md"), []byte("my dev"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(Dir(home), "gone")); err != nil {
		t.Fatal(err)
	}
	v2 := fstest.MapFS{
		"sup.yaml":        v1["sup.yaml"],
		"prompts/lead.md": {Data: []byte("lead v2")},
		"prompts/dev.md":  {Data: []byte("dev v2")},
		"gone.yaml":       v1["gone.yaml"],
		"new.yaml":        {Data: []byte("template: new\n")},
	}
	if err := unpack(home, v2); err != nil {
		t.Fatal(err)
	}
	if got := read("sup/prompts/lead.md"); got != "lead v2" {
		t.Fatalf("unchanged file = %q; want updated to v2", got)
	}
	if got := read("sup/prompts/dev.md"); got != "my dev" {
		t.Fatalf("user's file = %q; want kept", got)
	}
	if got := read("gone/manifest.yaml"); got != "(missing)" {
		t.Fatalf("deleted template came back: %q", got)
	}
	if got := read("new/manifest.yaml"); got != "template: new\n" {
		t.Fatalf("new built-in = %q; want unpacked", got)
	}
	// Resolve reads the home only, with the prompts inlined; nothing else is a template.
	m, err := Resolve("sup", home)
	if err != nil || !strings.Contains(m, "lead v2") || !strings.Contains(m, "my dev") || strings.Contains(m, "instructions_file") {
		t.Fatalf("resolve sup = %q, %v", m, err)
	}
	if _, err := Resolve("p2p", home); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("p2p (built into the binary, not in this home): %v; want not found", err)
	}
	// A file the user edited to exactly the next version is ours again: v3 updates it.
	if err := os.WriteFile(filepath.Join(Dir(home), "sup/prompts/dev.md"), []byte("dev v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unpack(home, v2); err != nil {
		t.Fatal(err)
	}
	v3 := fstest.MapFS{"sup.yaml": v1["sup.yaml"], "prompts/lead.md": v2["prompts/lead.md"],
		"prompts/dev.md": {Data: []byte("dev v3")}, "gone.yaml": v1["gone.yaml"], "new.yaml": v2["new.yaml"]}
	if err := unpack(home, v3); err != nil {
		t.Fatal(err)
	}
	if got := read("sup/prompts/dev.md"); got != "dev v3" {
		t.Fatalf("file edited to the built-in then upgraded = %q; want dev v3", got)
	}
}

// template new copies the current built-in under a new name (its model renamed, nothing else
// changed) and never over an existing one.
func TestNew(t *testing.T) {
	home := t.TempDir()
	dir, err := New(home, "mine", "lead-peer")
	if err != nil {
		t.Fatal(err)
	}
	if m, err := Resolve("mine", home); err != nil || !strings.Contains(m, "instructions:") {
		t.Fatalf("new template %s: %v\n%s", dir, err, m)
	}
	// Only the model line changes (it names the team): the rest of the file, comments included.
	orig, _ := builtin.ReadFile("lead-peer.yaml")
	got, _ := os.ReadFile(filepath.Join(dir, ManifestFile))
	if want := strings.Replace(string(orig), "template: lead-peer", "template: mine", 1); string(got) != want {
		t.Fatalf("copied manifest:\n%s\nwant:\n%s", got, want)
	}
	if _, err := New(home, "mine", "p2p"); err == nil {
		t.Fatal("new over an existing template: want refused")
	}
}

// A manifest that names itself with `model:` gets `template:` and no other byte changes; one that
// has `template:` is left alone; an unedited built-in so migrated is still tracked (the next
// Unpack updates it), an edited one still is not.
func TestMigrateKey(t *testing.T) {
	home := t.TempDir()
	v1 := fstest.MapFS{"sup.yaml": {Data: []byte("model: sup   # name\nroles: {a: {spawn: {model: small}}}\n")}}
	if err := unpack(home, v1); err != nil {
		t.Fatal(err)
	}
	mine := "# mine\nmodel: mine\nroles: {a: {}}\n"
	done := "template: done\nroles: {a: {}}\n"
	for name, text := range map[string]string{"mine": mine, "done": done} {
		if err := os.MkdirAll(filepath.Join(Dir(home), name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(Dir(home), name, ManifestFile), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := MigrateKey(home)
	if err != nil || len(changed) != 2 {
		t.Fatalf("changed %v, %v; want sup and mine", changed, err)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(Dir(home), name, ManifestFile))
		return string(b)
	}
	if got := read("sup"); got != "template: sup   # name\nroles: {a: {spawn: {model: small}}}\n" {
		t.Fatalf("sup: %q", got)
	}
	if got := read("mine"); got != "# mine\ntemplate: mine\nroles: {a: {}}\n" || read("done") != done {
		t.Fatalf("mine %q, done %q", got, read("done"))
	}
	if again, err := MigrateKey(home); err != nil || len(again) != 0 {
		t.Fatalf("second run changed %v, %v", again, err)
	}
	v2 := fstest.MapFS{"sup.yaml": {Data: []byte("template: sup\nsummary: v2\nroles: {a: {}}\n")}}
	if err := unpack(home, v2); err != nil {
		t.Fatal(err)
	}
	if got := read("sup"); !strings.Contains(got, "summary: v2") {
		t.Fatalf("a migrated built-in was not updated: %q", got)
	}
}
