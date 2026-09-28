package cli

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sting8k/piggery/internal/core"
)

// top and ps list by project directory, then by unit: a directory is a team's
// root, or for a solo the deepest team root holding its cwd (else its own cwd); directories most
// recently active first. In a directory, teams and solos are peers (a solo is a team of one),
// oldest first, closed teams last. What a person looks for is "what runs in this project".

// unit is one entry of a directory: a team (open or closed) or a solo.
type unit struct {
	team   *core.TeamState  // an open team, or a closed one's state
	closed *core.ClosedTeam // set for a closed team
	solo   *core.SoloState
}

func (u unit) created() int64 {
	if u.solo != nil {
		return u.solo.CreatedAt
	}
	return u.team.CreatedAt
}

// active is the unit's latest activity: a member's or the solo's last activity or state change,
// the team's creation, a closed team's closing.
func (u unit) active() int64 {
	if u.solo != nil {
		return max(u.solo.LastActivity, u.solo.StateSince, u.solo.CreatedAt)
	}
	a := u.team.CreatedAt
	if u.closed != nil {
		a = max(a, u.closed.ClosedAt)
	}
	for _, m := range u.team.Members {
		a = max(a, m.LastActivity, m.StateSince)
	}
	return a
}

// dirGroup is a project directory and what runs in it.
type dirGroup struct {
	dir    string
	units  []unit
	active int64
}

// groupByDir groups the listed teams, closed teams and solos by directory. roots are every
// team's root (open and closed), where a solo's cwd is placed.
func groupByDir(teams []core.TeamState, closed []core.ClosedTeam, solos []core.SoloState, roots []string) []dirGroup {
	byDir := map[string]*dirGroup{}
	var order []*dirGroup
	add := func(dir string, u unit) {
		g := byDir[dir]
		if g == nil {
			g = &dirGroup{dir: dir}
			byDir[dir] = g
			order = append(order, g)
		}
		g.units = append(g.units, u)
		g.active = max(g.active, u.active())
	}
	for i := range teams {
		add(teams[i].Root, unit{team: &teams[i]})
	}
	for i := range closed {
		add(closed[i].Root, unit{team: &closed[i].TeamState, closed: &closed[i]})
	}
	for i := range solos {
		add(deepestRoot(solos[i].Cwd, roots), unit{solo: &solos[i]})
	}
	for _, g := range order {
		slices.SortStableFunc(g.units, func(a, b unit) int {
			closed := func(u unit) int {
				if u.closed != nil {
					return 1
				}
				return 0
			}
			return cmp.Or(cmp.Compare(closed(a), closed(b)), cmp.Compare(a.created(), b.created()))
		})
	}
	slices.SortStableFunc(order, func(a, b *dirGroup) int {
		return cmp.Or(cmp.Compare(b.active, a.active), strings.Compare(a.dir, b.dir))
	})
	out := make([]dirGroup, len(order))
	for i, g := range order {
		out[i] = *g
	}
	return out
}

// deepestRoot is the longest root that is cwd or holds it; none: cwd itself.
func deepestRoot(cwd string, roots []string) string {
	best := ""
	for _, r := range roots {
		r = filepath.Clean(r)
		if (cwd == r || strings.HasPrefix(cwd, strings.TrimSuffix(r, "/")+"/")) && len(r) > len(best) {
			best = r
		}
	}
	if best == "" {
		return cwd
	}
	return best
}

// relCwd is a row's cwd in its directory's group: "" when it is the directory, ./sub under it,
// else the path (~ form).
func relCwd(dir, cwd string) string {
	if cwd == "" || cwd == dir {
		return ""
	}
	if rel, err := filepath.Rel(dir, cwd); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return "./" + rel
	}
	return home(cwd)
}
