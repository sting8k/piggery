package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	dshext "github.com/sting8k/piggery/extensions/dsh"
	ompext "github.com/sting8k/piggery/extensions/omp"
	piext "github.com/sting8k/piggery/extensions/pi"
	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/proto"
)

// What each integration installs, as a digest next to its integer: a change to an embedded
// extension tree, a hook list or a config entry piggery writes must come with a bump of
// IntegrationVersion (local/integration.go), or installed copies stay as they are. The digest
// leaves out the integer itself (the marker line, plugin.json's version, VERSION).
// To update a row: bump the integer, run the test, copy the digest it prints.
func TestIntegrationVersionsFollowWhatIsInstalled(t *testing.T) {
	trees := map[string]func() map[string][]byte{
		"pi": func() map[string][]byte {
			files := map[string][]byte{}
			err := fs.WalkDir(piext.Files, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				files[p], err = piext.Files.ReadFile(p)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return files
		},
		"omp": func() map[string][]byte { return treeOf(t, ompext.Tree) },
		"dsh": func() map[string][]byte { return treeOf(t, dshext.Tree) },
		"claude": func() map[string][]byte {
			root := t.TempDir()
			if err := writeClaudePlugin(root, "/x/piggery"); err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			for _, f := range []string{".claude-plugin/marketplace.json", "piggery/hooks/hooks.json"} { // not plugin.json: its version is the integer
				b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
				if err != nil {
					t.Fatal(err)
				}
				files[f] = b
			}
			return files
		},
		"codex": func() map[string][]byte {
			hooks, _, err := mergeCodexHooks(nil, "/h/hooks.json", "/x/piggery")
			if err != nil {
				t.Fatal(err)
			}
			block := strings.Replace(codexBlock("/h", "/x/piggery", nil), codexMarker(local.IntegrationVersion("codex"))+"\n", "", 1)
			return map[string][]byte{"hooks.json": hooks, "config.toml": []byte(block)}
		},
		"paseo": func() map[string][]byte {
			files := treeOf(t, func() (map[string][]byte, error) { return paseoFiles("/x/piggery") })
			delete(files, "VERSION")
			return files
		},
	}
	for _, want := range []struct {
		name    string
		version int
		digest  string
	}{
		{"pi", 4, "7d79e896900e"},
		{"omp", 4, "d80cefbddb38"},
		{"dsh", 5, "dc7a055fb8c8"},
		{"claude", 1, "ca51aeba0c80"},
		{"codex", 1, "c914fad6023c"},
		{"paseo", 3, "93664ce8a897"},
	} {
		if runtime.GOOS == "windows" && (want.name == "claude" || want.name == "codex") {
			continue // their hook commands are written in another form there (hookcmd_windows.go): the unix run holds the digest
		}
		if got := digestOf(trees[want.name]()); got != want.digest || local.IntegrationVersion(want.name) != want.version {
			t.Errorf("changed %s: bump IntegrationVersion and update this digest (now v%d %s; table has v%d %s)",
				want.name, local.IntegrationVersion(want.name), got, want.version, want.digest)
		}
	}
}

func treeOf(t *testing.T, tree func() (map[string][]byte, error)) map[string][]byte {
	t.Helper()
	files, err := tree()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// digestOf is a short hash of the files (names and bytes, in name order).
func digestOf(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n + "\x00"))
		h.Write(files[n])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Files setup left in ~/.piggery/claude are not an install: only what Claude itself has (its user
// MCP server, marketplace or plugin) is, the same answer as setup's status. Seen live: `setup` said
// "not installed" while ps, top and the daemon log said "claude (v0 < v1)".
func TestClaudeLeftoverFilesAreNotAnInstall(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	dir := filepath.Join(home, ".piggery")
	if err := writeClaudePlugin(filepath.Join(dir, "claude"), "/x/piggery"); err != nil {
		t.Fatal(err)
	}
	pj := filepath.Join(dir, "claude", "piggery", ".claude-plugin", "plugin.json")
	if err := os.WriteFile(pj, []byte(`{"name":"piggery","version":"v0.3.0"}`), 0o644); err != nil { // from before the integers
		t.Fatal(err)
	}
	if _, ok := integrationOf(dir, "claude"); ok || len(DaemonOutdated(dir)) != 0 {
		t.Fatalf("leftover files with nothing in Claude: installed %v, outdated %v", ok, DaemonOutdated(dir))
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"piggery":{"command":"/x/piggery","args":["mcp"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := proto.Notice(DaemonOutdated(dir)); !strings.Contains(n, "claude (v0 < v1)") {
		t.Fatalf("Claude has piggery's MCP server and the plugin files are old: notice %q", n)
	}
}
