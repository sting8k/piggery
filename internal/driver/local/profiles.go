package local

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/sting8k/piggery/internal/jsonobj"
)

// The worker profiles ~/.piggery/harness/{pi,claude,codex}.json: each harness's default profile
// below is what setup writes and what a missing key is filled with. Every field is written (no
// omitempty), so the file shows every key the driver reads.

// Inherit as a profile's model or thinking: the chain goes on, as if left empty.
const Inherit = "inherit"

func inheritEmpty(vs ...*string) {
	for _, v := range vs {
		if *v == Inherit {
			*v = ""
		}
	}
}

// DefaultProfile is the default pi.json for the pi extension at ext (its index.ts).
func DefaultProfile(ext string) Profile {
	// The default blacklist is built into the driver.
	return Profile{Cmd: "pi", Args: []string{"--mode", "rpc", "-e", ext}, Model: Inherit, Thinking: Inherit,
		Blacklist: []string{}, TestedVersions: []string{"0.87.1"}}
}

// DefaultClaudeProfile is the default claude.json: the claude symlink, Claude's
// own default tools minus the ones a worker must not have (they reach the Human, work around
// piggery's mail and workers, or schedule turns of their own), and nothing of the Human's Claude
// setup blacklisted.
var DefaultClaudeProfile = ClaudeProfile{
	Cmd: "claude", Args: []string{}, Env: []string{}, Model: Inherit, Thinking: Inherit,
	DisallowedTools: []string{"Agent", "SendMessage", "ListAgents", "Workflow", "CronCreate", "ScheduleWakeup",
		"Monitor", "RemoteTrigger", "AskUserQuestion", "EnterPlanMode", "ExitPlanMode", "EnterWorktree", "ExitWorktree",
		"PushNotification", "SendUserFile", "Artifact", "ShareOnboardingGuide", "SendFeedback"},
	Blacklist:      []string{},
	TestedVersions: []string{"2.1.283"},
}

// DefaultCodexProfile is the default codex.json: the native tool
// groups a worker must not have (a role keeps one with allow_tools), no MCP server of the Human's
// blacklisted.
var DefaultCodexProfile = CodexProfile{
	Cmd: "codex", Args: []string{}, Env: []string{}, Model: Inherit, Thinking: Inherit,
	DisabledTools: map[string]string{"goals": "features.goals=false", "apps": "features.apps=false",
		"agents": "agents.enabled=false"},
	Blacklist:      []string{},
	TestedVersions: []string{"0.157.1"},
}

// DefaultProfiles are the profile files under dir with their defaults.
func DefaultProfiles(dir string) []struct {
	Path    string
	Default any
} {
	return []struct {
		Path    string
		Default any
	}{
		{ProfilePath(dir), DefaultProfile(filepath.Join(PiExtDir(dir), "index.ts"))},
		{ClaudeProfilePath(dir), DefaultClaudeProfile},
		{CodexProfilePath(dir), DefaultCodexProfile},
	}
}

// FillProfile adds to the profile at path every key of def it lacks, with def's value, after
// the keys it has; nothing it has changes. A file the driver would refuse (not JSON, a wrong
// type) is not touched: the error says why. No file: nothing to do.
func FillProfile(path string, def any) ([]string, error) {
	cur, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	typ := reflect.TypeOf(def)
	if err := json.Unmarshal(cur, reflect.New(typ).Interface()); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	o, err := jsonobj.Parse(cur)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defB, _ := json.Marshal(def)
	defs, _ := jsonobj.Parse(defB)
	var added []string
	for _, m := range defs {
		if _, ok := o.Get(m.Key); !ok {
			o = o.Set(m.Key, m.Val)
			added = append(added, m.Key)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, o.Bytes(0), mode); err != nil {
		return nil, err
	}
	return added, os.Rename(tmp, path)
}
