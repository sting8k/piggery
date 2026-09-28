package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sting8k/piggery/internal/driver/local"
)

// Claude: a local plugin marketplace "piggery" in
// <dir>/claude with one plugin holding only hooks (`piggery hook claude <event>`), and piggery's
// MCP server at user scope. The MCP server is not in the plugin: a plugin's MCP tools are renamed
// mcp__plugin_<plugin>_<server>__*, and a session must see the tool names a worker sees
// (mcp__piggery__*). A worker never loads either twice: its --mcp-config replaces the user server
// "piggery", and its settings turn the plugin off. piggery changes Claude only through Claude's
// own commands (claude mcp, claude plugin); it reads ~/.claude.json and their --json lists.

// claudeSessionHooks are the hooks of a Claude Code session the Human opens: the worker's
// (local.ClaudeHooks), plus PreToolUse (piggery's own MCP tools need no permission prompt) and
// Notification (idle_prompt: Claude waits for its user).
var claudeSessionHooks = append(slices.Clone(local.ClaudeHooks), "PreToolUse", "Notification")

// claudeBin is the claude executable setup runs (a var for tests).
var claudeBin = "claude"

const claudeMarketplace = "piggery"

// claudeState is what Claude has of piggery.
type claudeState struct {
	mcpCmd      []string // command and args of the user MCP server "piggery" (nil = none)
	marketplace string   // where the marketplace "piggery" points ("" = none)
	plugin      bool     // piggery@piggery installed
	enabled     bool
	pluginPath  string // the installed plugin's directory (Claude's copy)
}

func claudeRun(args ...string) ([]byte, error) {
	out, err := exec.Command(claudeBin, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", claudeBin, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

func readClaudeState() (claudeState, error) {
	var st claudeState
	home, _ := os.UserHomeDir()
	if b, err := readOptional(filepath.Join(home, ".claude.json")); err != nil {
		return st, err
	} else if len(b) > 0 {
		var cfg struct {
			MCPServers map[string]struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return st, fmt.Errorf("~/.claude.json: %w", err)
		}
		if s, ok := cfg.MCPServers["piggery"]; ok {
			st.mcpCmd = append([]string{s.Command}, s.Args...)
		}
	}
	out, err := claudeRun("plugin", "marketplace", "list", "--json")
	if err != nil {
		return st, err
	}
	var mks []struct {
		Name, Path, InstallLocation string
	}
	if err := json.Unmarshal(jsonPart(out), &mks); err != nil {
		return st, fmt.Errorf("claude plugin marketplace list: %w", err)
	}
	for _, m := range mks {
		if m.Name == claudeMarketplace {
			st.marketplace = firstNonEmpty(m.Path, m.InstallLocation)
		}
	}
	if out, err = claudeRun("plugin", "list", "--json"); err != nil {
		return st, err
	}
	var pls []struct {
		ID          string `json:"id"`
		Enabled     bool   `json:"enabled"`
		InstallPath string `json:"installPath"`
	}
	if err := json.Unmarshal(jsonPart(out), &pls); err != nil {
		return st, fmt.Errorf("claude plugin list: %w", err)
	}
	for _, p := range pls {
		if p.ID == local.ClaudePlugin {
			st.plugin, st.enabled, st.pluginPath = true, p.Enabled, p.InstallPath
		}
	}
	return st, nil
}

// jsonPart skips anything Claude prints before its JSON.
func jsonPart(b []byte) []byte {
	if i := bytes.IndexAny(b, "[{"); i >= 0 {
		return b[i:]
	}
	return b
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// writeClaudePlugin writes the marketplace and its hooks-only plugin under root.
func writeClaudePlugin(root, self string) error {
	hooks := map[string]any{}
	for _, ev := range claudeSessionHooks {
		m := map[string]any{"hooks": []any{map[string]any{"type": "command",
			"command": local.ShellQuote(self) + " hook claude " + ev, "timeout": 10}}}
		switch ev {
		case "PreToolUse":
			m["matcher"] = "mcp__piggery__.*"
		case "Notification":
			m["matcher"] = "idle_prompt"
		}
		hooks[ev] = []any{m}
	}
	files := []struct {
		path string
		v    any
	}{
		{filepath.Join(root, ".claude-plugin", "marketplace.json"), map[string]any{"name": claudeMarketplace,
			"owner":   map[string]any{"name": "piggery"},
			"plugins": []any{map[string]any{"name": "piggery", "source": "./piggery", "description": "piggery hooks for Claude Code sessions"}}}},
		{filepath.Join(root, "piggery", ".claude-plugin", "plugin.json"), map[string]any{"name": "piggery", "version": Version,
			"description": "Joins this Claude Code session to piggery: mail with turns, tool calls and the end of a turn"}},
		{filepath.Join(root, "piggery", "hooks", "hooks.json"), map[string]any{"hooks": hooks}},
	}
	for _, f := range files {
		if _, err := writeJSON(f.path, f.v, true); err != nil {
			return err
		}
	}
	return nil
}

// hooksRunSelf: the plugin at dir (Claude's copy or ours) has hooks, and all of them run self.
func hooksRunSelf(dir, self string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "hooks", "hooks.json"))
	if err != nil {
		return false
	}
	var f struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(b, &f) != nil || len(f.Hooks) == 0 {
		return false
	}
	for _, groups := range f.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if !strings.HasPrefix(h.Command, local.ShellQuote(self)+" hook claude ") {
					return false
				}
			}
		}
	}
	return true
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// installClaude writes the plugin under root and makes Claude load it and piggery's MCP server,
// running only the commands whose part is missing or points elsewhere.
// claudeHarness: hooks and piggery mcp; an idle session is woken through its messaging socket.
var claudeHarness = harnessProfile{
	setupTarget: setupTarget{name: "claude", cmd: "claude",
		install: func(o setupOpts) (string, error) { return installClaude(filepath.Join(o.dir, "claude"), o.self) },
		remove:  func(o setupOpts) (string, error) { return removeClaude(filepath.Join(o.dir, "claude")) },
		status:  func(o setupOpts) harnessState { return claudeStatus(filepath.Join(o.dir, "claude"), o.self) },
	},
	profilePath: local.ClaudeProfilePath,
	hookEvent:   claudeHookEvent, hookOutput: claudeHookOutput,
	wake:    func(s *mcpServer, _ string) { s.nudge() },
	channel: local.ClaudeChannel,
}

