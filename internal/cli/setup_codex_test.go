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

	if _, err := installCodex(home, "/bin/piggery"); err != nil {
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
	if msg, err := installCodex(home, "/bin/piggery"); err != nil || !strings.Contains(msg, "already set up") {
		t.Fatalf("second run: %q %v", msg, err)
	}
	if _, err := installCodex(home, "/opt/piggery"); err != nil {
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
	if _, err := installCodex(home, "/bin/piggery"); err != nil {
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
	if _, err := installCodex(empty, "/bin/piggery"); err != nil {
		t.Fatal(err)
	}
	removeCodex(empty)
	if left, _ := os.ReadDir(empty); len(left) != 0 {
		t.Fatalf("left in an empty home: %v", left)
	}
}
