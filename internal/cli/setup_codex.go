package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/jsonobj"
)

// `piggery setup codex` installs the Codex adapter once for every Codex session and worker
// (piggery writes the Codex home itself):
//   - piggery's hooks (`piggery hook codex <Event>`) in <CODEX_HOME>/hooks.json, after the
//     Human's own groups there (a file of its own keeps piggery's positions, which are part of
//     Codex's trust key, stable);
//   - in <CODEX_HOME>/config.toml, a marked block with [mcp_servers.piggery] (tools approved
//     without asking) and [hooks.state."<key>"] trusted_hash for each piggery hook. Codex runs a
//     non-managed hook only when its hash is trusted, and the hash cannot be computed outside
//     Codex: it comes from `hooks/list` of a short `codex app-server` (no thread, no model turn).
// Everything else in both files is kept. Run again it changes nothing; when the trust no longer
// matches (a moved binary, hooks the Human added before piggery's) it writes it again.

// codexHookEvents are the Codex hooks the adapter maps, with their trust-key names.
var codexHookEvents = []struct{ name, key string }{
	{"SessionStart", "session_start"},
	{"UserPromptSubmit", "user_prompt_submit"},
	{"PostToolUse", "post_tool_use"},
	{"Stop", "stop"},
	{"Interrupt", "interrupt"},
	{"PermissionRequest", "permission_request"},
	{"SessionEnd", "session_end"},
}

const (
	codexBlockBegin = "# >>> piggery: written by `piggery setup codex`; run it again instead of editing >>>"
	codexBlockEnd   = "# <<< piggery <<<"
)

// codexHookMeta is the part of app-server's hooks/list entry setup reads.
type codexHookMeta struct {
	Key         string `json:"key"`
	CurrentHash string `json:"currentHash"`
	TrustStatus string `json:"trustStatus"`
	SourcePath  string `json:"sourcePath"`
}

// codexHooksList lists the hooks Codex loads for home (a var for tests).
var codexHooksList = listCodexHooks

func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// installCodex writes piggery's hooks, MCP server and hook trust into home (see above) and says
// what it did.
// codexHarness: hooks and piggery mcp; an idle session is woken by a queued prompt.
var codexHarness = harnessProfile{
	setupTarget: setupTarget{name: "codex", cmd: "codex",
		install: func(o setupOpts) (string, error) { return installCodex(o.dir, codexHome(), o.self) },
		remove:  func(setupOpts) (string, error) { return removeCodex(codexHome()) },
		status:  func(o setupOpts) harnessState { return codexStatus(codexHome(), o.self) },
	},
	profilePath: local.CodexProfilePath,
	hookEvent:   codexHookEvent, hookOutput: codexHookOutput,
	wake:    func(s *mcpServer, ref string) { s.queueNudge(ref) },
	channel: codexChannel,
}