func installClaude(root, self string) (string, error) {
	if _, err := exec.LookPath(claudeBin); err != nil {
		return "", errors.New("claude: `claude` is not on PATH; install Claude Code first")
	}
	if err := writeClaudePlugin(root, self); err != nil {
		return "", err
	}
	st, err := readClaudeState()
	if err != nil {
		return "", err
	}
	var did []string
	run := func(args ...string) error {
		if _, err := claudeRun(args...); err != nil {
			return err
		}
		did = append(did, "claude "+strings.Join(args, " "))
		return nil
	}
	if want := []string{self, "mcp"}; !slices.Equal(st.mcpCmd, want) {
		if st.mcpCmd != nil {
			if err := run("mcp", "remove", "--scope", "user", "piggery"); err != nil {
				return "", err
			}
		}
		if err := run("mcp", "add", "--scope", "user", "piggery", "--", self, "mcp"); err != nil {
			return "", err
		}
	}
	// Claude may run its own copy of the plugin: one that is stale (moved binary, another
	// marketplace) is installed again.
	if st.plugin && (st.marketplace == "" || !samePath(st.marketplace, root) || !hooksRunSelf(st.pluginPath, self)) {
		if err := run("plugin", "uninstall", local.ClaudePlugin); err != nil {
			return "", err
		}
		st.plugin = false
	}
	if st.marketplace != "" && !samePath(st.marketplace, root) {
		if err := run("plugin", "marketplace", "remove", claudeMarketplace); err != nil {
			return "", err
		}
		st.marketplace = ""
	}
	if st.marketplace == "" {
		if err := run("plugin", "marketplace", "add", root); err != nil {
			return "", err
		}
	}
	switch {
	case !st.plugin:
		if err := run("plugin", "install", local.ClaudePlugin); err != nil {
			return "", err
		}
	case !st.enabled:
		if err := run("plugin", "enable", local.ClaudePlugin); err != nil {
			return "", err
		}
	}
	if len(did) == 0 {
		return "claude: piggery is already set up (MCP server and plugin)", nil
	}
	return "claude: ran\n  " + strings.Join(did, "\n  ") +
		"\nclaude: sessions started from now on join piggery; restart any that are open.", nil
}

