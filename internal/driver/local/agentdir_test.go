package local

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
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
	setHome(t, home)
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

// An omp worker's agent dir follows pi's (entries are symlinks, extensions/ a real dir of
// symlinks to what is not blacklisted, nothing of the human's dir changes) with omp's settings:
// config.yml (YAML) and the legacy settings.json are filtered copies; models.yml and agent.db
// are the human's own.
func TestBuildOmpAgentDir(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	src := filepath.Join(home, ".omp", "agent")
	repo := filepath.Join(home, "code", "piggery")
	write(t, filepath.Join(repo, "extensions", "omp", "index.ts"), "")
	write(t, filepath.Join(home, "wt", "omp", "package.json"), `{"name": "piggery-omp"}`)
	write(t, filepath.Join(home, "wt", "omp", "index.ts"), "")
	write(t, filepath.Join(src, "models.yml"), "providers: {}\n")
	write(t, filepath.Join(src, "agent.db"), "")
	write(t, filepath.Join(src, "extensions", "keep.ts"), "")
	write(t, filepath.Join(src, "extensions", "a.ts"), "")
	write(t, filepath.Join(src, "extensions", "piggery", "index.ts"), "") // setup omp's copy
	yml := "theme: dark\nmcp:\n  enableProjectConfig: true\nextensions:\n  - rel/ext-x.ts\n  - " +
		filepath.Join(home, "wt", "omp", "index.ts") + "\n  - " + filepath.Join(home, "tools", "a.ts") + "\n"
	write(t, filepath.Join(src, "config.yml"), yml)
	write(t, filepath.Join(src, "settings.json"), `{"extensions": ["rel/ext-y.ts", "`+filepath.Join(home, "tools", "a.ts")+`"]}`)
	before, _ := os.ReadFile(filepath.Join(src, "config.yml"))

	dst := filepath.Join(home, "gen", "r1")
	bl := newBlacklist([]string{"a"}, nil, home, home)
	if err := buildAgentDirWith(src, dst, home, bl, ompSettings); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"models.yml", "agent.db"} {
		if target, err := os.Readlink(filepath.Join(dst, name)); err != nil || target != filepath.Join(src, name) {
			t.Fatalf("%s -> %q, %v; want a symlink to the human's", name, target, err)
		}
	}
	exts, _ := os.ReadDir(filepath.Join(dst, "extensions"))
	var names []string
	for _, e := range exts {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"keep.ts"}) {
		t.Fatalf("extensions %v; want only keep.ts (a by name, piggery's copy skipped)", names)
	}
	var cfg struct {
		Theme string
		Mcp   struct {
			EnableProjectConfig bool `yaml:"enableProjectConfig"`
		}
		Extensions []string
	}
	b, err := os.ReadFile(filepath.Join(dst, "config.yml"))
	if err != nil || yaml.Unmarshal(b, &cfg) != nil || cfg.Theme != "dark" || !cfg.Mcp.EnableProjectConfig ||
		!slices.Equal(cfg.Extensions, []string{filepath.Join(src, "rel", "ext-x.ts")}) {
		t.Fatalf("config.yml %q (%v); want the human's keys, extensions without piggery's copy and a.ts, paths from src", b, err)
	}
	var legacy struct{ Extensions []string }
	b, err = os.ReadFile(filepath.Join(dst, "settings.json"))
	if err != nil || json.Unmarshal(b, &legacy) != nil || !slices.Equal(legacy.Extensions, []string{filepath.Join(src, "rel", "ext-y.ts")}) {
		t.Fatalf("settings.json %q (%v)", b, err)
	}
	if after, _ := os.ReadFile(filepath.Join(src, "config.yml")); string(after) != string(before) {
		t.Fatal("the human's config.yml changed")
	}
}

// A worker started by a build that kept its omp sessions in harness/omp-sessions finds them, with
// their session ids, in the new place; once the new place has sessions the old ones are left alone.
func TestOmpSessionsAreFoundAfterTheMove(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "harness", "omp-sessions", "p1")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "s1.jsonl"), []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}
	moveOmpSessions(dir, "p1")
	if b, err := os.ReadFile(filepath.Join(OmpSessionsDir(dir, "p1"), "s1.jsonl")); err != nil || string(b) != "session" {
		t.Fatalf("session after the move: %q, %v", b, err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("the old directory is still there")
	}
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	moveOmpSessions(dir, "p1")
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("a worker that has sessions in the new place had the old ones moved over them: %v", err)
	}
}
