package view

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// top and ps list by project directory, then by unit: a directory is a team's
// root, or for a solo the deepest team root holding its cwd (else its own cwd); live directories
// by their latest real turn first, then sleeping, all gone and closed ones. Only a real turn reorders
// a live directory: a reconnect or a daemon restart moves nothing. In a directory, teams and solos
// are peers (a solo is a team of one), oldest first, closed teams last. What a person looks for is
// "what runs in this project".

// Unit is one entry of a directory: a team (open or closed) or a solo.
type Unit struct {
	Team   *core.TeamState  // an open team, or a closed one's state
	Closed *core.ClosedTeam // set for a closed team
	Solo   *core.SoloState
	// Under is the participant the unit is listed under: a taskforce sits under its caller (the
	// solo, or the team of the member that called it up), Depth steps in. "" for every other unit,
	// and for a taskforce whose caller is not in the list (another tab, gone with its team).
	Under string
	Depth int
}

func (u Unit) created() int64 {
	if u.Solo != nil {
		return u.Solo.CreatedAt
	}
	return u.Team.CreatedAt
}

// active is the unit's latest activity: a member's or the solo's last activity or state change,
// the team's creation, a closed team's closing.
func (u Unit) Active() int64 {
	if u.Solo != nil {
		return max(u.Solo.LastActivity, u.Solo.StateSince, u.Solo.CreatedAt)
	}
	a := u.Team.CreatedAt
	if u.Closed != nil {
		a = max(a, u.Closed.ClosedAt)
	}
	for _, m := range u.Team.Members {
		a = max(a, m.LastActivity, m.StateSince)
	}
	return a
}

// sleepAfter is how long a unit with no activity stays "live" before its directory is sorted as
// sleeping.
const sleepAfter = 24 * time.Hour

// Where a directory sorts: live, sleeping, all gone, closed. Ordering only: top, ps and Paseo draw no
// header for them.
const (
	bucketLive = iota
	bucketSleeping
	bucketGone
	bucketClosed
)

var bucketNames = [...]string{bucketLive: "live", bucketSleeping: "sleeping", bucketGone: "gone", bucketClosed: "closed"}

// bucket is where the unit sorts at now: closed; all gone (a gone solo, or an open team whose members
// are all gone); sleeping when nothing works and its latest turn is over sleepAfter old; else live.
func (u Unit) bucket(now time.Time) int {
	switch {
	case u.Closed != nil:
		return bucketClosed
	case u.Solo != nil && u.Solo.State == "gone", u.Solo == nil && Dead(*u.Team):
		return bucketGone
	case u.sleeping(now):
		return bucketSleeping
	}
	return bucketLive
}

// sleeping reports that nothing in the unit works (or waits on a permission) and its latest real turn
// is over sleepAfter old. A turn is what `last_turn_end` records: a reconnect or a daemon restart
// moves last_activity and state_since but is not one, so neither is read here. A member or a solo
// that never ran a turn counts from when it joined.
func (u Unit) sleeping(now time.Time) bool {
	busy := func(state string) bool { return state == "working" || state == "awaiting_permission" }
	var last int64
	if u.Solo != nil {
		if busy(u.Solo.State) {
			return false
		}
		last = cmp.Or(u.Solo.LastTurnEnd, u.Solo.CreatedAt)
	} else {
		last = u.Team.CreatedAt
		if len(u.Team.Members) > 0 {
			last = 0
		}
		for _, m := range u.Team.Members {
			if busy(m.State) {
				return false
			}
			last = max(last, cmp.Or(m.LastTurnEnd, m.CreatedAt))
		}
	}
	return now.UnixMilli()-last > sleepAfter.Milliseconds()
}

// lastTurn is the unit's latest real turn: a solo's last_turn_end, or the newest of its members';
// its creation when none ran a turn. A reconnect or a daemon restart is not a turn, so
// last_activity and state_since are not read.
func (u Unit) lastTurn() int64 {
	if u.Solo != nil {
		return cmp.Or(u.Solo.LastTurnEnd, u.Solo.CreatedAt)
	}
	var last int64
	for _, m := range u.Team.Members {
		last = max(last, m.LastTurnEnd)
	}
	return cmp.Or(last, u.Team.CreatedAt)
}

// stamp is the fixed time that orders units of one bucket, newest first: live by the latest real turn
// (a directory moves up when a turn ends, never for a reconnect), sleeping by creation, all gone by
// when it last had activity (its going), closed by closing.
func (u Unit) stamp(bucket int) int64 {
	switch bucket {
	case bucketLive:
		return u.lastTurn()
	case bucketGone:
		return u.Active()
	case bucketClosed:
		return u.Closed.ClosedAt
	}
	return u.created()
}

// DirGroup is a project directory and what runs in it.
type DirGroup struct {
	Dir    string
	Units  []Unit
	Bucket int // where the directory sorts: its best unit's bucket (bucketLive...)
}

