package core

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/yamlfill"
	"gopkg.in/yaml.v3"
)

// yamlKeys is the yaml keys of struct type t.
func yamlKeys(t reflect.Type) []string {
	var keys []string
	for i := range t.NumField() {
		if k, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); k != "" && k != "-" {
			keys = append(keys, k)
		}
	}
	return keys
}

// teamKeys are the parser's team keys that every template writes. taskforce is the author's to add:
// a filled block would make every template callable as a taskforce.
func teamKeys() []string {
	keys := slices.DeleteFunc(yamlKeys(reflect.TypeOf(manifest{})), func(k string) bool { return k == "taskforce" })
	return append(keys, "timers") // timers: parseTimers
}

// Every built-in template writes every field a manifest takes, defaults too: the keys come from the
// manifest's own types and knownLimits, so a field added without its template line fails here. And
// every role of a built-in has send.
func TestBuiltinTemplatesWriteEveryField(t *testing.T) {
	files, _ := filepath.Glob("../../manifests/*.yaml")
	if len(files) == 0 {
		t.Fatal("no built-in templates found")
	}
	team := teamKeys()
	role := yamlKeys(reflect.TypeOf(roleSpec{}))
	spawn := yamlKeys(reflect.TypeOf(roleSpec{}.Spawn))
	route := yamlKeys(reflect.TypeOf(routeRule{}))
	var limits []string
	for k := range knownLimits {
		limits = append(limits, k)
	}
	missing := func(file, where string, m map[string]any, want []string) {
		for _, k := range want {
			if _, ok := m[k]; !ok {
				t.Errorf("%s: %s has no %s", filepath.Base(file), where, k)
			}
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := yaml.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		missing(f, "the team", m, team)
		lim, _ := m["limits"].(map[string]any)
		missing(f, "limits", lim, limits)
		for i, r := range m["routing"].([]any) {
			missing(f, "routing["+strconv.Itoa(i)+"]", r.(map[string]any), route)
		}
		for name, r := range m["roles"].(map[string]any) {
			rm := r.(map[string]any)
			want := slices.DeleteFunc(slices.Clone(role), func(k string) bool { return k == "instructions" || k == "instructions_file" })
			missing(f, "role "+name, rm, want)
			if _, a := rm["instructions"]; !a {
				if _, b := rm["instructions_file"]; !b {
					t.Errorf("%s: role %s has neither instructions nor instructions_file", filepath.Base(f), name)
				}
			}
			sp, _ := rm["spawn"].(map[string]any)
			missing(f, "role "+name+" spawn", sp, spawn)
			// Every role of a built-in talks with send; routing, not the tool list, fences it.
			if tools, _ := rm["tools"].([]any); !slices.Contains(tools, any("send")) {
				t.Errorf("%s: role %s has no send", filepath.Base(f), name)
			}
		}
	}
}

// inherit (spawn settings) and none (limits) read as if left out; another word is an error.
func TestInheritAndNone(t *testing.T) {
	m, err := parseManifest(`template: x
roles:
  r: {spawn: {harness: inherit, model: inherit, thinking: inherit}}
limits: {depth: 2, max_respawn_per_hour: none}
`)
	if err != nil {
		t.Fatal(err)
	}
	if s := m.Roles["r"].Spawn; s.Harness != "" || s.Model != "" || s.Thinking != "" {
		t.Fatalf("spawn %+v; want inherit read as empty", s)
	}
	if _, ok := m.Limits["max_respawn_per_hour"]; ok || m.Limits["depth"] != 2 {
		t.Fatalf("limits %v; want max_respawn_per_hour left out, depth kept", m.Limits)
	}
	for _, bad := range []string{"limits: {depth: lots}", "limits: {depthz: none}"} {
		if _, err := parseManifest("template: x\nroles: {r: {}}\n" + bad); err == nil {
			t.Fatalf("%s: accepted", bad)
		}
	}
}

// The keys filled into a template are exactly the keys the parser reads: a field added to the
// manifest types without its default fails here.
func TestManifestKeysMatchTheParser(t *testing.T) {
	names := func(ks []yamlfill.Key) []string {
		var out []string
		for _, k := range ks {
			out = append(out, k.Name)
		}
		slices.Sort(out)
		return out
	}
	sorted := func(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }
	field := func(ks []yamlfill.Key, name string) yamlfill.Key {
		return ks[slices.IndexFunc(ks, func(k yamlfill.Key) bool { return k.Name == name })]
	}
	role := slices.DeleteFunc(yamlKeys(reflect.TypeOf(roleSpec{})), func(k string) bool { return k == "instructions" || k == "instructions_file" })
	var limits []string
	for k := range knownLimits {
		limits = append(limits, k)
	}
	for _, c := range []struct {
		where     string
		got, want []string
	}{
		{"team", names(manifestKeys), sorted(teamKeys())},
		{"role", names(field(manifestKeys, "roles").Each), sorted(role)},
		{"spawn", names(field(field(manifestKeys, "roles").Each, "spawn").Fields), sorted(yamlKeys(reflect.TypeOf(roleSpec{}.Spawn)))},
		{"routing", names(field(manifestKeys, "routing").Items), sorted(yamlKeys(reflect.TypeOf(routeRule{})))},
		{"limits", names(field(manifestKeys, "limits").Fields), sorted(limits)},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: filled keys %v, parser's %v", c.where, c.got, c.want)
		}
	}
}

// A template the user edited, missing keys: the values and comments it has stay, the missing
// keys get their defaults, it means the same, and a second run changes nothing. A template the
// parser refuses is not touched.
func TestFillManifestFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.yaml")
	src := "# my workflow\ntemplate: mine\nroles:\n  lead:\n    instructions: \"Lead.\"   # short\n    tools: [send, agent]\n    spawn:\n      model: openai/gpt-x\n"
	os.WriteFile(p, []byte(src), 0o600)
	added, err := FillManifestFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	m, err := parseManifest(string(b))
	if err != nil || !strings.HasPrefix(string(b), src) || m.Roles["lead"].Spawn.Model != "openai/gpt-x" ||
		!strings.Contains(string(b), "      harness: inherit\n") || !slices.Contains(added, "limits") {
		t.Fatalf("added %v, err %v:\n%s", added, err, b)
	}
	if again, err := FillManifestFile(p); err != nil || len(again) != 0 {
		t.Fatalf("second run: %v %v", again, err)
	}
	bad := "template: x\nroles: {r: {}}\nlimits: {depth: lots}\n"
	os.WriteFile(p, []byte(bad), 0o600)
	if _, err := FillManifestFile(p); err == nil {
		t.Fatal("a refused template was filled")
	}
	if b, _ := os.ReadFile(p); string(b) != bad {
		t.Fatalf("a refused template changed:\n%s", b)
	}
}
