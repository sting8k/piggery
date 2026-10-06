package local

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	ompext "github.com/sting8k/piggery/extensions/omp"
	piext "github.com/sting8k/piggery/extensions/pi"
)

// The omp extension is one tree in two places (the Human's and a worker's), written from the binary
// and marked with its integration version: the layout has the manifest, the entry and the shared pi
// files (the same bytes as extensions/pi), a current copy is not rewritten, and neither is one at
// the same integer (a rebuild); one with a lower integer, or a build-version marker from before
// the integers, is updated once; a directory that is not piggery's is never replaced or removed,
// and a worker gets the entry to pass to `omp -e`.
func TestOmpExtCopy(t *testing.T) {
	ext := filepath.Join(t.TempDir(), "extensions", "piggery")
	if wrote, err := InstallOmpExt(ext); err != nil || !wrote {
		t.Fatalf("install: %v, %v", wrote, err)
	}
	for name, want := range map[string]string{"pi/adapter.mjs": "adapter.mjs", "pi/client.mjs": "client.mjs", "pi/render.mjs": "render.mjs", "pi/tools.json": "tools.json"} {
		got, err := os.ReadFile(filepath.Join(ext, filepath.FromSlash(name)))
		orig, _ := piext.Files.ReadFile(want)
		if err != nil || string(got) != string(orig) {
			t.Errorf("%s is not extensions/pi/%s: %v", name, want, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ext, "package.json")); !strings.Contains(string(b), `"name": "`+ompext.PackageName+`"`) ||
		!strings.Contains(string(b), `"./omp/index.ts"`) {
		t.Errorf("package.json = %s", b)
	}
	if v, ok := OmpExtVersion(ext); !ok || v != IntegrationVersion("omp") || !OmpExtCurrent(ext) {
		t.Fatalf("version %d managed %v", v, ok)
	}
	if wrote, err := InstallOmpExt(ext); err != nil || wrote {
		t.Fatalf("second install wrote again: %v, %v", wrote, err)
	}
	entryFile := filepath.Join(ext, "omp", "index.ts")
	cur, _ := os.ReadFile(entryFile)
	os.WriteFile(entryFile, append(cur, "// edited\n"...), 0o644)
	if up, _ := UpdateOmpExt(ext); up {
		t.Fatal("a copy at this binary's integer was rewritten (a rebuild does not change it)")
	}
	_, rest, _ := strings.Cut(string(cur), "\n")
	os.WriteFile(entryFile, []byte("// managed by piggery v0.3.0: written by `piggery setup omp`\n"+rest), 0o644)
	if v, _ := OmpExtVersion(ext); v != 0 {
		t.Fatalf("a build-version marker reads as v%d, want v0", v)
	}
	if up, err := UpdateOmpExt(ext); err != nil || !up || !OmpExtCurrent(ext) {
		t.Fatalf("outdated copy: updated %v, %v", up, err)
	}
	if removed, err := RemoveOmpExt(ext); err != nil || !removed {
		t.Fatalf("remove: %v, %v", removed, err)
	}
	if removed, _ := RemoveOmpExt(ext); removed {
		t.Fatal("removed twice")
	}

	// Not piggery's (no marker): never replaced, never removed.
	other := filepath.Join(t.TempDir(), "piggery")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "index.ts"), []byte("// mine\n"), 0o644)
	if _, err := InstallOmpExt(other); err == nil {
		t.Error("install replaced a directory that is not piggery's")
	}
	if _, err := RemoveOmpExt(other); err == nil {
		t.Error("remove deleted a directory that is not piggery's")
	}

	// A worker: the copy piggery owns, and the file for `-e`. A current copy is left as it is.
	dir := t.TempDir()
	entry, err := EnsureOmpWorkerExt(dir)
	if err != nil || entry != filepath.Join(dir, "plugins", "omp", "omp", "index.ts") {
		t.Fatalf("worker entry %q, %v", entry, err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(entry, old, old)
	if _, err := EnsureOmpWorkerExt(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(entry); fi.ModTime().After(old.Add(time.Minute)) {
		t.Error("a current worker copy was rewritten")
	}
}

// Every ../pi/<file> the entry imports is in the copy, and every shared file is imported: the
// list in ompext.Shared and the imports of omp/index.ts cannot drift apart.
func TestOmpEntryImportsMatchShared(t *testing.T) {
	tree, err := ompext.Tree()
	if err != nil {
		t.Fatal(err)
	}
	imported := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.\./pi/([A-Za-z0-9_.-]+)`).FindAllStringSubmatch(string(tree[ompext.Entry]), -1) {
		imported[m[1]] = true
		if _, ok := tree["pi/"+m[1]]; !ok {
			t.Errorf("omp/index.ts imports ../pi/%s, which the copy does not carry", m[1])
		}
	}
	for _, name := range ompext.Shared {
		if !imported[name] {
			t.Errorf("ompext.Shared has %s, which omp/index.ts does not import", name)
		}
	}
}

// omp's agent dir: a named profile wins over PI_CODING_AGENT_DIR (as omp does), PI_CODING_AGENT_DIR
// over the default, and one under the per-run root (a worker's) is ignored.
func TestOmpHumanAgentDir(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PI_CONFIG_DIR", "")
	t.Setenv("PI_PROFILE", "")
	t.Setenv("OMP_PROFILE", "")
	os.Unsetenv("OMP_PROFILE") // unset, not empty: an empty one would win over PI_PROFILE below
	custom := filepath.Join(t.TempDir(), "agent")
	t.Setenv("PI_CODING_AGENT_DIR", custom)
	root := filepath.Join(t.TempDir(), "omp-agent")
	if got := OmpHumanAgentDir(root); got != custom {
		t.Errorf("PI_CODING_AGENT_DIR: %s, want %s", got, custom)
	}
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "w1", "run1"))
	if got, want := OmpHumanAgentDir(root), filepath.Join(home, ".omp", "agent"); got != want {
		t.Errorf("under the worker root: %s, want %s", got, want)
	}
	t.Setenv("PI_CODING_AGENT_DIR", custom)
	t.Setenv("PI_PROFILE", "work")
	if got, want := OmpHumanAgentDir(root), filepath.Join(home, ".omp", "profiles", "work", "agent"); got != want {
		t.Errorf("profile: %s, want %s", got, want)
	}
	t.Setenv("OMP_PROFILE", "") // an OMP_PROFILE that is set, even empty, wins over PI_PROFILE: the default profile
	if got := OmpHumanAgentDir(root); got != custom {
		t.Errorf("empty OMP_PROFILE: %s, want %s", got, custom)
	}
}
