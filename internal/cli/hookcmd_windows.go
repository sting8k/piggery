//go:build windows

package cli

import "slices"

// The hook commands `piggery setup` writes into Claude Code's plugin and Codex's hooks.json, from
// the harnesses' documentation (not yet measured on Windows):
//   - Codex runs the command string as `cmd.exe /C "<command>"` (openai/codex #32402, #38168): a
//     quoted executable path first, then plain words, is the shape that works there.
//   - Claude Code runs a command string in Git Bash, or in PowerShell when Git Bash is not
//     installed, so no one quoting is right. A handler with `args` is spawned directly with no shell
//     (its hooks reference, "Exec form"): self and its arguments go in as they are.

// codexHookCommand is the command string Codex runs for piggery's hook of event.
func codexHookCommand(self, event string) string { return `"` + self + `" hook codex ` + event }

// claudeHookHandler is the handler piggery's hook of event is in Claude's hooks.json.
func claudeHookHandler(self, event string) map[string]any {
	return map[string]any{"type": "command", "command": self, "args": []string{"hook", "claude", event}, "timeout": 10}
}

// claudeHookRunsSelf: a hooks.json handler (its command and args) is piggery's hook of this binary.
func claudeHookRunsSelf(command string, args []string, self string) bool {
	return command == self && len(args) == 3 && slices.Equal(args[:2], []string{"hook", "claude"})
}