func installCodex(dir, home, self string) (string, error) {
	var backups []string
	hooksPath := filepath.Join(home, "hooks.json")
	oldHooks, err := readOptional(hooksPath)
	if err != nil {
		return "", err
	}
	newHooks, keys, err := mergeCodexHooks(oldHooks, hooksPath, self)
	if err != nil {
		return "", fmt.Errorf("%s: %w", hooksPath, err)
	}
	changedHooks := !bytes.Equal(oldHooks, newHooks)
	if changedHooks {
		msg, err := backupHumanConfig(dir, "codex", hooksPath, contains(" hook codex ")) // what isPiggeryCodexGroup looks for
		if err != nil {
			return "", err
		}
		backups = append(backups, msg)
		if err := writeFileAtomic(hooksPath, newHooks); err != nil {
			return "", err
		}
	}
	metas, err := codexHooksList(home)
	if err != nil {
		return "", err
	}
	trusted, err := piggeryHookState(metas, hooksPath, keys)
	if err != nil {
		return "", err
	}
	cfgPath := filepath.Join(home, "config.toml")
	oldCfg, err := readOptional(cfgPath)
	if err != nil {
		return "", err
	}
	newCfg := withCodexBlock(oldCfg, codexBlock(home, self, trusted))
	if !changedHooks && bytes.Equal(oldCfg, newCfg) && allTrusted(metas, keys) {
		return fmt.Sprintf("piggery is already set up in %s (hooks trusted)", home), nil
	}
	if !bytes.Equal(oldCfg, newCfg) {
		msg, err := backupHumanConfig(dir, "codex", cfgPath, contains(codexBlockBegin))
		if err != nil {
			return "", err
		}
		backups = append(backups, msg)
		if err := writeFileAtomic(cfgPath, newCfg); err != nil {
			return "", err
		}
	}
	if metas, err = codexHooksList(home); err != nil {
		return "", err
	}
	if bad := untrustedPiggeryHooks(metas, keys); len(bad) > 0 {
		return "", fmt.Errorf("Codex does not trust piggery's hooks after setup: %s", strings.Join(bad, ", "))
	}
	return fmt.Sprintf("wrote piggery's hooks to %s and its MCP server and hook trust to %s (%d hooks trusted)\n%s"+
		"Codex sessions started from now on join piggery; restart any that are open.", hooksPath, cfgPath, len(keys), backupLines(backups)), nil
}

// codexHookGroups walks hooks.json (order and the Human's groups kept): fn gets each event's
// groups without piggery's and returns the list to write (empty drops the event, and an empty
// "hooks" drops that key).
func codexHookGroups(old []byte, fn func(event string, kept []json.RawMessage) []json.RawMessage) (jsonobj.Object, error) {
	doc, err := jsonobj.Parse(old)
	if err != nil {
		return nil, err
	}
	var hooks jsonobj.Object
	if raw, ok := doc.Get("hooks"); ok {
		if hooks, err = jsonobj.Parse(raw); err != nil {
			return nil, fmt.Errorf("hooks: %w", err)
		}
	}
	for _, ev := range codexHookEvents {
		var groups, kept []json.RawMessage
		if raw, ok := hooks.Get(ev.name); ok {
			if err := json.Unmarshal(raw, &groups); err != nil {
				return nil, fmt.Errorf("hooks.%s: %w", ev.name, err)
			}
		}
		for _, g := range groups {
			if !isPiggeryCodexGroup(g) {
				kept = append(kept, g)
			}
		}
		if out := fn(ev.name, kept); len(out) > 0 {
			hooks = hooks.Set(ev.name, rawArray(out))
		} else {
			hooks = hooks.Del(ev.name)
		}
	}
	if len(hooks) > 0 {
		doc = doc.Set("hooks", hooks.Bytes(1))
	} else {
		doc = doc.Del("hooks")
	}
	return doc, nil
}

