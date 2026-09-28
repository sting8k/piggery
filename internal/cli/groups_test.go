package cli

import (
	"slices"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

// A solo goes to the deepest team root holding its cwd (two roots nested), else to its own
// directory; directories are listed most recently active first, and in one, units oldest first
// with a closed team last.
func TestGroupByDir(t *testing.T) {
	teams := []core.TeamState{
		{ID: "a", Root: "/p", CreatedAt: 10},
		{ID: "b", Root: "/p/sub", CreatedAt: 20, Members: []core.MemberState{{LastActivity: 500}}},
	}
	closed := []core.ClosedTeam{{TeamState: core.TeamState{ID: "c", Root: "/p", CreatedAt: 1}, ClosedAt: 30}}
	solos := []core.SoloState{
		{ID: "in-sub", Cwd: "/p/sub/x", CreatedAt: 40},
		{ID: "in-p", Cwd: "/p/other", CreatedAt: 5},
		{ID: "alone", Cwd: "/q", CreatedAt: 50, LastActivity: 900},
	}
	gs := groupByDir(teams, closed, solos, []string{"/p", "/p/sub"})
	var dirs []string
	units := map[string][]string{}
	for _, g := range gs {
		dirs = append(dirs, g.dir)
		for _, u := range g.units {
			id := ""
			switch {
			case u.solo != nil:
				id = u.solo.ID
			default:
				id = u.team.ID
			}
			units[g.dir] = append(units[g.dir], id)
		}
	}
	if !slices.Equal(dirs, []string{"/q", "/p/sub", "/p"}) {
		t.Fatalf("directories %v; want the most recently active first", dirs)
	}
	if !slices.Equal(units["/p/sub"], []string{"b", "in-sub"}) || !slices.Equal(units["/p"], []string{"in-p", "a", "c"}) ||
		!slices.Equal(units["/q"], []string{"alone"}) {
		t.Fatalf("units %v; want each solo under its deepest root, oldest first, closed last", units)
	}
}