// GroupByDir groups the listed teams, closed teams and solos by directory. roots are every
// team's root (open and closed), where a solo's cwd is placed. Directories are ordered by the best
// bucket of their units (unit.bucket at now), then newest stamp of the units in that bucket, then
// path; units inside a directory are open ones oldest first, closed ones last.
func GroupByDir(teams []core.TeamState, closed []core.ClosedTeam, solos []core.SoloState, roots []string, now time.Time) []DirGroup {
	byDir := map[string]*DirGroup{}
	var order []*DirGroup
	add := func(dir string, u Unit) {
		g := byDir[dir]
		if g == nil {
			g = &DirGroup{Dir: dir}
			byDir[dir] = g
			order = append(order, g)
		}
		g.Units = append(g.Units, u)
	}
	// A taskforce is listed in its caller's directory, whatever its own root.
	dirOf := map[string]string{} // a solo's or a member's id -> the directory of its unit
	for i := range teams {
		for _, m := range teams[i].Members {
			dirOf[m.ID] = teams[i].Root
		}
	}
	for i := range closed {
		for _, m := range closed[i].Members {
			dirOf[m.ID] = closed[i].Root
		}
	}
	for i := range solos {
		dirOf[solos[i].ID] = deepestRoot(solos[i].Cwd, roots)
	}
	home := func(t core.TeamState) string {
		if d, ok := dirOf[t.ParentID]; ok && t.ParentID != "" {
			return d
		}
		return t.Root
	}
	for i := range teams {
		add(home(teams[i]), Unit{Team: &teams[i]})
	}
	for i := range closed {
		add(home(closed[i].TeamState), Unit{Team: &closed[i].TeamState, Closed: &closed[i]})
	}
	for i := range solos {
		add(dirOf[solos[i].ID], Unit{Solo: &solos[i]})
	}
	for _, g := range order {
		slices.SortStableFunc(g.Units, func(a, b Unit) int {
			closed := func(u Unit) int {
				if u.Closed != nil {
					return 1
				}
				return 0
			}
			return cmp.Or(cmp.Compare(closed(a), closed(b)), cmp.Compare(a.created(), b.created()))
		})
		g.Units = nestTaskforces(g.Units)
	}
	rank := func(g *DirGroup) (bucket int, stamp int64) { // the best bucket of its units, the newest stamp in it
		bucket = bucketClosed
		for _, u := range g.Units {
			bucket = min(bucket, u.bucket(now))
		}
		for _, u := range g.Units {
			if u.bucket(now) == bucket {
				stamp = max(stamp, u.stamp(bucket))
			}
		}
		return bucket, stamp
	}
	slices.SortStableFunc(order, func(a, b *DirGroup) int {
		ab, as := rank(a)
		bb, bs := rank(b)
		return cmp.Or(cmp.Compare(ab, bb), cmp.Compare(bs, as), strings.Compare(a.Dir, b.Dir))
	})
	out := make([]DirGroup, len(order))
	for i, g := range order {
		out[i] = *g
		out[i].Bucket, _ = rank(g)
	}
	return out
}

// nestTaskforces puts each taskforce right after the unit of its caller (a solo, or the team the
// calling member belongs to), after that unit's own taskforces, in the order of us, and sets Under
// and Depth. A taskforce whose caller is not among us stays where it is.
func nestTaskforces(us []Unit) []Unit {
	ownerOf := map[string]int{} // a solo's or a member's id -> the index of its unit
	for i, u := range us {
		if u.Solo != nil {
			ownerOf[u.Solo.ID] = i
			continue
		}
		for _, m := range u.Team.Members {
			ownerOf[m.ID] = i
		}
	}
	parentOf := func(u Unit) string {
		if u.Team != nil {
			return u.Team.ParentID
		}
		return ""
	}
	kids := map[int][]int{}
	var roots []int
	for i, u := range us {
		if o, ok := ownerOf[parentOf(u)]; ok && o != i && parentOf(u) != "" {
			kids[o] = append(kids[o], i)
		} else {
			roots = append(roots, i)
		}
	}
	out := make([]Unit, 0, len(us))
	seen := map[int]bool{}
	var walk func(i int, under string, depth int)
	walk = func(i int, under string, depth int) {
		if seen[i] { // a caller loop cannot be made; do not trust that here
			return
		}
		seen[i] = true
		u := us[i]
		u.Under, u.Depth = under, depth
		out = append(out, u)
		for _, k := range kids[i] {
			walk(k, parentOf(us[k]), depth+1)
		}
	}
	for _, i := range roots {
		walk(i, "", 0)
	}
	for i := range us { // units only reachable through a loop
		walk(i, "", 0)
	}
	return out
}

// FoldGone splits a team's members into those listed and the gone ones folded into one line: a
// gone member with no live member below it (by reports_to) folds; one above a live member stays,
// so the tree is kept. Both keep the order of ms; memberTree of kept never orphans a member.
func FoldGone(ms []core.MemberState) (kept, folded []core.MemberState) {
	byID := map[string]core.MemberState{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	above := map[string]bool{} // live members and everyone they report to
	for _, m := range ms {
		if m.State == "gone" {
			continue
		}
		for id := m.ID; id != "" && !above[id]; id = byID[id].ReportsTo {
			above[id] = true
			if _, ok := byID[id]; !ok {
				break
			}
		}
	}
	for _, m := range ms {
		if m.State == "gone" && !above[m.ID] {
			folded = append(folded, m)
		} else {
			kept = append(kept, m)
		}
	}
	return kept, folded
}

// deepestRoot is the longest root that is cwd or holds it; none: cwd itself.
func deepestRoot(cwd string, roots []string) string {
	best := ""
	c := filepath.ToSlash(cwd) // compared with "/" whatever the OS separator is
	for _, r := range roots {
		r = filepath.Clean(r)
		s := filepath.ToSlash(r)
		if (c == s || strings.HasPrefix(c, strings.TrimSuffix(s, "/")+"/")) && len(r) > len(best) {
			best = r
		}
	}
	if best == "" {
		return cwd
	}
	return best
}

// Dead is an open team whose members are all gone. All lists it as one line, like a closed team;
// its own tab lists its members.
func Dead(t core.TeamState) bool {
	for _, mem := range t.Members {
		if mem.State != "gone" {
			return false
		}
	}
	return len(t.Members) > 0
}
