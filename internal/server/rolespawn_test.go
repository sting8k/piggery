package server

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/manifests"
)

// config.yaml `roles`, per field: `<template>/<role>` over `<template>/*` over `<role>` over `*`; a
// field an entry leaves out falls to the next one that sets it, and a role no entry names gets the
// entries that match it (or nothing, which core takes as "go on to the template and the chain").
func TestRoleSpawnOrderAndFallThrough(t *testing.T) {
	pin := roleSpawnFor(map[string]RoleSpawn{
		"*":            {Harness: "pi", Model: "m-star", Thinking: "t-star"},
		"copilot":      {Model: "m-role"},
		"crew/*":       {Model: "m-tpl", Thinking: "t-tpl"},
		"crew/copilot": {Harness: "codex"},
	})
	for _, c := range []struct {
		template, role string
		want           core.SpawnPin
	}{
		{"crew", "copilot", core.SpawnPin{Harness: "codex", Model: "m-tpl", Thinking: "t-tpl"}},
		{"crew", "builder", core.SpawnPin{Harness: "pi", Model: "m-tpl", Thinking: "t-tpl"}},
		{"slp", "copilot", core.SpawnPin{Harness: "pi", Model: "m-role", Thinking: "t-star"}},
	} {
		if got := pin(c.template, c.role); got != c.want {
			t.Errorf("%s/%s: %+v, want %+v", c.template, c.role, got, c.want)
		}
	}
	if got := roleSpawnFor(nil)("crew", "copilot"); got != (core.SpawnPin{}) {
		t.Errorf("no entries: %+v", got)
	}
}

// An entry that cannot work is dropped with a line naming it, and the daemon goes on: a key no
// template has (or a role one lost), a harness piggery lacks, a pattern that is not a role; `inherit`
// reads as left out.
func TestRolesEntriesThatCannotWork(t *testing.T) {
	dir := t.TempDir()
	if _, err := manifests.Unpack(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(ConfigPath(dir), []byte(`roles:
  slp/lead: {harness: codex, model: gpt-x, thinking: inherit}
  slp/nobody: {model: x}
  nosuch/*: {model: x}
  "exec*": {model: x}
  peer: {harness: vim, model: y}
`), 0o600)
	set, err := LoadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept, warns := CheckRoles(dir, set.Roles)
	all := strings.Join(append(set.Warnings, warns...), "\n")
	for _, want := range []string{`"exec*"`, `harness "vim"`, `"slp/nobody"`, `"nosuch/*"`} {
		if !strings.Contains(all, want) {
			t.Errorf("no warning names %s:\n%s", want, all)
		}
	}
	if len(kept) != 2 || kept["slp/lead"] != (RoleSpawn{Harness: "codex", Model: "gpt-x"}) || kept["peer"] != (RoleSpawn{Model: "y"}) {
		t.Errorf("kept %+v", kept)
	}
}

// A key the settings file does not have still stops the daemon, and the error is for a person: the
// key, its line, where it sits, the keys that are known there, and what to do; no Go type names.
func TestUnknownConfigKeyMessage(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(ConfigPath(dir), []byte("harness: pi\nfoo: 1\nspawn:\n  crew/copilot: {model: x}\n"), 0o600)
	_, err := LoadSettings(dir)
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	msg := err.Error()
	for _, want := range []string{ConfigPath(dir), `line 2: unknown key "foo"`, "known: harness", `line 4: unknown key "crew/copilot" under spawn (known: allowed_roots)`, "update piggery"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	for _, leak := range []string{"configFile", "struct {", "not found in type"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the message shows %q:\n%s", leak, msg)
		}
	}
}

// config.yaml is read at each use, not kept from the daemon's start: an edit counts from the next
// spawn with no restart, and a file that cannot be read is an error there.
func TestConfigReadAtEachUse(t *testing.T) {
	dir := t.TempDir()
	if _, err := manifests.Unpack(dir); err != nil {
		t.Fatal(err)
	}
	s := &server{dir: dir, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, model := range []string{"a", "b"} {
		os.WriteFile(ConfigPath(dir), []byte("gate: closed\nroles:\n  slp/lead: {model: "+model+"}\n"), 0o600)
		c, err := s.engineConfig()
		if err != nil || c.RoleSpawn("slp", "lead").Model != model || !c.SoloGateClosed {
			t.Fatalf("after writing model %s: %+v, %v", model, c.RoleSpawn("slp", "lead"), err)
		}
	}
	os.WriteFile(ConfigPath(dir), []byte("roles: [\n"), 0o600)
	if _, err := s.engineConfig(); err == nil {
		t.Fatal("a config that cannot be parsed was read as fine")
	}
}
