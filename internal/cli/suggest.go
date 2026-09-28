package cli

import (
	"regexp"
	"slices"
	"strings"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
)

// ulid matches a ULID (the DB's ids), which a human never needs to read.
var ulid = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`)

// relabel replaces the ids in human text lines: a participant or team by its name, a message
// by its #seq (asked from the daemon), anything else (a run) cut to its last 6 characters.
func (e *env) relabel(lines []string) []string {
	var ids []string
	for _, l := range lines {
		ids = append(ids, ulid.FindAllString(l, -1)...)
	}
	if len(ids) == 0 {
		return lines
	}
	labels := map[string]string{}
	if c, err := e.dial(false); err == nil {
		c.CallInto(proto.VerbLabels, core.LabelsArgs{IDs: ids}, &labels)
		c.Close()
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ulid.ReplaceAllStringFunc(l, func(id string) string {
			if s, ok := labels[id]; ok {
				return s
			}
			return "…" + id[len(id)-6:]
		})
	}
	return out
}

// A not_found for a name the human typed: `no participant "x"`, `no open team "x"`, `no team "x"`.
var missingName = regexp.MustCompile(`^no (participant|open team|team) "([^"]*)"$`)

// suggestion is "did you mean …" for a worker or team name that does not exist, from the names
// the daemon has now (admin only, no autostart); "" when there is nothing close.
func (e *env) suggestion(ce *core.Error) string {
	m := missingName.FindStringSubmatch(ce.Message)
	if m == nil || ce.Code != core.CodeNotFound || !e.admin {
		return ""
	}
	c, err := e.dial(false)
	if err != nil {
		return ""
	}
	defer c.Close()
	var r proto.PsResult
	if _, err := c.CallInto(proto.VerbPs, core.StateArgs{}, &r); err != nil {
		return ""
	}
	var names []string
	for _, t := range r.Teams {
		if m[1] != "participant" {
			names = append(names, t.Name)
			continue
		}
		for _, p := range t.Members {
			names = append(names, p.Name)
		}
	}
	if m[1] == "participant" {
		for _, s := range r.Solos {
			names = append(names, s.Name)
		}
	}
	if near := closest(m[2], names); len(near) > 0 {
		return "did you mean: " + strings.Join(near, ", ") + "?"
	}
	return ""
}

// closest returns up to 3 names near name: the ones containing it or it containing them, and the
// ones within a small edit distance, nearest first.
func closest(name string, names []string) []string {
	type cand struct {
		name string
		d    int
	}
	var cs []cand
	limit := max(2, len(name)/3)
	for _, n := range names {
		d := distance(strings.ToLower(name), strings.ToLower(n))
		if strings.Contains(n, name) || strings.Contains(name, n) {
			d = min(d, 1)
		}
		if d <= limit && !slices.ContainsFunc(cs, func(c cand) bool { return c.name == n }) {
			cs = append(cs, cand{n, d})
		}
	}
	slices.SortStableFunc(cs, func(a, b cand) int {
		if a.d != b.d {
			return a.d - b.d
		}
		return strings.Compare(a.name, b.name)
	})
	var out []string
	for i := 0; i < len(cs) && i < 3; i++ {
		out = append(out, cs[i].name)
	}
	return out
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			sub := prev[j-1]
			if a[i-1] != b[j-1] {
				sub++
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, sub)
		}
		prev = cur
	}
	return prev[len(b)]
}
