package local

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A worker agent dir: entries are symlinks to the human's, extensions/ keeps only what is not
// blacklisted, settings.json is filtered. One source of each kind: npm, git, a local path (by
// basename, relative, ~), an extensions/ entry, the always-blacklisted, and piggery's own
// extension (at the -e path, a copy found by its package.json name, and extensions/piggery). A dir is never
// reused.
func TestBuildAgentDir(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	src := filepath.Join(home, ".pi", "agent")
	repo := filepath.Join(home, "code", "piggery")
	write(t, filepath.Join(repo, "extensions", "pi", "index.ts"), "")
	write(t, filepath.Join(home, "wt", "pi", "package.json"), `{"name": "piggery-pi"}`)
	write(t, filepath.Join(home, "wt", "pi", "index.ts"), "")
	write(t, filepath.Join(src, "auth.json"), "{}")
	write(t, filepath.Join(src, "extensions", "keep.ts"), "")
	write(t, filepath.Join(src, "extensions", "a.ts"), "")                // by name
	write(t, filepath.Join(src, "extensions", "pi-peer", "index.ts"), "") // always
	write(t, filepath.Join(src, "extensions", "piggery", "index.ts"), "") // setup pi's copy, by its name
	if err := os.Symlink(filepath.Join(repo, "extensions", "pi"), filepath.Join(src, "extensions", "pig")); err != nil {
		t.Fatal(err) // piggery itself, under another name
	}
	write(t, filepath.Join(src, "settings.json"), `{
		"theme": "x",
		"extensions": ["rel/ext-x.ts", "~/tools/pi-askuserquestion.ts", "`+filepath.Join(repo, "extensions", "pi")+`",
			"`+filepath.Join(home, "wt", "pi", "index.ts")+`"],
		"packages": ["npm:@scope/blocked-npm@1.2.3", "git:github.com/u/blocked-git@v1", {"source": "npm:ok-pkg", "skills": ["s"]},
			"./local/blocked-local", "github.com/u/kept-local"]}`)

	dst := filepath.Join(home, "gen", "r1")
	bl := newBlacklist([]string{"blocked-npm", "blocked-git", "blocked-local", "a"},
		[]string{filepath.Join(repo, "extensions", "pi", "index.ts")}, home, home)
	if err := buildAgentDir(src, dst, home, bl); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "auth.json")); err != nil || target != filepath.Join(src, "auth.json") {
		t.Fatalf("auth.json -> %q, %v; want a symlink to the human's", target, err)
	}
	exts, _ := os.ReadDir(filepath.Join(dst, "extensions"))
	var names []string
	for _, e := range exts {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"keep.ts"}) {
		t.Fatalf("extensions/ = %v, want [keep.ts]", names)
	}
	var got map[string]any
	b, _ := os.ReadFile(filepath.Join(dst, "settings.json"))
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"theme":      "x",
		"extensions": []any{filepath.Join(src, "rel", "ext-x.ts")},
		"packages": []any{
			map[string]any{"source": "npm:@scope/blocked-npm@1.2.3", "extensions": []any{}},
			map[string]any{"source": "git:github.com/u/blocked-git@v1", "extensions": []any{}},
			map[string]any{"source": "npm:ok-pkg", "skills": []any{"s"}},
			map[string]any{"source": filepath.Join(src, "local", "blocked-local"), "extensions": []any{}},
			filepath.Join(src, "github.com", "u", "kept-local"), // no prefix: a local path
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings.json =\n%s\nwant %v", b, want)
	}
	if err := buildAgentDir(src, dst, home, bl); err == nil {
		t.Fatal("an existing dir was reused")
	}
}

// A fresh machine (no ~/.pi/agent) still gets a dir: an empty extensions/ and nothing else.
func TestBuildAgentDirFreshMachine(t *testing.T) {
	home := t.TempDir()
	dst := filepath.Join(home, "gen", "r1")
	if err := buildAgentDir(filepath.Join(home, ".pi", "agent"), dst, home, newBlacklist(nil, nil, home, home)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 1 || entries[0].Name() != "extensions" {
		t.Fatalf("fresh dir = %v", entries)
	}
}

// A daemon started from inside a worker inherits the worker's PI_CODING_AGENT_DIR; the source
// is then the human's ~/.pi/agent, never a generated dir.
func TestHumanAgentDirIgnoresAGeneratedDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := AgentDirRoot(filepath.Join(home, ".piggery"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "p1", "r1"))
	if got := HumanAgentDir(root); got != filepath.Join(home, ".pi", "agent") {
		t.Fatalf("source = %s", got)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "~/elsewhere")
	if got := HumanAgentDir(root); got != filepath.Join(home, "elsewhere") {
		t.Fatalf("source = %s", got)
	}
}
