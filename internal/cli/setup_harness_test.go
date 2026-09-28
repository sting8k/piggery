package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/driver/local"
)

// TestMain lets this test binary stand in for `claude` (fakeClaude) and `paseo` (fakePaseo) when
// a test puts it on PATH under that name.
func TestMain(m *testing.M) {
	if log := os.Getenv("PIGGERY_FAKE_CLAUDE"); log != "" && filepath.Base(os.Args[0]) == "claude" {
		os.Exit(fakeClaude(log, os.Args[1:]))
	}
	if log := os.Getenv("PIGGERY_FAKE_PASEO"); log != "" && filepath.Base(os.Args[0]) == "paseo" {
		os.Exit(fakePaseo(log, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeClaude does what the claude commands setup runs do to Claude's state, kept in $HOME:
// ~/.claude.json (mcpServers, like Claude) and ~/fake-claude.json (marketplaces and plugins,
// listed with --json). Installing copies the plugin, as Claude does. argv goes to log.
func fakeClaude(log string, args []string) int {
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	f.WriteString(strings.Join(args, " ") + "\n")
	f.Close()
	home, _ := os.UserHomeDir()
	cfgPath, statePath := filepath.Join(home, ".claude.json"), filepath.Join(home, "fake-claude.json")
	cfg := map[string]any{}
	if b, err := os.ReadFile(cfgPath); err == nil {
		json.Unmarshal(b, &cfg)
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	type mk struct{ Name, Path string }
	type pl struct {
		ID          string `json:"id"`
		Enabled     bool   `json:"enabled"`
		InstallPath string `json:"installPath"`
	}
	var state struct {
		Marketplaces []mk
		Plugins      []pl
	}
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &state)
	}
	cmd := strings.Join(args[:min(3, len(args))], " ")
	switch {
	case cmd == "mcp add --scope": // mcp add --scope user <name> -- <command> <args>
		servers[args[4]] = map[string]any{"type": "stdio", "command": args[6], "args": args[7:]}
	case cmd == "mcp remove --scope":
		delete(servers, args[4])
	case cmd == "plugin marketplace list":
		b, _ := json.Marshal(state.Marketplaces)
		os.Stdout.Write(b)
	case cmd == "plugin marketplace add":
		var m struct{ Name string }
		b, _ := os.ReadFile(filepath.Join(args[3], ".claude-plugin", "marketplace.json"))
		json.Unmarshal(b, &m)
		state.Marketplaces = append(state.Marketplaces, mk{m.Name, args[3]})
	case cmd == "plugin marketplace remove":
		state.Marketplaces = slices.DeleteFunc(state.Marketplaces, func(m mk) bool { return m.Name == args[3] })
	case cmd == "plugin list --json":
		b, _ := json.Marshal(state.Plugins)
		os.Stdout.Write(b)
	case args[0] == "plugin" && args[1] == "install":
		name, market, _ := strings.Cut(args[2], "@")
		i := slices.IndexFunc(state.Marketplaces, func(m mk) bool { return m.Name == market })
		if i < 0 {
			os.Stderr.WriteString("no marketplace " + market)
			return 1
		}
		dir := filepath.Join(home, ".claude", "plugins", "cache", market, name)
		b, _ := os.ReadFile(filepath.Join(state.Marketplaces[i].Path, name, "hooks", "hooks.json"))
		os.MkdirAll(filepath.Join(dir, "hooks"), 0o700)
		os.WriteFile(filepath.Join(dir, "hooks", "hooks.json"), b, 0o600)
		state.Plugins = append(state.Plugins, pl{args[2], true, dir})
	case args[0] == "plugin" && args[1] == "uninstall":
		state.Plugins = slices.DeleteFunc(state.Plugins, func(p pl) bool { return p.ID == args[2] })
	default:
		os.Stderr.WriteString("fake claude: unknown command")
		return 1
	}
	cfg["mcpServers"] = servers
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(cfgPath, b, 0o600)
	b, _ = json.Marshal(state)
	os.WriteFile(statePath, b, 0o600)
	return 0
}

// changes are the claude commands of log that change something (not the --json lists), and
// empty the log.
func changes(t *testing.T, log string) []string {
	f, err := os.Open(log)
	if err != nil {
		return nil
	}
	defer os.Remove(log)
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if !strings.HasSuffix(sc.Text(), "--json") {
			out = append(out, sc.Text())
		}
	}
	return out
}

// setup claude / setup remove claude through Claude's own commands: installs what is missing,
// changes nothing when run again, installs again what runs a moved binary, and removes all of it;
// the Human's other MCP servers are kept. Without claude on PATH: a clear error.
func TestSetupClaudeInstallRemove(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	exe, _ := os.Executable()
	os.Symlink(exe, filepath.Join(bin, "claude"))
	t.Setenv("PATH", bin)
	log := filepath.Join(home, "argv")
	t.Setenv("PIGGERY_FAKE_CLAUDE", log)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"mine":{"command":"x"}}}`), 0o600)
	root := filepath.Join(home, ".piggery", "claude")

	if _, err := installClaude(root, "/opt/a/piggery"); err != nil {
		t.Fatal(err)
	}
	want := []string{"mcp add --scope user piggery -- /opt/a/piggery mcp", "plugin marketplace add " + root, "plugin install piggery@piggery"}
	if got := changes(t, log); !slices.Equal(got, want) {
		t.Fatalf("install ran %q", got)
	}
	if msg, err := installClaude(root, "/opt/a/piggery"); err != nil || len(changes(t, log)) > 0 || !strings.Contains(msg, "already") {
		t.Fatalf("second install: %q %v", msg, err)
	}
	if st := claudeStatus(root, "/opt/a/piggery"); !st.Installed || len(st.Problems) > 0 {
		t.Fatalf("status: %+v", st)
	}
	if st := claudeStatus(root, "/opt/b/piggery"); len(st.Problems) != 2 {
		t.Fatalf("status for a moved binary: %+v", st)
	}
	changes(t, log)
	if _, err := installClaude(root, "/opt/b/piggery"); err != nil {
		t.Fatal(err)
	}
	want = []string{"mcp remove --scope user piggery", "mcp add --scope user piggery -- /opt/b/piggery mcp",
		"plugin uninstall piggery@piggery", "plugin install piggery@piggery"}
	if got := changes(t, log); !slices.Equal(got, want) {
		t.Fatalf("install after a move ran %q", got)
	}
	if _, err := removeClaude(root); err != nil {
		t.Fatal(err)
	}
	want = []string{"mcp remove --scope user piggery", "plugin uninstall piggery@piggery", "plugin marketplace remove piggery"}
	if got := changes(t, log); !slices.Equal(got, want) {
		t.Fatalf("remove ran %q", got)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if _, err := os.Stat(root); err == nil || !strings.Contains(string(b), `"mine"`) || strings.Contains(string(b), "piggery") {
		t.Fatalf("after remove: root kept=%v, ~/.claude.json %s", err == nil, b)
	}
	if msg, err := removeClaude(root); err != nil || len(changes(t, log)) > 0 || !strings.Contains(msg, "not installed") {
		t.Fatalf("second remove: %q %v", msg, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := installClaude(root, "/opt/a/piggery"); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("without claude: %v", err)
	}
}

// setup pi installs the binary's copy of the extension into pi's extensions/piggery (marked,
// versioned) and points the worker profile at it; --ext installs a checkout instead, and each
// takes the other out (never both); pi's settings.json is otherwise untouched (byte for byte,
// with no final newline, as pi writes it); remove takes out both; a second run changes nothing;
// the daemon's update only moves forward; a directory piggery does not manage is refused.
func TestSetupPiInstallRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(home, ".piggery")
	checkout := filepath.Join(home, "src", "piggery", "extensions", "pi")
	os.MkdirAll(checkout, 0o700)
	os.WriteFile(filepath.Join(checkout, "index.ts"), nil, 0o600)
	os.WriteFile(filepath.Join(checkout, "package.json"), []byte(`{"name":"piggery-pi"}`), 0o600)
	settings := filepath.Join(home, ".pi", "agent", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o700)
	orig := "{\n  \"theme\": \"tokyo\",\n  \"compaction\": {\n    \"enabled\": true\n  },\n  \"extensions\": [\n    \"/x/other\"\n  ],\n  \"empty\": [],\n  \"name\": \"é <&>\"\n}"
	os.WriteFile(settings, []byte(orig), 0o600)
	writeJSON(local.ProfilePath(dir), local.DefaultProfile("/gone/piggery/extensions/pi/index.ts"), false)
	copyDir := local.PiExtDir(dir)
	profileExt := func() string {
		var p struct{ Args []string }
		b, _ := os.ReadFile(local.ProfilePath(dir))
		json.Unmarshal(b, &p)
		return p.Args[slices.Index(p.Args, "-e")+1]
	}

	if _, err := installPi(dir, "", "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	if v, ok := local.PiExtVersion(copyDir); !ok || v != "v1.2.0" || profileExt() != filepath.Join(copyDir, "index.ts") {
		t.Fatalf("copy %q %v, profile -e %s", v, ok, profileExt())
	}
	if b, _ := os.ReadFile(settings); string(b) != orig {
		t.Fatalf("settings changed:\n%s", b)
	}
	if msg, _ := installPi(dir, "", "v1.2.0"); !strings.Contains(msg, "already") {
		t.Fatalf("second install: %q", msg)
	}
	if st := piStatus(dir, "/self", "v1.3.0"); !st.Installed || !strings.Contains(st.Problems[0].Text, "not this piggery's") {
		t.Fatalf("status for a newer binary: %+v", st)
	}
	if up, _ := local.UpdatePiExt(copyDir, "v1.1.0"); up {
		t.Fatal("an older binary updated the copy")
	}
	if up, _ := local.UpdatePiExt(copyDir, "v1.3.0"); !up {
		t.Fatal("a newer binary did not update the copy")
	}

	if _, err := installPi(dir, checkout, "v1.3.0"); err != nil {
		t.Fatal(err)
	}
	var s struct{ Extensions []string }
	b, _ := os.ReadFile(settings)
	json.Unmarshal(b, &s)
	if _, err := os.Stat(copyDir); err == nil || !slices.Equal(s.Extensions, []string{"/x/other", checkout}) ||
		profileExt() != filepath.Join(checkout, "index.ts") {
		t.Fatalf("--ext: copy kept=%v, extensions %q, profile -e %s", err == nil, s.Extensions, profileExt())
	}
	if _, err := installPi(dir, "", "v1.3.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(settings); string(b) != orig {
		t.Fatalf("the checkout entry stayed:\n%s", b)
	}
	if _, err := removePi(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copyDir); err == nil {
		t.Fatal("remove kept the copy")
	}
	if msg, _ := removePi(dir); !strings.Contains(msg, "not installed") {
		t.Fatalf("second remove: %q", msg)
	}
	os.MkdirAll(copyDir, 0o700)
	os.WriteFile(filepath.Join(copyDir, "index.ts"), []byte("// mine"), 0o600)
	if _, err := installPi(dir, "", "v1.3.0"); err == nil || !strings.Contains(err.Error(), "not piggery's") {
		t.Fatalf("over a directory not piggery's: %v", err)
	}
}
