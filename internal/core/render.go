package core

import (
	"fmt"
	"strings"
	"time"
)

// Model-facing text shared by every adapter: mail, who and a send result, as the pi extension
// renders them (extensions/pi/render.mjs), with the harness's tool prefix. No ULIDs: messages are
// #N, people are names.

// RenderMail renders msgs for the model. heading replaces the default "N new messages" line
// (a read-only view is not new mail).
func RenderMail(msgs []Delivered, heading, toolPrefix string, now time.Time) string {
	if heading == "" {
		heading = fmt.Sprintf("%d new message", len(msgs))
		if len(msgs) != 1 {
			heading += "s"
		}
	}
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		a := fmt.Sprintf(`id="#%d" from="%s" sender="%s"`, m.Seq, m.FromLabel, m.FromName)
		if m.Kind != "" {
			a += fmt.Sprintf(` kind="%s"`, m.Kind)
		}
		if m.ReplyTo != "" {
			a += fmt.Sprintf(` reply_to="#%d"`, m.ReplyToSeq)
		}
		if m.CcOf != "" { // a routing cc copy
			a += fmt.Sprintf(` cc_of="#%d" cc_of_mail_to="%s"`, m.CcOfSeq, m.CcTo)
		}
		sent := time.UnixMilli(m.CreatedAt)
		a += fmt.Sprintf(` at="%s"`, sentAt(sent, now))
		if d := now.Sub(sent); d > time.Minute { // held, or shown again
			a += fmt.Sprintf(` age="%s"`, shortAge(d))
		}
		if m.Redelivered { // given before and not acked: whatever its age
			a += ` redelivered="true"`
		}
		parts[i] = fmt.Sprintf("<message %s>\n%s\n</message>", a, m.Body)
	}
	return fmt.Sprintf("[piggery] %s:\n\n%s\n\nTo reply, use the %ssend tool with to=<sender> and reply_to=<id> (the #N).",
		heading, strings.Join(parts, "\n\n"), toolPrefix)
}

// sentAt is a send time in local time: 14:02:11 today, 2026-09-26 23:58 another day.
func sentAt(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04:05")
	}
	return t.Format("2006-01-02 15:04")
}

// shortAge is a duration, short: 4m, 2h5m, 3d2h.
func shortAge(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m < 24*60:
		if m%60 == 0 {
			return fmt.Sprintf("%dh", m/60)
		}
		return fmt.Sprintf("%dh%dm", m/60, m%60)
	}
	h := m / 60
	if h%24 == 0 {
		return fmt.Sprintf("%dd", h/24)
	}
	return fmt.Sprintf("%dd%dh", h/24, h%24)
}

// SentText is the model-facing result of a send (the send tool and declarative tools alike).
func SentText(r SendResult) string {
	ref := fmt.Sprintf("#%d", r.Seq)
	switch {
	case r.Duplicate:
		return "duplicate of " + ref
	case r.Held:
		return fmt.Sprintf("stored %s but HELD by %s: not delivered until an admin releases it", ref, r.RuleID)
	}
	return "sent " + ref
}

// RenderWho renders who for the model: the caller's team in full, then one line per other team and
// per solo session. An id is shown only to tell apart two equal names.
func RenderWho(ps []Presence, selfID string) string {
	seen := map[string]int{}
	for _, p := range ps {
		if p.Kind != WhoTeam {
			seen[p.Name]++
		}
	}
	id := func(p Presence) string {
		if seen[p.Name] > 1 {
			return " id=" + p.ID
		}
		return ""
	}
	flag := func(on bool, s string) string {
		if on {
			return s
		}
		return ""
	}
	var members, teams, solos []Presence
	for _, p := range ps {
		switch p.Kind {
		case WhoMember:
			members = append(members, p)
		case WhoTeam:
			teams = append(teams, p)
		case WhoSolo:
			solos = append(solos, p)
		}
	}
	var out []string
	if len(members) > 0 {
		out = append(out, fmt.Sprintf("Your team %s:", members[0].Team))
		for _, p := range members {
			out = append(out, fmt.Sprintf("  %s (%s) %s%s%s%s", p.Name, p.Role, p.State,
				flag(p.Gate, " [gate]"), flag(p.ID == selfID, " (you)"), id(p)))
		}
	}
	if len(teams) > 0 {
		out = append(out, "Other teams (write to the team name; it reaches the gate):")
		for _, t := range teams {
			gate := t.GateName
			if gate == "" {
				gate = "none live"
			}
			out = append(out, fmt.Sprintf("  %s (root %s) gate %s", t.Name, t.Cwd, gate))
		}
	}
	if len(solos) > 0 {
		out = append(out, "Solo sessions (each is its own gate):")
		for _, p := range solos {
			out = append(out, fmt.Sprintf("  %s (cwd %s) %s%s%s%s", p.Name, p.Cwd, p.State,
				flag(p.Admittable, " [admittable]"), flag(p.ID == selfID, " (you)"), id(p)))
		}
	}
	if len(out) == 0 {
		return "Nobody else is on piggery."
	}
	return strings.Join(out, "\n")
}
