//go:build unix

package cli

import (
	"strings"

	"github.com/sting8k/piggery/internal/driver/local"
)

// The hook commands `piggery setup` writes into Claude Code's plugin and Codex's hooks.json. Both
// harnesses run a command string in a POSIX shell here, so self is quoted for one.

// codexHookCommand is the command string Codex runs for piggery's hook of event.
func codexHookCommand(self, event string) string {
	return local.ShellQuote(self) + " hook codex " + event
}

// claudeHookHandler is the handler piggery's hook of event is in Claude's hooks.json.
func claudeHookHandler(self, event string) map[string]any {
	return map[string]any{"type": "command", "command": local.ShellQuote(self) + " hook claude " + event, "timeout": 10}
}

// claudeHookRunsSelf: a hooks.json handler (its command and args) is piggery's hook of this binary.
func claudeHookRunsSelf(command string, _ []string, self string) bool {
	return strings.HasPrefix(command, local.ShellQuote(self)+" hook claude ")
}

// claudeHooksSupported: the installed Claude Code runs the hooks setup writes (any version does).
func claudeHooksSupported() error { return nil }
