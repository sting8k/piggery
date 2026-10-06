package view

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// A solo goes to the deepest team root holding its cwd (two roots nested), else to its own
// directory; in one directory units are oldest first with a closed team last. Directories sort by
// bucket (live, sleeping: nothing working and no turn for over a day, all gone, closed) and inside it
// by a fixed time: live by the latest real turn (an older team whose turn ended later is above a newer
// one), sleeping by creation, and a directory takes its best bucket. Contact that is not a turn (a
// reconnect, a daemon restart) neither wakes a directory nor moves it.
func TestGroupByDir(t *testing.T) {
	skipSlashPaths(t)
	now := time.UnixMilli(1_000_000_000_000)
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	h := time.Hour
	idle := func(turn time.Duration) []core.MemberState { // its last turn was turn ago; it was in contact just now
		return []core.MemberState{{State: "idle", LastTurnEnd: ago(turn), LastActivity: ago(time.Second), StateSince: ago(time.Second)}}
	}
	teams := []core.TeamState{
		{ID: "a", Root: "/p", CreatedAt: ago(50 * h), Members: idle(1 * h)},
		{ID: "b", Root: "/p/sub", CreatedAt: ago(30 * h), Members: idle(3 * h)},
		{ID: "s", Root: "/z", CreatedAt: ago(26 * h), Members: idle(48 * h)}, // nothing for over a day, though newer than the live ones below
		{ID: "g", Root: "/g", CreatedAt: ago(200 * h), Members: []core.MemberState{{State: "gone", StateSince: ago(70 * h)}}},
	}
	closed := []core.ClosedTeam{
		{TeamState: core.TeamState{ID: "c", Root: "/p", CreatedAt: ago(300 * h)}, ClosedAt: ago(1 * h)},
		{TeamState: core.TeamState{ID: "d1", Root: "/d1", CreatedAt: ago(300 * h)}, ClosedAt: ago(5 * h)},
		{TeamState: core.TeamState{ID: "d2", Root: "/d2", CreatedAt: ago(300 * h)}, ClosedAt: ago(20 * h)},
	}
	solos := []core.SoloState{
		{ID: "in-sub", Cwd: "/p/sub/x", State: "idle", CreatedAt: ago(20 * h), LastActivity: ago(1 * h)},
		{ID: "in-p", Cwd: "/p/other", State: "idle", CreatedAt: ago(60 * h), LastActivity: ago(2 * h), LastTurnEnd: ago(2 * h)},
		{ID: "alone", Cwd: "/q", State: "idle", CreatedAt: ago(40 * h), LastActivity: ago(30 * time.Minute), LastTurnEnd: ago(30 * time.Minute)}, // the most active
		// no turn for two days, though a daemon restart just moved its activity and state: sleeping
		{ID: "napping", Cwd: "/n", State: "idle", CreatedAt: ago(100 * h), LastActivity: ago(time.Minute), StateSince: ago(time.Minute), LastTurnEnd: ago(48 * h)},
		{ID: "gone-solo", Cwd: "/h", State: "gone", CreatedAt: ago(300 * h), StateSince: ago(10 * h)},
	}
	roots := []string{"/p", "/p/sub", "/z", "/g", "/d1", "/d2"}
	order := func(gs []DirGroup) (dirs []string, units map[string][]string) {
		units = map[string][]string{}
		for _, g := range gs {
			dirs = append(dirs, g.Dir)
			for _, u := range g.Units {
				id := ""
				if u.Solo != nil {
					id = u.Solo.ID
				} else {
					id = u.Team.ID
				}
				units[g.Dir] = append(units[g.Dir], id)
			}
		}
		return dirs, units
	}

	dirs, units := order(GroupByDir(teams, closed, solos, roots, now))
	// live by latest turn (/q: 30m, /p: 1h, /p/sub: 3h; /q was created before /p/sub but its turn is
	// newer), then sleeping, all gone (newest going first), closed (newest closing first)
	if want := []string{"/q", "/p", "/p/sub", "/z", "/n", "/h", "/g", "/d1", "/d2"}; !slices.Equal(dirs, want) {
		t.Fatalf("directories %v; want %v", dirs, want)
	}
	if !slices.Equal(units["/p/sub"], []string{"b", "in-sub"}) || !slices.Equal(units["/p"], []string{"in-p", "a", "c"}) ||
		!slices.Equal(units["/q"], []string{"alone"}) {
		t.Fatalf("units %v; want each solo under its deepest root, oldest first, closed last", units)
	}

	// a reconnect (activity and state change now, no turn) moves nothing
	solos[2].LastActivity, solos[2].StateSince = ago(time.Second), ago(time.Second)
	solos[0].LastActivity, solos[0].StateSince = ago(time.Second), ago(time.Second) // in-sub, in /p/sub
	dirs, _ = order(GroupByDir(teams, closed, solos, roots, now))
	if want := []string{"/q", "/p", "/p/sub"}; !slices.Equal(dirs[:3], want) {
		t.Fatalf("directories %v; want a reconnect to leave the order as it was", dirs)
	}

	// a new team in /p, which has not run a turn, counts from its creation: /p goes on top
	teams = append(teams, core.TeamState{ID: "n", Root: "/p", CreatedAt: ago(10 * time.Minute)})
	dirs, _ = order(GroupByDir(teams, closed, solos, roots, now))
	if want := []string{"/p", "/q", "/p/sub"}; !slices.Equal(dirs[:3], want) {
		t.Fatalf("directories %v; want the new team's /p first", dirs)
	}
}

func skipSlashPaths(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's paths are unix paths; TestRootsAndRelCwdFollowTheOSSeparator covers Windows ones")
	}
}

// A root holds a cwd and a cwd is shown relative to its directory whatever the OS separator is.
func TestRootsAndRelCwdFollowTheOSSeparator(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if got := deepestRoot(sub, []string{filepath.Dir(root), root}); got != filepath.Clean(root) {
		t.Fatalf("deepestRoot = %q; want %q", got, root)
	}
	if got := RelCwd(root, sub); got != "./a/b" {
		t.Fatalf("RelCwd = %q; want ./a/b", got)
	}
	if got := deepestRoot(sub+"x", []string{sub}); got != sub+"x" {
		t.Fatalf("deepestRoot of a sibling with the same prefix = %q; want itself", got)
	}
}
