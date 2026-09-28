package yamlfill

import (
	"slices"
	"testing"
)

var schema = []Key{
	{Name: "model"}, // required
	{Name: "summary", Default: `""`},
	{Name: "roles", Each: []Key{
		{Name: "tools", Default: "[]"},
		{Name: "can_pin", Default: "false"},
		{Name: "spawn", Fields: []Key{{Name: "harness", Default: "inherit"}, {Name: "allow_tools", Default: "[]"}}},
	}},
	{Name: "routing", Default: "[]", Items: []Key{{Name: "allow", Default: "false"}, {Name: "cc", Default: "[]"}}},
	{Name: "limits", Fields: []Key{{Name: "depth", Default: "none"}, {Name: "max_hops", Default: "none"}}},
}

// Missing keys are added in each mapping's own style and nothing written before changes: comments,
// order, a block scalar ending a mapping, flow mappings, a file with no final newline. A second
// run adds nothing.
func TestFill(t *testing.T) {
	src := `# my team
model: mine   # name
roles:
  lead:
    # the lead's card
    instructions: |
      Line one.
      Line two.
  w: {tools: [send], spawn: {harness: pi}}
  x:
    spawn:
      harness: claude # keep
routing:
  - {from: lead, to: w, allow: true}
  - from: w
    to: lead
limits:
  depth: 3`
	want := `# my team
model: mine   # name
roles:
  lead:
    # the lead's card
    instructions: |
      Line one.
      Line two.
    tools: []
    can_pin: false
    spawn:
      harness: inherit
      allow_tools: []
  w: {tools: [send], spawn: {harness: pi, allow_tools: []}, can_pin: false}
  x:
    spawn:
      harness: claude # keep
      allow_tools: []
    tools: []
    can_pin: false
routing:
  - {from: lead, to: w, allow: true, cc: []}
  - from: w
    to: lead
    allow: false
    cc: []
limits:
  depth: 3
  max_hops: none
summary: ""
`
	got, added, err := Fill([]byte(src), schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if !slices.Contains(added, "roles.lead.spawn") || !slices.Contains(added, "routing[1].cc") || !slices.Contains(added, "summary") {
		t.Fatalf("added %v", added)
	}
	again, added, err := Fill(got, schema)
	if err != nil || string(again) != string(got) || len(added) != 0 {
		t.Fatalf("second run: added %v, err %v, changed %v", added, err, string(again) != string(got))
	}
}
