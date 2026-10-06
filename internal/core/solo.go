package core

import "fmt"

// A solo is a participant with no team and no role: a new harness session. It is its own gate: it
// sees the teams (who), sends to their gates, gets replies, and founds a team. Its role is the
// implicit "" role of a manifest with these tools and the limits of the built-in p2p template
// (WithSoloTemplate).

// soloTools are a solo's tools; of agent it may only found, reopen, close a taskforce and spawn template=.
var soloTools = []string{"send", "inbox", "who", "agent"}

// WithSoloTemplate takes a solo's limits from manifest text (the server passes the built-in
// p2p template). Without it a solo has no limits.
func WithSoloTemplate(text string) Option {
	return func(e *Engine) {
		if m, err := parseManifest(text); err == nil {
			e.soloLimits = m.Limits
		}
	}
}

func (e *Engine) soloManifest() manifest {
	return manifest{Template: "solo", Roles: map[string]roleSpec{"": {Tools: soloTools}}, Limits: e.soloLimits}
}

// soloCard is the role card of a solo.
func soloCard(p participant, taskforces []string) string {
	return fmt.Sprintf("You are %s, a solo piggery session: you are in no team. ", p.name) +
		"Mail from others arrives as a user message with a header naming the sender and the message's #N." +
		toolTips(p.toolPrefix, soloTools) +
		" Other teams are reached only through their gate: send to a team's name to reach it." +
		" When asked to set up a team, use " + p.toolPrefix + "agent action=found (template default p2p);" +
		" the team is rooted at your directory and you become its gate. When asked to reopen a closed team rooted" +
		" at your directory, use " + p.toolPrefix + "agent action=reopen team=<name>." +
		// The guide is not installed as a harness skill (it would go stale): the model asks for it.
		callTaskforceText(p.toolPrefix, taskforces) +
		" For the rest of piggery (templates, workers, shell commands), run `piggery skills`.\n"
}
