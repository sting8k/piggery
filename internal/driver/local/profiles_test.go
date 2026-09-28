package local

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Each default profile writes every key its driver reads: a field added with
// omitempty, or left out of the default, fails here.
func TestDefaultProfilesWriteEveryKey(t *testing.T) {
	for _, p := range DefaultProfiles(t.TempDir()) {
		var got map[string]any
		b, _ := json.Marshal(p.Default)
		json.Unmarshal(b, &got)
		typ := reflect.TypeOf(p.Default)
		for i := range typ.NumField() {
			k, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if _, ok := got[k]; !ok || strings.Contains(typ.Field(i).Tag.Get("json"), "omitempty") {
				t.Errorf("%s: %s is not written", filepath.Base(p.Path), k)
			}
		}
	}
}

// A profile missing keys (written by an older setup) gets them with their defaults after its
// own, which keep every byte; model/thinking inherit read as left out; a second run adds
// nothing; a file the driver would refuse is not touched.
func TestFillProfile(t *testing.T) {
	dir := t.TempDir()
	path := ProfilePath(dir)
	os.MkdirAll(filepath.Dir(path), 0o700)
	old := "{\n  \"cmd\": \"pi\",\n  \"args\": [\n    \"--mode\",\n    \"rpc\"\n  ],\n  \"blacklist\": [\n    \"pi-boomerang\"\n  ]\n}\n"
	os.WriteFile(path, []byte(old), 0o600)
	added, err := FillProfile(path, DefaultProfile("/x/index.ts"))
	if err != nil || !slices.Equal(added, []string{"model", "thinking", "tested_versions"}) {
		t.Fatalf("added %v, %v", added, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), strings.TrimSuffix(old, "\n}\n")+",\n  \"model\": \"inherit\"") {
		t.Fatalf("file:\n%s", b)
	}
	p, err := piCodec{dir: dir}.profile()
	if err != nil || p.Model != "" || p.Thinking != "" || !slices.Equal(p.Blacklist, []string{"pi-boomerang"}) {
		t.Fatalf("profile %+v, %v", p, err)
	}
	if again, err := FillProfile(path, DefaultProfile("/x/index.ts")); err != nil || again != nil {
		t.Fatalf("second run: %v %v", again, err)
	}
	bad := `{"cmd": "pi", "model": 3}`
	os.WriteFile(path, []byte(bad), 0o600)
	if _, err := FillProfile(path, DefaultProfile("/x/index.ts")); err == nil {
		t.Fatal("a refused profile was filled")
	}
	if b, _ := os.ReadFile(path); string(b) != bad {
		t.Fatalf("a refused profile changed: %s", b)
	}
}