// removeClaude undoes installClaude: Claude's MCP server, plugin and marketplace, then root.
func removeClaude(root string) (string, error) {
	if _, err := exec.LookPath(claudeBin); err != nil {
		if _, serr := os.Stat(root); serr != nil {
			return "claude: piggery is not installed", nil
		}
		return "", errors.New("claude: `claude` is not on PATH; cannot remove piggery's MCP server and plugin")
	}
	st, err := readClaudeState()
	if err != nil {
		return "", err
	}
	var did []string
	for _, c := range []struct {
		have bool
		args []string
	}{
		{st.mcpCmd != nil, []string{"mcp", "remove", "--scope", "user", "piggery"}},
		{st.plugin, []string{"plugin", "uninstall", local.ClaudePlugin}},
		{st.marketplace != "", []string{"plugin", "marketplace", "remove", claudeMarketplace}},
	} {
		if !c.have {
			continue
		}
		if _, err := claudeRun(c.args...); err != nil {
			return "", err
		}
		did = append(did, "claude "+strings.Join(c.args, " "))
	}
	_, statErr := os.Stat(root)
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}
	if len(did) == 0 && statErr != nil {
		return "claude: piggery is not installed", nil
	}
	if statErr == nil {
		did = append(did, "removed "+root)
	}
	return "claude: " + strings.Join(did, "\nclaude: "), nil
}

// claudeStatus: piggery's MCP server, marketplace and plugin in Claude, each running self.
func claudeStatus(root, self string) harnessState {
	st := harnessState{Name: "claude"}
	fix := "piggery setup claude"
	if _, err := exec.LookPath(claudeBin); err != nil {
		if _, serr := os.Stat(root); serr == nil {
			st.Installed = true
			st.Problems = append(st.Problems, problem{"`claude` is not on PATH", "install Claude Code, or `piggery setup remove claude`"})
		}
		return st
	}
	cs, err := readClaudeState()
	if err != nil {
		st.Problems = append(st.Problems, problem{err.Error(), ""})
		return st
	}
	if cs.mcpCmd == nil && cs.marketplace == "" && !cs.plugin {
		return st
	}
	st.Installed = true
	var parts []string
	switch {
	case cs.mcpCmd == nil:
		st.Problems = append(st.Problems, problem{"piggery's MCP server is missing", fix})
	case !slices.Equal(cs.mcpCmd, []string{self, "mcp"}):
		st.Problems = append(st.Problems, problem{fmt.Sprintf("the MCP server runs %q, not %s mcp", strings.Join(cs.mcpCmd, " "), self), fix})
	default:
		parts = append(parts, "MCP server")
	}
	switch {
	case !cs.plugin:
		st.Problems = append(st.Problems, problem{"the plugin " + local.ClaudePlugin + " is not installed", fix})
	case !cs.enabled:
		st.Problems = append(st.Problems, problem{"the plugin " + local.ClaudePlugin + " is disabled", fix})
	case !hooksRunSelf(cs.pluginPath, self):
		st.Problems = append(st.Problems, problem{"the plugin's hooks run another piggery", fix})
	default:
		parts = append(parts, "plugin")
	}
	if cs.marketplace != "" && !samePath(cs.marketplace, root) {
		st.Problems = append(st.Problems, problem{fmt.Sprintf("the marketplace %q is %s, not %s", claudeMarketplace, cs.marketplace, root), fix})
	}
	st.Detail = strings.Join(parts, ", ")
	return st
}
