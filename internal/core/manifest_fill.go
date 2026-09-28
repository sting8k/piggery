package core

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sting8k/piggery/internal/yamlfill"
)

// manifestKeys are a template's keys, each with what is written when it is missing: every default
// reads as if the key were left out (inherit, none, [], false, ""). model, roles and a rule's
// from/to are required and never added; a role's instructions or instructions_file is the author's
// to choose. The key set is kept equal to the manifest types by TestManifestKeysMatchTheParser.
var manifestKeys = []yamlfill.Key{
	{Name: "model"},
	{Name: "summary", Default: `""`},
	{Name: "auto_join_role", Default: `""`},
	{Name: "roles", Each: []yamlfill.Key{
		{Name: "description", Default: `""`},
		{Name: "tools", Default: "[]"},
		{Name: "can_spawn", Default: "[]"},
		{Name: "can_pin", Default: "false"},
		{Name: "can_set_cwd", Default: "false"},
		{Name: "spawn", Fields: []yamlfill.Key{
			{Name: "harness", Default: inherit},
			{Name: "model", Default: inherit},
			{Name: "thinking", Default: inherit},
			{Name: "allow_tools", Default: "[]"},
		}},
	}},
	{Name: "routing", Default: "[]", Items: []yamlfill.Key{
		{Name: "from"}, {Name: "to"},
		{Name: "allow", Default: "false"},
		{Name: "cc", Default: "[]"},
	}},
	{Name: "tools", Default: "{}"},
	{Name: "timers", Default: "[]"},
	{Name: "limits", Fields: []yamlfill.Key{
		{Name: "depth", Default: "none"},
		{Name: "concurrency", Default: "none"},
		{Name: "messages_per_participant_per_minute", Default: "none"},
		{Name: "messages_per_thread", Default: "none"},
		{Name: "max_hops", Default: "none"},
		{Name: "max_respawn_per_hour", Default: "none"},
	}},
}

// FillManifestFile adds the keys the template at path lacks and returns them. A template the
// parser refuses is not touched (the error says why); one whose meaning the added keys would
// change is not written either (never expected: every default reads as left out).
func FillManifestFile(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	before, err := parseManifest(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out, added, err := yamlfill.Fill(src, manifestKeys)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(added) == 0 {
		return nil, nil
	}
	after, err := parseManifest(string(out))
	tb, _ := parseTimers(string(src))
	ta, _ := parseTimers(string(out))
	if err != nil || fmt.Sprintf("%+v %+v", before, tb) != fmt.Sprintf("%+v %+v", after, ta) {
		return nil, fmt.Errorf("%s: adding %v would change what it means; left as it is", path, added)
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, out, mode); err != nil {
		return nil, err
	}
	return added, os.Rename(tmp, path)
}
