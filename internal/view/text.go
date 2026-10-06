// Package view decides what top, ps and the Paseo plugin show of a ps snapshot: which rows, in what
// order, with what words, and what can be done to a row. It knows core types and the standard
// library only: no colours, no widths, no keys. The callers draw (decision D-q744).
package view

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Ago is the time since ms in one unit: 12s, 4m, 3h, 2d; "-" when unknown.
func Ago(ms int64, now time.Time) string {
	if ms == 0 {
		return "-"
	}
	d := max(now.Sub(time.UnixMilli(ms)), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// SinceMs is when a member or a solo got into its state, as a person reads it. An idle one counts
// from its last real turn (last_turn_end), not from its state change: a daemon restart sets a session
// gone and its reconnect sets it idle again, and neither is a turn. One that never ran a turn, and
// every other state, count from state_since.
func SinceMs(state string, stateSince, lastTurnEnd int64) int64 {
	if state == "idle" {
		return cmp.Or(lastTurnEnd, stateSince)
	}
	return stateSince
}

// Tokens is a token count for display: 857, 15.5k, 1.2M.
func Tokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}

// Plural is "1 team", "2 teams".
func Plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

// OrDash is s, or "-" when it is empty.
func OrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Home shortens a path under the home directory to ~/…
func Home(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if rel, err := filepath.Rel(h, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}

// HarnessLabel is what runs a participant: pi, claude, codex; `pi·worker` when piggery runs it
// headless (a worker it spawned); "-" unknown.
func HarnessLabel(harness string, headless bool) string {
	switch {
	case harness == "":
		return "-"
	case headless:
		return harness + "·worker"
	}
	return harness
}

// ModelID is a model without its provider (what comes before the first "/"; the id itself may
// hold "/": CPAv2/devin/swe-2 is devin/swe-2), for the model column; "-" unknown.
func ModelID(model string) string {
	if model == "" {
		return "-"
	}
	if _, id, ok := strings.Cut(model, "/"); ok {
		return id
	}
	return model
}

// ModelLabel is a model with its thinking level: "HP/x · high", "HP/x", "thinking high", "-".
func ModelLabel(model, thinking string) string {
	switch {
	case model != "" && thinking != "":
		return model + " · " + thinking
	case thinking != "":
		return "thinking " + thinking
	case model != "":
		return model
	}
	return "-"
}

// DirLabel is a directory line's text: a path, so it ends in "/".
func DirLabel(p string) string {
	return strings.TrimSuffix(p, "/") + "/"
}

// RelCwd is a row's cwd in its directory's group: "" when it is the directory, ./sub under it,
// else the path (~ form).
func RelCwd(dir, cwd string) string {
	if cwd == "" || cwd == dir {
		return ""
	}
	if rel, err := filepath.Rel(dir, cwd); err == nil {
		if rel = filepath.ToSlash(rel); rel != ".." && !strings.HasPrefix(rel, "../") {
			return "./" + rel
		}
	}
	return Home(cwd)
}

// ShortPaths shortens each path for a cwd column: ~ for home; a path of at most two folders
// under ~ as is; else its last two folders after "…/", more for paths whose tails would read the
// same though the paths differ.
func ShortPaths(paths []string) []string {
	parts := make([][]string, len(paths))
	n := make([]int, len(paths))
	full := make([]string, len(paths)) // the path as shown; "/" and "\\" spell one path
	for i, p := range paths {
		full[i] = filepath.ToSlash(Home(p))
		parts[i] = strings.Split(full[i], "/")
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
				if i != j && full[i] != full[j] && short(i) == short(j) {
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

// EventTime is an event's local time: 15:04:05 today, 01-02 15:04 on an earlier day.
func EventTime(ts int64, now time.Time) string {
	t, now := time.UnixMilli(ts).Local(), now.Local()
	if y, mo, d := t.Date(); y == now.Year() && mo == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("01-02 15:04")
}