// mergeCodexHooks returns hooks.json with the Human's groups kept and piggery's own group for
// each event at the end, and the trust key of each piggery hook ("<file>:<event>:<group>:0").
func mergeCodexHooks(old []byte, path, self string) ([]byte, []string, error) {
	var keys []string
	doc, err := codexHookGroups(old, func(event string, kept []json.RawMessage) []json.RawMessage {
		timeout := 10
		if event == "Interrupt" || event == "SessionEnd" {
			timeout = 3 // Codex allows 1–3 s for these
		}
		for _, ev := range codexHookEvents {
			if ev.name == event {
				keys = append(keys, fmt.Sprintf("%s:%s:%d:0", path, ev.key, len(kept)))
			}
		}
		g, _ := json.Marshal(map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": codexHookCommand(self, event), "timeout": timeout}}})
		return append(kept, g)
	})
	if err != nil {
		return nil, nil, err
	}
	return doc.Bytes(0), keys, nil
}

// isPiggeryCodexGroup: a group whose handlers are all piggery's (`… hook codex <Event>`).
func isPiggeryCodexGroup(raw json.RawMessage) bool {
	var g struct {
		Hooks []struct{ Command string } `json:"hooks"`
	}
	if json.Unmarshal(raw, &g) != nil || len(g.Hooks) == 0 {
		return false
	}
	for _, h := range g.Hooks {
		if !strings.Contains(h.Command, " hook codex ") {
			return false
		}
	}
	return true
}

// removeCodex takes piggery's hook groups out of hooks.json (the file goes when nothing else is
// left) and its block out of config.toml.
func removeCodex(home string) (string, error) {
	var did []string
	hooksPath := filepath.Join(home, "hooks.json")
	old, err := readOptional(hooksPath)
	if err != nil {
		return "", err
	}
	if old != nil {
		doc, err := codexHookGroups(old, func(_ string, kept []json.RawMessage) []json.RawMessage { return kept })
		if err != nil {
			return "", fmt.Errorf("%s: %w", hooksPath, err)
		}
		switch b := doc.Bytes(0); {
		case len(doc) == 0:
			if err := os.Remove(hooksPath); err != nil {
				return "", err
			}
			did = append(did, "removed "+hooksPath)
		case !bytes.Equal(b, old):
			if err := writeFileAtomic(hooksPath, b); err != nil {
				return "", err
			}
			did = append(did, "removed piggery's hooks from "+hooksPath)
		}
	}
	cfgPath := filepath.Join(home, "config.toml")
	cfg, err := readOptional(cfgPath)
	if err != nil {
		return "", err
	}
	if cfg != nil {
		switch b := withCodexBlock(cfg, ""); {
		case len(bytes.TrimSpace(b)) == 0:
			if err := os.Remove(cfgPath); err != nil {
				return "", err
			}
			did = append(did, "removed "+cfgPath)
		case !bytes.Equal(b, cfg):
			if err := writeFileAtomic(cfgPath, b); err != nil {
				return "", err
			}
			did = append(did, "removed piggery's MCP server and hook trust from "+cfgPath)
		}
	}
	if len(did) == 0 {
		return "codex: piggery is not installed", nil
	}
	return "codex: " + strings.Join(did, "\ncodex: "), nil
}

// piggeryHookState is the trust state to write for piggery's hooks: Codex's own key for each (it
// names the file by its resolved path, which may differ from the path given) and its current hash.
func piggeryHookState(metas []codexHookMeta, hooksPath string, keys []string) ([]codexHookMeta, error) {
	var state []codexHookMeta
	for _, k := range keys {
		m, ok := findHook(metas, hooksPath, k)
		if !ok || m.CurrentHash == "" {
			return nil, fmt.Errorf("Codex did not list piggery's hook %s (is %s valid?)", k, hooksPath)
		}
		state = append(state, m)
	}
	return state, nil
}

// findHook finds key in hooks/list; Codex may report the file under its resolved path.
func findHook(metas []codexHookMeta, hooksPath, key string) (codexHookMeta, bool) {
	suffix := strings.TrimPrefix(key, hooksPath)
	want, _ := filepath.EvalSymlinks(hooksPath)
	for _, m := range metas {
		if m.Key == key {
			return m, true
		}
		if src, _ := filepath.EvalSymlinks(m.SourcePath); src != "" && src == want && strings.HasSuffix(m.Key, suffix) {
			return m, true
		}
	}
	return codexHookMeta{}, false
}

func allTrusted(metas []codexHookMeta, keys []string) bool {
	return len(untrustedPiggeryHooks(metas, keys)) == 0
}

// untrustedPiggeryHooks names piggery's hooks Codex will not run (missing or not trusted).
func untrustedPiggeryHooks(metas []codexHookMeta, keys []string) []string {
	var bad []string
	for _, k := range keys {
		hooksPath := k[:strings.Index(k, ".json:")+len(".json")]
		m, ok := findHook(metas, hooksPath, k)
		if !ok || m.TrustStatus != "trusted" {
			status := "missing"
			if ok {
				status = m.TrustStatus
			}
			bad = append(bad, fmt.Sprintf("%s (%s)", k[len(hooksPath)+1:], status))
		}
	}
	return bad
}

// codexBlock is piggery's part of config.toml. The MCP server gets CODEX_HOME: Codex does not
// pass it on, and without it `codex queue` (the wake) writes to another home that no TUI reads
// (captured: exit 0, nothing queued).
func codexBlock(home, self string, state []codexHookMeta) string {
	var b strings.Builder
	b.WriteString(codexBlockBegin + "\n" + codexMarker(local.IntegrationVersion("codex")) + "\n")
	fmt.Fprintf(&b, "[mcp_servers.piggery]\ncommand = %s\nargs = [\"mcp\"]\nenv = { CODEX_HOME = %s }\ndefault_tools_approval_mode = \"approve\"\n", strconv.Quote(self), strconv.Quote(home))
	for _, m := range state {
		fmt.Fprintf(&b, "\n[hooks.state.%s]\ntrusted_hash = %s\n", strconv.Quote(m.Key), strconv.Quote(m.CurrentHash))
	}
	b.WriteString(codexBlockEnd + "\n")
	return b.String()
}

var tomlHeader = regexp.MustCompile(`^\s*\[\[?\s*([^\]]+?)\s*\]\]?\s*(#.*)?$`)

// withCodexBlock replaces piggery's block in config.toml (or appends it; "" removes it) and drops tables outside
// it that it defines again: [mcp_servers.piggery…] (e.g. from `codex mcp add piggery`) and the
// trust state of piggery's hooks (e.g. written by /hooks). Everything else is kept byte for byte.
func withCodexBlock(cfg []byte, block string) []byte {
	own := map[string]bool{}
	for _, line := range strings.Split(block, "\n") {
		if m := tomlHeader.FindStringSubmatch(line); m != nil {
			own[m[1]] = true
		}
	}
	var out []string
	inBlock, dropping := false, false
	for _, line := range strings.SplitAfter(string(cfg), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == codexBlockBegin:
			inBlock = true
			continue
		case t == codexBlockEnd && inBlock:
			inBlock = false
			continue
		case inBlock:
			continue
		}
		if m := tomlHeader.FindStringSubmatch(line); m != nil {
			name := m[1]
			dropping = name == "mcp_servers.piggery" || strings.HasPrefix(name, "mcp_servers.piggery.") || own[name]
		}
		if !dropping && line != "" {
			out = append(out, line)
		}
	}
	s := strings.Join(out, "")
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if block == "" { // removed: without the blank line that set it apart
		return []byte(strings.TrimRight(s, "\n") + strings.Repeat("\n", min(len(s), 1)))
	}
	if s != "" && !strings.HasSuffix(s, "\n\n") {
		s += "\n"
	}
	return []byte(s + block)
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeFileAtomic replaces path (0600, dirs 0700) through a temp file in the same directory.
func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".piggery-tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// listCodexHooks runs `codex app-server --stdio` for home just long enough for initialize and
// hooks/list: no thread starts, so no model turn. PIGGERY_DISABLED keeps piggery's own MCP
// server, if Codex starts it, out of piggery.
func listCodexHooks(home string) ([]codexHookMeta, error) {
	cmd := exec.Command("codex", "app-server", "--listen", "stdio://")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "PIGGERY_DISABLED=1")
	cmd.Dir = home
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex app-server: %w (is codex on PATH?)", err)
	}
	defer func() {
		in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	type reply struct {
		ID     *int            `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	replies := make(chan reply, 8)
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var r reply
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.ID != nil {
				replies <- r
			}
		}
		close(replies)
	}()
	call := func(id int, method string, params any) (json.RawMessage, error) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := in.Write(append(b, '\n')); err != nil {
			return nil, err
		}
		timeout := time.After(20 * time.Second)
		for {
			select {
			case r, ok := <-replies:
				if !ok {
					return nil, errors.New("codex app-server exited")
				}
				if *r.ID != id {
					continue
				}
				if r.Error != nil {
					return nil, fmt.Errorf("codex app-server %s: %s", method, r.Error.Message)
				}
				return r.Result, nil
			case <-timeout:
				return nil, fmt.Errorf("codex app-server %s: no answer", method)
			}
		}
	}
	if _, err := call(1, "initialize", map[string]any{"clientInfo": map[string]any{"name": "piggery", "version": Version}}); err != nil {
		return nil, err
	}
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","method":"initialized"}`+"\n")
	raw, err := call(2, "hooks/list", map[string]any{"cwds": []string{home}})
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []struct {
			Hooks []codexHookMeta `json:"hooks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var metas []codexHookMeta
	for _, d := range res.Data {
		for _, h := range d.Hooks {
			if !slices.ContainsFunc(metas, func(m codexHookMeta) bool { return m.Key == h.Key }) {
				metas = append(metas, h)
			}
		}
	}
	return metas, nil
}

// codexStatus: piggery's hooks and block in the Codex home, running self, and trusted by Codex.
func codexStatus(home, self string) harnessState {
	st := harnessState{Name: "codex"}
	fix := "piggery setup codex"
	hooksPath := filepath.Join(home, "hooks.json")
	b, err := readOptional(hooksPath)
	if err != nil {
		st.Problems = append(st.Problems, problem{err.Error(), ""})
		return st
	}
	var keys []string
	stale := false
	if b != nil {
		var doc struct {
			Hooks map[string][]json.RawMessage `json:"hooks"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			st.Problems = append(st.Problems, problem{hooksPath + ": " + err.Error(), ""})
			return st
		}
		for _, ev := range codexHookEvents {
			for i, g := range doc.Hooks[ev.name] {
				if isPiggeryCodexGroup(g) {
					keys = append(keys, fmt.Sprintf("%s:%s:%d:0", hooksPath, ev.key, i))
					stale = stale || !bytes.Contains(g, jsonobj.String(codexHookCommand(self, ev.name)))
				}
			}
		}
	}
	cfg, err := readOptional(filepath.Join(home, "config.toml"))
	if err != nil {
		st.Problems = append(st.Problems, problem{err.Error(), ""})
		return st
	}
	block := strings.Contains(string(cfg), codexBlockBegin)
	if len(keys) == 0 && !block {
		return st
	}
	st.Installed = true
	switch {
	case len(keys) == 0:
		st.Problems = append(st.Problems, problem{"piggery's hooks are missing from " + hooksPath, fix})
	case len(keys) < len(codexHookEvents):
		st.Problems = append(st.Problems, problem{fmt.Sprintf("only %d of piggery's %d hooks are in %s", len(keys), len(codexHookEvents), hooksPath), fix})
	case stale:
		st.Problems = append(st.Problems, problem{"piggery's hooks run another piggery", fix})
	}
	if !block {
		st.Problems = append(st.Problems, problem{"piggery's MCP server and hook trust are missing from config.toml", fix})
	} else if !strings.Contains(string(cfg), "command = "+strconv.Quote(self)+"\n") {
		st.Problems = append(st.Problems, problem{"piggery's MCP server runs another piggery", fix})
	}
	if len(keys) == 0 || len(st.Problems) > 0 {
		return st
	}
	metas, err := codexHooksList(home)
	if err != nil {
		st.Problems = append(st.Problems, problem{"cannot check the hooks' trust: " + err.Error(), ""})
		return st
	}
	if bad := untrustedPiggeryHooks(metas, keys); len(bad) > 0 {
		st.Problems = append(st.Problems, problem{"Codex will not run piggery's hooks (not trusted: " + strings.Join(bad, ", ") + ")", fix})
		return st
	}
	st.Detail = fmt.Sprintf("MCP server, %d hooks trusted", len(keys))
	return st
}
