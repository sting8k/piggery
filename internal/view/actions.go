package view

import (
	"slices"

	"github.com/sting8k/piggery/internal/core"
)

// Pickable reports whether a model can be picked for mem of team teamID: a headless worker that is
// live, in a team that is open (not among closed).
func Pickable(mem core.MemberState, teamID string, closed []core.ClosedTeam) bool {
	return mem.Headless && mem.State != "gone" && !slices.ContainsFunc(closed, func(c core.ClosedTeam) bool { return c.ID == teamID })
}

// ReloadNote is why the participant called name cannot be reloaded from here, "" when it can: a
// headless worker of an open team, running or stopped (a stopped one starts with the new config).
func ReloadNote(name string, headless, closedTeam bool) string {
	switch {
	case !headless:
		return name + " is not a headless worker: restart it in its own window"
	case closedTeam:
		return name + "'s team is closed"
	}
	return ""
}

// KillNote is why the participant called name cannot be killed from here, "" when it can: only a
// headless worker that is not gone is killed this way; a session is stopped in its own window.
func KillNote(name string, headless bool, state string) string {
	switch {
	case !headless:
		return name + " is not a headless worker: stop it in its own window (Esc)"
	case state == "gone":
		return name + " is already stopped"
	}
	return ""
}

// Logged reports whether mem's harness declares usage: a driver log with context, turns and a
// tail. The capability is read; a member that declared none (spawned before capabilities) is
// judged as before, by being headless.
func Logged(mem core.MemberState) bool {
	if mem.Capabilities == nil {
		return mem.Headless
	}
	return slices.Contains(mem.Capabilities, core.CapUsage)
}

// MemberTailable reports whether mem has a log to tail: a driver log (capability usage), or a
// transcript readable says piggery can read.
func MemberTailable(mem core.MemberState, readable func(*core.Transcript) bool) bool {
	return Logged(mem) || readable != nil && readable(mem.Transcript)
}

// SoloTailable reports whether a solo has a transcript piggery can read.
func SoloTailable(s core.SoloState, readable func(*core.Transcript) bool) bool {
	return readable != nil && readable(s.Transcript)
}
