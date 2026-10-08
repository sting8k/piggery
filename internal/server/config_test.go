package server

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/store"
)

// Automatic gc on a fake clock: with gc off nothing is touched; with the defaults a team closed
// longer than closed_after goes with its participants' entries in logs/, run/ and sessions/ (and
// nothing else in the directory), a younger one stays, and an
// archive older than archive_keep is deleted while a newer one stays.
func TestAutoGC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	eng := core.New(db, core.WithClock(func() time.Time { return now }))
	logs := map[string]string{}  // team -> its member's run log dir
	own := map[string][]string{} // team -> its member's entries in run/ and sessions/
	closeTeam := func(name string) {
		t.Helper()
		team, err := eng.TeamUp(ctx, core.TeamUpArgs{Name: name, Cwd: t.TempDir(),
			Manifest: "template: m\nroles: {peer: {tools: [send]}}\n"})
		if err != nil {
			t.Fatal(err)
		}
		j, err := eng.Join(ctx, core.JoinArgs{Team: team.ID, Role: "peer", Name: "p", Cwd: team.RootCwd})
		if err != nil {
			t.Fatal(err)
		}
		logs[name] = filepath.Dir(local.LogPath(dir, j.ID, "r1"))
		if err := os.MkdirAll(logs[name], 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local.LogPath(dir, j.ID, "r1"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		own[name] = []string{filepath.Join(local.RunRoot(dir, "omp"), j.ID, "r1"), filepath.Join(local.SessionsRoot(dir, "dsh"), j.ID)}
		for _, p := range own[name] {
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, "f"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := eng.TeamDown(ctx, core.TeamDownArgs{Team: name}); err != nil {
			t.Fatal(err)
		}
	}
	closeTeam("old")
	now = now.Add(13 * 24 * time.Hour)
	closeTeam("young")
	now = now.Add(2 * 24 * time.Hour) // old closed 15 days ago, young 2
	if err := os.MkdirAll(ArchiveDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, age := range map[string]time.Duration{"stale.jsonl": 31 * 24 * time.Hour, "fresh.jsonl": 24 * time.Hour} {
		p := filepath.Join(ArchiveDir(dir), name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	if res, err := runGC(ctx, eng, dir, Settings{}); err != nil || len(res.Teams) != 0 || len(res.ExpiredArchives) != 0 ||
		!exists(logs["old"]) || !exists(filepath.Join(ArchiveDir(dir), "stale.jsonl")) {
		t.Fatalf("gc off: %+v, %v; want nothing touched", res, err)
	}

	set, err := LoadSettings(dir) // no config.yaml: the defaults
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "piggery.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := runGC(ctx, eng, dir, set)
	if err != nil || len(res.Teams) != 1 || res.Teams[0].Name != "old" || !res.Teams[0].Deleted || res.Teams[0].LogDirs != 3 {
		t.Fatalf("gc: %+v, %v; want old deleted with its log dir, run entry and session entry", res, err)
	}
	if exists(logs["old"]) || !exists(logs["young"]) {
		t.Fatalf("log dirs after gc: old %v (want gone), young %v (want kept)", exists(logs["old"]), exists(logs["young"]))
	}
	for _, p := range own["old"] {
		if exists(p) {
			t.Fatalf("%s kept after its participant's gc", p)
		}
	}
	for _, p := range append(own["young"], filepath.Join(dir, "piggery.db")) {
		if !exists(p) {
			t.Fatalf("%s deleted; only the entries of a deleted participant may go", p)
		}
	}
	if strings.Join(res.ExpiredArchives, " ") != "stale.jsonl" || !exists(filepath.Join(ArchiveDir(dir), "fresh.jsonl")) ||
		!exists(res.Teams[0].Archive) {
		t.Fatalf("archives: expired %v; want only stale.jsonl, fresh and old's new archive kept", res.ExpiredArchives)
	}
}

// A bad value or an unknown key in config.yaml stops the daemon at start, naming the key.
func TestBadConfigStopsTheDaemon(t *testing.T) {
	for conf, want := range map[string]string{
		"gc: {closed_after: 2 weeks}\n": "gc.closed_after",
		"gc: {keep: 30d}\n":             "keep",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(ConfigPath(dir), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := Run(ctx, Config{Dir: dir})
		cancel()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("config %q: Run = %v; want an error naming %s", conf, err, want)
		}
	}
}

// Every harness a built-in driver runs is a valid harness setting (harness: codex stopped the
// daemon: the list was written by hand).
func TestEveryHarnessIsASetting(t *testing.T) {
	for _, h := range local.Harnesses() {
		dir := t.TempDir()
		if err := os.WriteFile(ConfigPath(dir), []byte("harness: "+h+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if set, err := LoadSettings(dir); err != nil || set.Harness != h {
			t.Fatalf("harness %s: %+v, %v", h, set, err)
		}
	}
}

// yamlPaths is the dotted keys of struct type t (nested structs flattened).
func yamlPaths(t reflect.Type, prefix string) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		k, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if f.Type.Kind() == reflect.Struct {
			out = append(out, yamlPaths(f.Type, prefix+k+".")...)
		} else {
			out = append(out, prefix+k)
		}
	}
	return out
}

// setup writes config.yaml with every key LoadSettings takes, at the defaults LoadSettings
// starts from (a key added to the settings without setup writing it fails here).
func TestDefaultConfigHasEveryKey(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := EnsureConfig(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ConfigPath(dir))
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var got []string
	var walk func(m map[string]any, prefix string)
	walk = func(m map[string]any, prefix string) {
		for k, v := range m {
			if sub, ok := v.(map[string]any); ok {
				walk(sub, prefix+k+".")
			} else {
				got = append(got, prefix+k)
			}
		}
	}
	walk(m, "")
	want := yamlPaths(reflect.TypeOf(configFile{}), "")
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("config.yaml keys %v; want %v\n%s", got, want, b)
	}
	if set, err := LoadSettings(dir); err != nil || !reflect.DeepEqual(set, defaultSettings()) {
		t.Fatalf("written defaults load as %+v, %v", set, err)
	}
}

// An existing config.yaml keeps every byte of the user's (values, comments) and gets only its
// missing keys, inside the gc block; a second run changes nothing.
func TestEnsureConfigKeepsTheUsers(t *testing.T) {
	dir := t.TempDir()
	mine := "# my settings\ngc:\n    closed_after: 7d   # a week\n# end of gc\n"
	if err := os.WriteFile(ConfigPath(dir), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	added, _, err := EnsureConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ConfigPath(dir))
	for _, l := range strings.SplitAfter(mine, "\n") {
		if !strings.Contains(string(b), l) {
			t.Fatalf("user's line %q lost:\n%s", l, b)
		}
	}
	set, err := LoadSettings(dir)
	if err != nil || set.GCClosedAfter != 7*24*time.Hour || set.GCArchiveKeep != defaultSettings().GCArchiveKeep {
		t.Fatalf("after adding %v: %+v, %v\n%s", added, set, err, b)
	}
	if !slices.Equal(added, []string{"harness", "gate", "gc.archive_keep", "display.columns", "spawn.allowed_roots", "update.check", "prompts"}) || !strings.Contains(string(b), "    archive_keep: 30d") {
		t.Fatalf("added %v:\n%s", added, b)
	}
	if again, _, _ := EnsureConfig(dir); again != nil {
		t.Fatalf("second run added %v", again)
	}
	if b2, _ := os.ReadFile(ConfigPath(dir)); string(b2) != string(b) {
		t.Fatal("second run changed the file")
	}
}

// An unknown column is a display matter: the daemon starts; top and ps warn, naming the valid
// columns, and show the default ones.
func TestUnknownColumnFallsBack(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ConfigPath(dir), []byte("display: {columns: [role, colour]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := LoadSettings(dir)
	if err != nil {
		t.Fatalf("LoadSettings: %v; want no error for a display setting", err)
	}
	cols, warn := ColumnsOf(set, dir)
	if !slices.Equal(cols, DisplayColumns) || !strings.Contains(warn, `"colour" is not a column (valid: role, state,`) {
		t.Fatalf("columns %v, warning %q; want the defaults and the valid ones named", cols, warn)
	}
}
