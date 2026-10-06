package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeHooksList stands in for Codex's hooks/list: every handler of home/hooks.json, keyed by
// position, hashed from its command, trusted when config.toml holds that hash under that key.
func fakeHooksList(home string) ([]codexHookMeta, error) {
	path := filepath.Join(home, "hooks.json")
	b, _ := os.ReadFile(path)
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	json.Unmarshal(b, &doc)
	var metas []codexHookMeta
	for _, ev := range codexHookEvents {
		for g, grp := range doc.Hooks[ev.name] {
			for h, hd := range grp.Hooks {
				key := fmt.Sprintf("%s:%s:%d:%d", path, ev.key, g, h)
				hash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(hd.Command)))
				status := "untrusted"
				if strings.Contains(string(cfg), "[hooks.state."+strconv.Quote(key)+"]\ntrusted_hash = "+strconv.Quote(hash)) {
					status = "trusted"
				}
				metas = append(metas, codexHookMeta{Key: key, CurrentHash: hash, TrustStatus: status, SourcePath: path})
			}
		}
	}
	return metas, nil
}

// setup codex keeps the Human's hooks and config, replaces what it owns (also a piggery server
// added by hand, and trust written by /hooks), changes nothing when run again, and trusts again
// after the binary moved.
func TestInstallCodex(t *testing.T) {
	old := codexHooksList
	codexHooksList = fakeHooksList
	t.Cleanup(func() { codexHooksList = old })
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "hooks.json"), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]},"description":"mine"}`), 0o600)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"x\"\n\n[mcp_servers.piggery]\ncommand = \"old\"\n\n[mcp_servers.other]\ncommand = \"o\"\n"), 0o600)

	if _, err := installCodex(t.TempDir(), home, "/bin/piggery"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	hooks, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	if !strings.Contains(string(cfg), "model = \"x\"") || !strings.Contains(string(cfg), "[mcp_servers.other]") ||
		strings.Contains(string(cfg), "\"old\"") || strings.Count(string(cfg), "[mcp_servers.piggery]") != 1 ||
		!strings.Contains(string(cfg), "default_tools_approval_mode = \"approve\"") ||
		!strings.Contains(string(cfg), "env = { CODEX_HOME = "+strconv.Quote(home)+" }") ||
		!strings.Contains(string(hooks), "say done") || !strings.Contains(string(hooks), "\"mine\"") {
		t.Fatalf("config:\n%s\nhooks:\n%s", cfg, hooks)
	}
	if msg, err := installCodex(t.TempDir(), home, "/bin/piggery"); err != nil || !strings.Contains(msg, "already set up") {
		t.Fatalf("second run: %q %v", msg, err)
	}
	if _, err := installCodex(t.TempDir(), home, "/opt/piggery"); err != nil {
		t.Fatalf("moved binary: %v", err)
	}
	metas, _ := fakeHooksList(home)
	cfg, _ = os.ReadFile(filepath.Join(home, "config.toml"))
	if n := strings.Count(string(cfg), "[hooks.state."); n != len(codexHookEvents) || strings.Contains(string(cfg), "/bin/piggery") {
		t.Fatalf("state tables %d, config:\n%s", n, cfg)
	}
	for _, m := range metas {
		if strings.Contains(m.Key, ":stop:0:") == false && m.TrustStatus != "trusted" {
			t.Fatalf("not trusted after the move: %+v", m)
		}
	}
}

// setup remove codex gives the Human's hooks.json and config.toml back as they were before setup,
// a second remove changes nothing, and a home that had nothing before is left with nothing.
// Status names hooks that run a moved binary.
func TestRemoveCodex(t *testing.T) {
	old := codexHooksList
	codexHooksList = fakeHooksList
	t.Cleanup(func() { codexHooksList = old })
	home := t.TempDir()
	hooks := "{\n  \"description\": \"mine\",\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"say done\"\n          }\n        ]\n      }\n    ]\n  }\n}\n"
	cfg := "model = \"x\"\n\n[mcp_servers.other]\ncommand = \"o\"\n"
	os.WriteFile(filepath.Join(home, "hooks.json"), []byte(hooks), 0o600)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600)
	if _, err := installCodex(t.TempDir(), home, "/bin/piggery"); err != nil {
		t.Fatal(err)
	}
	if st := codexStatus(home, "/bin/piggery"); !st.Installed || len(st.Problems) > 0 {
		t.Fatalf("status: %+v", st)
	}
	if st := codexStatus(home, "/opt/piggery"); len(st.Problems) != 2 {
		t.Fatalf("status for a moved binary: %+v", st)
	}
	if _, err := removeCodex(home); err != nil {
		t.Fatal(err)
	}
	h, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	c, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(h) != hooks || string(c) != cfg {
		t.Fatalf("after remove:\n%s\n%s", h, c)
	}
	if msg, _ := removeCodex(home); !strings.Contains(msg, "not installed") {
		t.Fatalf("second remove: %q", msg)
	}
	empty := t.TempDir()
	if _, err := installCodex(t.TempDir(), empty, "/bin/piggery"); err != nil {
		t.Fatal(err)
	}
	removeCodex(empty)
	if left, _ := os.ReadDir(empty); len(left) != 0 {
		t.Fatalf("left in an empty home: %v", left)
	}
}

// A hook piggery writes that is missing from hooks.json (one added to piggery since) is outdated
// even at this binary's integer; a config block from before the integers (no marker) reads as v0;
// setup codex puts both right.
func TestCodexHookDriftIsOutdated(t *testing.T) {
	old := codexHooksList
	codexHooksList = fakeHooksList
	t.Cleanup(func() { codexHooksList = old })
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if _, err := installCodex(t.TempDir(), home, "/bin/piggery"); err != nil {
		t.Fatal(err)
	}
	if i, ok := integrationOf(dir, "codex"); !ok || i.outdated() {
		t.Fatalf("after setup: %+v %v", i, ok)
	}
	hooksPath := filepath.Join(home, "hooks.json")
	var doc struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	b, _ := os.ReadFile(hooksPath)
	json.Unmarshal(b, &doc)
	delete(doc.Hooks, "Interrupt")
	b, _ = json.Marshal(doc)
	os.WriteFile(hooksPath, b, 0o600)
	if i, _ := integrationOf(dir, "codex"); !i.outdated() || i.Have != i.Want || !strings.Contains(i.Drift, "6 of 7") {
		t.Fatalf("a missing hook: %+v", i)
	}
	if _, err := installCodex(t.TempDir(), home, "/bin/piggery"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(strings.Replace(string(cfg), codexMarker(1)+"\n", "", 1)), 0o600)
	if i, _ := integrationOf(dir, "codex"); i.Have != 0 || !i.outdated() || i.Drift != "" {
		t.Fatalf("a block without the marker: %+v", i)
	}
	if _, err := installCodex(t.TempDir(), home, "/bin/piggery"); err != nil {
		t.Fatal(err)
	}
	if i, _ := integrationOf(dir, "codex"); i.outdated() {
		t.Fatalf("after setup again: %+v", i)
	}
}

// `setup --outdated` runs setup for what is installed and outdated, and only that: Codex with a
// config block from before the integers is written again; Claude, which is not installed (files
// left in ~/.piggery/claude are not an install), is not touched; the second run says all is current.
func TestSetupOutdatedUpdatesWhatIsInstalled(t *testing.T) {
	old := codexHooksList
	codexHooksList = fakeHooksList
	t.Cleanup(func() { codexHooksList = old })
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(home, ".piggery")
	if _, err := installCodex(t.TempDir(), codexHome(), "/bin/piggery"); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(codexHome(), "config.toml")
	cfg, _ := os.ReadFile(cfgPath)
	os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), codexMarker(1)+"\n", "", 1)), 0o600)
	if err := writeClaudePlugin(filepath.Join(dir, "claude"), "/x/piggery"); err != nil {
		t.Fatal(err)
	}
	run := func() (string, error) {
		var out strings.Builder
		e := &env{dir: dir, stdout: &out}
		err := e.setup([]string{"--outdated"})
		return out.String(), err
	}
	out, err := run()
	if err != nil || !strings.Contains(out, "codex: updated (v0 < v1)") || strings.Contains(out, "claude") {
		t.Fatalf("first run: %q, %v", out, err)
	}
	if i, _ := integrationOf(dir, "codex"); i.outdated() {
		t.Fatalf("codex still outdated: %+v", i)
	}
	if out, err := run(); err != nil || !strings.Contains(out, "up to date") {
		t.Fatalf("second run: %q, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Fatal("setup --outdated touched Claude's config")
	}
}
