package cli

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/sting8k/piggery/internal/proto"
)

// What top remembers between runs (<dir>/cache/top.json, next to logs.json): which teams the user
// opened or folded, which teams' folded gone members were expanded, and whether the events list and the notices box were
// opened. Only where the user chose; the rest follows the defaults (a live team open, a dead or closed
// one folded, gone members folded, events folded). It is the CLI's own: the daemon never reads it, and losing it only costs the choices.
type topState struct {
	Teams map[string]bool `json:"teams,omitempty"` // team id -> its members are listed (true) or it is one line (false)
	Gone  map[string]bool `json:"gone,omitempty"`  // team id -> its gone members are listed, not folded into one line
	// Events is true when the user opened the events list (it starts folded to one line).
	Events bool `json:"events,omitempty"`
	// Notices is true when the user opened the notices box (it starts folded to one line, like the events).
	Notices bool `json:"notices,omitempty"`
	// Side is the sidebar's width in cells as the user dragged its edge (0: the default, a third of the window).
	Side int `json:"side,omitempty"`
}

func topStatePath(dir string) string { return filepath.Join(dir, "cache", "top.json") }

// loadTopState is dir's saved choices, empty when there are none or dir is "".
func loadTopState(dir string) topState {
	var s topState
	if dir != "" {
		if b, err := os.ReadFile(topStatePath(dir)); err == nil {
			_ = json.Unmarshal(b, &s)
		}
	}
	if s.Teams == nil {
		s.Teams = map[string]bool{}
	}
	if s.Gone == nil {
		s.Gone = map[string]bool{}
	}
	return s
}

// saveTopState writes s without the teams that no longer exist (known: ids of the snapshot's
// teams, open and closed), through a temporary file so a concurrent top never reads half of it.
// Errors are ignored.
func saveTopState(dir string, s topState, known map[string]bool) {
	if dir == "" {
		return
	}
	out := topState{Teams: map[string]bool{}, Gone: map[string]bool{}, Events: s.Events, Notices: s.Notices, Side: s.Side}
	for id, v := range s.Teams {
		if known[id] {
			out.Teams[id] = v
		}
	}
	for id, v := range s.Gone {
		if known[id] {
			out.Gone[id] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	path := topStatePath(dir)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".top-*.json")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
	}
}

// teamIDs are the ids of every team in ps, open and closed.
func teamIDs(ps proto.PsResult) map[string]bool {
	out := map[string]bool{}
	for _, t := range teamsOf(ps) {
		out[t.ID] = true
	}
	return out
}
