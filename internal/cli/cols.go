package cli

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
)

// The columns top and ps show after the name come from config.yaml's display.columns
// (server.DisplayColumns), in its order; one table for every row, "-" where a row has none.
var rightCols = map[string]bool{"ctx": true, "turns": true, "unacked": true, "age": true, "since": true}

// shown is name, then the configured columns of kinds, in the configured order.
func shown(cols, kinds []string) []string {
	out := []string{"name"}
	for _, c := range cols {
		if slices.Contains(kinds, c) {
			out = append(out, c)
		}
	}
	return out
}

// namedFit is a fitStep by column name; a step on a column not shown is skipped.
type namedFit struct {
	name string
	min  int
}

// layout is what fitCols and row take for the columns names: headers, right-aligned columns and
// the fit steps.
func layout(names []string, fit []namedFit) (head []string, right map[int]bool, steps []fitStep) {
	right = map[int]bool{}
	for i, n := range names {
		head = append(head, strings.ToUpper(n))
		right[i] = rightCols[n]
	}
	for _, f := range fit {
		if i := slices.Index(names, f.name); i >= 0 {
			steps = append(steps, fitStep{i, f.min})
		}
	}
	return head, right, steps
}

// shortPaths shortens each path for a cwd column: ~ for home; a path of at most two folders
// under ~ as is; else its last two folders after "…/", more for paths whose tails would read the
// same though the paths differ.
func shortPaths(paths []string) []string {
	parts := make([][]string, len(paths))
	n := make([]int, len(paths))
	for i, p := range paths {
		parts[i] = strings.Split(home(p), "/")
		n[i] = 2
	}
	short := func(i int) string {
		ps := parts[i]
		if len(ps) <= n[i]+1 { // ~ or / and at most n folders: nothing to cut
			return strings.Join(ps, "/")
		}
		return "…/" + strings.Join(ps[len(ps)-n[i]:], "/")
	}
	for grown := true; grown; { // every path of a clash grows together, one folder a round
		grown = false
		clash := make([]bool, len(paths))
		for i := range paths {
			for j := range paths {
				if i != j && home(paths[i]) != home(paths[j]) && short(i) == short(j) {
					clash[i] = true
				}
			}
		}
		for i, c := range clash {
			if c {
				n[i]++
				grown = true
			}
		}
	}
	out := make([]string, len(paths))
	for i := range paths {
		out[i] = short(i)
	}
	return out
}

// truncLeft cuts s to n display cells from the left, with … first when cut: the end of a path
// (its last folder) is what stays.
func truncLeft(s string, n int) string {
	if n <= 0 || lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width("…"+string(r)) > n {
		r = r[1:]
	}
	return "…" + string(r)
}
