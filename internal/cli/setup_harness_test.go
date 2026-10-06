package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sting8k/piggery/internal/driver/local"
)

// TestMain lets this test binary stand in for `claude` (fakeClaude) and `paseo` (fakePaseo) when
// a test puts it on PATH under that name.
func TestMain(m *testing.M) {
	if log := os.Getenv("PIGGERY_FAKE_CLAUDE"); log != "" && strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "claude" {
		os.Exit(fakeClaude(log, os.Args[1:]))
	}
	if log := os.Getenv("PIGGERY_FAKE_PASEO"); log != "" && strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "paseo" {
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
	setHome(t, home)
	linkExe(t, bin, "claude")
	t.Setenv("PATH", bin)
	log := filepath.Join(home, "argv")
	t.Setenv("PIGGERY_FAKE_CLAUDE", log)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"mine":{"command":"x"}}}`), 0o600)
	dir := filepath.Join(home, ".piggery")
	root := filepath.Join(dir, "claude")

	if _, err := installClaude(dir, "/opt/a/piggery"); err != nil {
		t.Fatal(err)
	}
	want := []string{"mcp add --scope user piggery -- /opt/a/piggery mcp", "plugin marketplace add " + root, "plugin install piggery@piggery"}
	if got := changes(t, log); !slices.Equal(got, want) {
		t.Fatalf("install ran %q", got)
	}
	if msg, err := installClaude(dir, "/opt/a/piggery"); err != nil || len(changes(t, log)) > 0 || !strings.Contains(msg, "already") {
		t.Fatalf("second install: %q %v", msg, err)
	}
	if st := claudeStatus(root, "/opt/a/piggery"); !st.Installed || len(st.Problems) > 0 {
		t.Fatalf("status: %+v", st)
	}
	if st := claudeStatus(root, "/opt/b/piggery"); len(st.Problems) != 2 {
		t.Fatalf("status for a moved binary: %+v", st)
	}
	changes(t, log)
	if _, err := installClaude(dir, "/opt/b/piggery"); err != nil {
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
	if _, err := installClaude(dir, "/opt/a/piggery"); err == nil || !strings.Contains(err.Error(), "not on PATH") {
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
	setHome(t, home)
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

	if _, err := installPi(dir, ""); err != nil {
		t.Fatal(err)
	}
	if v, ok := local.PiExtVersion(copyDir); !ok || v != local.IntegrationVersion("pi") || profileExt() != filepath.Join(copyDir, "index.ts") {
		t.Fatalf("copy v%d %v, profile -e %s", v, ok, profileExt())
	}
	if b, _ := os.ReadFile(settings); string(b) != orig {
		t.Fatalf("settings changed:\n%s", b)
	}
	if msg, _ := installPi(dir, ""); !strings.Contains(msg, "already") {
		t.Fatalf("second install: %q", msg)
	}
	// A copy at this binary's integer is not outdated, whatever build wrote it; one that only has
	// a build version (an install from before the integers) is, once, and the daemon's update
	// rewrites it.
	if i, ok := integrationOf(dir, "pi"); !ok || i.outdated() {
		t.Fatalf("a current copy: %+v %v", i, ok)
	}
	oldMarker(t, filepath.Join(copyDir, "index.ts"))
	if i, _ := integrationOf(dir, "pi"); !i.outdated() || i.Have != 0 || i.Want != local.IntegrationVersion("pi") {
		t.Fatalf("a copy with a build-version marker: %+v", i)
	}
	if up, _ := local.UpdatePiExt(copyDir); !up {
		t.Fatal("the daemon start did not update an outdated copy")
	}
	if i, _ := integrationOf(dir, "pi"); i.outdated() {
		t.Fatalf("after the update: %+v", i)
	}

	if _, err := installPi(dir, checkout); err != nil {
		t.Fatal(err)
	}
	var s struct{ Extensions []string }
	b, _ := os.ReadFile(settings)
	json.Unmarshal(b, &s)
	if _, err := os.Stat(copyDir); err == nil || !slices.Equal(s.Extensions, []string{"/x/other", checkout}) ||
		profileExt() != filepath.Join(checkout, "index.ts") {
		t.Fatalf("--ext: copy kept=%v, extensions %q, profile -e %s", err == nil, s.Extensions, profileExt())
	}
	if _, err := installPi(dir, ""); err != nil {
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
	if _, err := installPi(dir, ""); err == nil || !strings.Contains(err.Error(), "not piggery's") {
		t.Fatalf("over a directory not piggery's: %v", err)
	}
}

// setup omp: the binary's copy goes into omp's agent dir (PI_CODING_AGENT_DIR, else ~/.omp/agent), a
// second run changes nothing, a copy from another version is a problem for doctor and updated by
// a newer binary, remove takes it out, and a directory that is not piggery's is never replaced.
func TestSetupOmpInstallRemove(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(home, ".piggery")
	copyDir := filepath.Join(home, ".omp", "agent", "extensions", "piggery")
	if local.OmpExtDir(dir) != copyDir {
		t.Fatalf("ext dir %s, want %s", local.OmpExtDir(dir), copyDir)
	}
	if st := ompStatus(dir, "/self"); st.Installed || len(st.Problems) != 0 {
		t.Fatalf("before setup: %+v", st)
	}
	if msg, err := installOmp(dir); err != nil || !strings.Contains(msg, "installed") {
		t.Fatalf("install: %q, %v", msg, err)
	}
	if v, ok := local.OmpExtVersion(copyDir); !ok || v != local.IntegrationVersion("omp") {
		t.Fatalf("copy v%d %v", v, ok)
	}
	if msg, _ := installOmp(dir); !strings.Contains(msg, "already") {
		t.Fatalf("second install: %q", msg)
	}
	if st := ompStatus(dir, "/self"); len(st.Problems) != 1 || !strings.Contains(st.Problems[0].Text, "not on PATH") {
		t.Fatalf("status of a current copy with no piggery on PATH: %+v", st) // the extension starts the daemon with it
	}
	if _, err := removeOmp(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copyDir); err == nil {
		t.Fatal("remove kept the copy")
	}
	if msg, _ := removeOmp(dir); !strings.Contains(msg, "not installed") {
		t.Fatalf("second remove: %q", msg)
	}
	os.MkdirAll(copyDir, 0o700)
	os.WriteFile(filepath.Join(copyDir, "index.ts"), []byte("// mine"), 0o600)
	if _, err := installOmp(dir); err == nil || !strings.Contains(err.Error(), "not piggery's") {
		t.Fatalf("over a directory not piggery's: %v", err)
	}
	if st := ompStatus(dir, "/self"); st.Installed || len(st.Problems) != 1 || !strings.Contains(st.Problems[0].Text, "not piggery's") {
		t.Fatalf("status of a directory not piggery's: %+v", st)
	}
}

// setup dsh: the plugin copy goes under ~/.piggery/plugins/dsh and one managed block, whose row names
// the directory for the session records, into dsh's home patch,
// after the user's own rows, which stay; a second run changes nothing, remove takes out the block
// and the copy and leaves the user's rows as they were, a home patch that is not a list is refused
// untouched, and status says when the row is missing.
func TestSetupDshInstallRemove(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("DSH_HOME", "")
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(home, ".piggery")
	patch := filepath.Join(home, ".dsh", "cordis.patch.yml")
	mine := "# mine\n- id: ui-skin\n  disabled: true\n"
	os.MkdirAll(filepath.Dir(patch), 0o700)
	os.WriteFile(patch, []byte(mine), 0o644)
	if msg, err := installDsh(dir); err != nil || !strings.Contains(msg, "installed") {
		t.Fatalf("install: %q, %v", msg, err)
	}
	got, _ := os.ReadFile(patch)
	var rows []map[string]any
	if err := yaml.Unmarshal(got, &rows); err != nil || len(rows) != 2 || !strings.HasPrefix(string(got), mine) ||
		!strings.Contains(string(got), local.DshEntry(dir)) || !strings.Contains(string(got), "sessions: '"+local.DshSessionsDir(dir)+"'") {
		t.Fatalf("home patch after install: %v\n%s", err, got)
	}
	if st, _ := os.Stat(patch); !permIs(st.Mode().Perm(), 0o644) {
		t.Fatalf("mode %v: the user's file mode changed", st.Mode())
	}
	if msg, _ := installDsh(dir); !strings.Contains(msg, "already") {
		t.Fatalf("second install: %q", msg)
	}
	if st := dshStatus(dir, "/self"); !st.Installed || len(st.Problems) != 1 || !strings.Contains(st.Problems[0].Text, "not on PATH") {
		t.Fatalf("status of a full install: %+v", st)
	}
	os.WriteFile(patch, []byte(mine), 0o644)
	if st := dshStatus(dir, "/self"); !strings.Contains(st.Problems[0].Text, "no row") {
		t.Fatalf("status without the row: %+v", st)
	}
	installDsh(dir)
	if _, err := removeDsh(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(patch); string(got) != mine {
		t.Fatalf("home patch after remove: %q, want the user's rows as they were", got)
	}
	if _, err := os.Stat(local.DshExtDir(dir)); err == nil {
		t.Fatal("remove kept the copy")
	}
	if msg, _ := removeDsh(dir); !strings.Contains(msg, "not installed") {
		t.Fatalf("second remove: %q", msg)
	}
	os.WriteFile(patch, []byte("a: 1\n"), 0o600)
	if _, err := installDsh(dir); err == nil {
		t.Fatal("a home patch that is not a list was rewritten")
	} else if got, _ := os.ReadFile(patch); string(got) != "a: 1\n" {
		t.Fatalf("refused, but the file is now %q", got)
	} else if _, err := os.Stat(local.DshExtDir(dir)); err == nil {
		t.Fatal("refused, but the copy was installed")
	}
}

// integrationOf is the installed integration of dir by name.
func integrationOf(dir, name string) (integration, bool) {
	for _, i := range installedIntegrations(dir) {
		if i.Name == name {
			return i, true
		}
	}
	return integration{}, false
}

// oldMarker rewrites the first line of file as a piggery from before the integers wrote it: a
// build version where the integer is now.
func oldMarker(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(string(b), "\n")
	if err := os.WriteFile(file, []byte("// managed by piggery v0.3.0: written by `piggery setup`; run it again instead of editing\n"+rest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup keeps one backup of the Human's config file, made from a file that has no piggery part yet
// and kept (not overwritten) once the file has it; a file that does not exist has nothing to
// copy; remove neither makes nor restores one.
func TestSetupBackupOnce(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("DSH_HOME", "")
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(home, ".piggery")
	patch := filepath.Join(home, ".dsh", "cordis.patch.yml")
	backup := setupBackupPath(dir, "dsh", patch)
	if msg, err := installDsh(dir); err != nil || strings.Contains(msg, "kept a copy") {
		t.Fatalf("no file yet, nothing to copy: %q %v", msg, err)
	}
	removeDsh(dir)
	before := "- id: ui-skin\n  disabled: true\n"
	os.WriteFile(patch, []byte(before), 0o644)
	msg, err := installDsh(dir)
	got, _ := os.ReadFile(backup)
	if st, serr := os.Stat(backup); err != nil || serr != nil || string(got) != before || !permIs(st.Mode().Perm(), 0o600) || !strings.Contains(msg, backup) {
		t.Fatalf("backup: %q %v %v\n%s", msg, err, serr, got)
	}
	// the Human edits the file after piggery came in; a second run and an upgrade keep the backup
	cur, _ := os.ReadFile(patch)
	os.WriteFile(patch, append(cur, "- id: later\n"...), 0o644)
	if msg, err := installDsh(dir); err != nil || strings.Contains(msg, "kept a copy") {
		t.Fatalf("second run: %q %v", msg, err)
	}
	if got, _ := os.ReadFile(backup); string(got) != before {
		t.Fatalf("the backup was overwritten:\n%s", got)
	}
	removeDsh(dir)
	if got, _ := os.ReadFile(backup); string(got) != before {
		t.Fatalf("remove touched the backup:\n%s", got)
	}
	if cur, _ := os.ReadFile(patch); strings.Contains(string(cur), "piggery") || !strings.Contains(string(cur), "- id: later") {
		t.Fatalf("remove did not give back only piggery's part:\n%s", cur)
	}
}

// pi --ext on a settings.json that has no "extensions" key: setup then remove gives the file back
// byte for byte (a value on one line stays on one line), and a settings.json that setup made is
// gone again, not left as `{}`.
func TestSetupPiExtRoundTrip(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(home, ".piggery")
	checkout := filepath.Join(home, "src", "piggery", "extensions", "pi")
	os.MkdirAll(checkout, 0o700)
	os.WriteFile(filepath.Join(checkout, "index.ts"), nil, 0o600)
	os.WriteFile(filepath.Join(checkout, "package.json"), []byte(`{"name":"piggery-pi"}`), 0o600)
	settings := filepath.Join(home, ".pi", "agent", "settings.json")

	if _, err := installPi(dir, checkout); err != nil {
		t.Fatal(err)
	}
	if _, err := removePi(dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(settings); err == nil {
		t.Fatalf("settings.json made by setup is left behind: %s", b)
	}

	orig := "{\n  \"packages\": [\"a\"],\n  \"theme\": \"tokyo\"\n}\n"
	os.WriteFile(settings, []byte(orig), 0o600)
	if _, err := installPi(dir, checkout); err != nil {
		t.Fatal(err)
	}
	if _, err := removePi(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(settings); string(b) != orig {
		t.Fatalf("settings.json after setup + remove:\n%s", b)
	}
}

// setup opencode unpacks piggery's plugin under ~/.piggery/plugins/opencode and adds one entry to
// "plugin" of opencode's config after the Human's own, which stay byte for byte (a second run
// changes nothing); the config is backed up first, and remove gives it back as it was. A config
// that is not plain JSON (opencode.jsonc) is never written: nothing is, and the line to add is printed.
func TestSetupOpencodeInstallRemove(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(home, ".piggery")
	cfgDir := filepath.Join(home, ".config", "opencode")
	cfg := filepath.Join(cfgDir, "opencode.json")
	os.MkdirAll(cfgDir, 0o700)

	// opencode.jsonc: refused, nothing written
	jsonc := filepath.Join(cfgDir, "opencode.jsonc")
	os.WriteFile(jsonc, []byte("{\n  // mine\n  \"theme\": \"x\"\n}\n"), 0o600)
	_, err := installOpencode(dir)
	if err == nil || !strings.Contains(err.Error(), `"`+local.OpencodePluginSpec(local.OpencodeEntry(dir))+`"`) {
		t.Fatalf("a .jsonc config: %v", err)
	}
	if _, serr := os.Stat(dir); serr == nil {
		t.Fatal("setup wrote into piggery's dir though it refused the config")
	}
	os.Remove(jsonc)

	orig := "{\n  \"theme\": \"x\",\n  \"plugin\": [\"other\", [\"file:///opt/mine.js\", {\"a\": 1}]],\n  \"mcp\": {\"m\": {\"type\": \"local\"}}\n}\n"
	os.WriteFile(cfg, []byte(orig), 0o600)
	msg, err := installOpencode(dir)
	if err != nil || !strings.Contains(msg, "kept a copy") {
		t.Fatalf("install: %q %v", msg, err)
	}
	spec := local.OpencodePluginSpec(local.OpencodeEntry(dir))
	got, _ := os.ReadFile(cfg)
	if want := strings.Replace(orig, `{"a": 1}]]`, `{"a": 1}], "`+spec+`"]`, 1); string(got) != want {
		t.Fatalf("config after install:\n%s", got)
	}
	if v, ok := local.OpencodeExtVersion(local.OpencodeExtDir(dir)); !ok || v != local.IntegrationVersion("opencode") {
		t.Fatalf("plugin copy version %d managed %v", v, ok)
	}
	if b, _ := os.ReadFile(setupBackupPath(dir, "opencode", cfg)); string(b) != orig {
		t.Fatalf("backup:\n%s", b)
	}
	if st := opencodeStatus(dir, "x"); !st.Installed || slices.ContainsFunc(st.Problems, func(p problem) bool { return strings.Contains(p.Text, "no entry") }) {
		t.Fatalf("status: %+v", st)
	}
	if msg, _ := installOpencode(dir); !strings.Contains(msg, "already") {
		t.Fatalf("second install: %q", msg)
	}
	if _, err := removeOpencode(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != orig {
		t.Fatalf("config after remove:\n%s", b)
	}
	if _, err := os.Stat(local.OpencodeExtDir(dir)); err == nil {
		t.Fatal("remove kept the plugin copy")
	}
	if msg, _ := removeOpencode(dir); !strings.Contains(msg, "not installed") {
		t.Fatalf("second remove: %q", msg)
	}

	// no config at all: setup makes one, remove deletes what is left of it
	os.Remove(cfg)
	if _, err := installOpencode(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := removeOpencode(dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(cfg); err == nil {
		t.Fatalf("a config made by setup is left: %s", b)
	}
}
