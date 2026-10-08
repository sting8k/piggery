//go:build windows

package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sting8k/piggery/internal/driver/local"
)

// The hook commands `piggery setup` writes into Claude Code's plugin and Codex's hooks.json, from
// the harnesses' documentation (not yet measured on Windows):
//   - Codex runs the command string as `cmd.exe /C "<command>"` (openai/codex #38168): a quoted
//     executable path first, then plain words, is the shape that works there. A Codex from before
//     that passed the string as one argument, its quotes escaped as \", which cmd.exe does not read
//     (#32402): no command with a quote in it ran. So the path is quoted only when cmd.exe would
//     not take it as one word, and a path without a space runs on every Codex.
//   - Claude Code runs a command string in Git Bash, or in PowerShell when Git Bash is not
//     installed, so no one quoting is right. A handler with `args` is spawned directly with no shell
//     (its hooks reference, "Exec form"): self and its arguments go in as they are.

// codexHookCommand is the command string Codex runs for piggery's hook of event.
func codexHookCommand(self, event string) string {
	if strings.ContainsAny(self, cmdSpecial) {
		self = `"` + self + `"`
	}
	return self + " hook codex " + event
}

// cmdSpecial are the characters a file name must be quoted for in cmd.exe (its own help, cmd /?).
const cmdSpecial = " \t&()[]{}^=;!'+,`~"

// claudeHookHandler is the handler piggery's hook of event is in Claude's hooks.json.
func claudeHookHandler(self, event string) map[string]any {
	return map[string]any{"type": "command", "command": self, "args": []string{"hook", "claude", event}, "timeout": 10}
}

// claudeHookRunsSelf: a hooks.json handler (its command and args) is piggery's hook of this binary.
func claudeHookRunsSelf(command string, args []string, self string) bool {
	return command == self && len(args) == 3 && slices.Equal(args[:2], []string{"hook", "claude"})
}

// claudeExecFormSince is the first Claude Code that reads `args` in a hook (its changelog, 2.1.139).
// An older one ignores it and runs `command` alone, here the bare piggery.exe, through bash.
const claudeExecFormSince = "2.1.139"

// claudeHooksSupported: the installed Claude Code reads the exec form setup writes. A version that
// cannot be read is let through.
func claudeHooksSupported() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v, err := local.HarnessVersion(ctx, claudeBin)
	if err != nil || versionAtLeast(v, claudeExecFormSince) {
		return nil
	}
	return fmt.Errorf("claude: Claude Code %s ignores the `args` of a hook, so piggery's hooks would run without their arguments; update it to %s or later", v, claudeExecFormSince)
}

// versionAtLeast: dotted version v is min or later.
func versionAtLeast(v, min string) bool {
	num := func(s string) []int {
		var n []int
		for _, p := range strings.Split(s, ".") {
			i, _ := strconv.Atoi(p)
			n = append(n, i)
		}
		return n
	}
	a, b := num(v), num(min)
	for i := range b {
		if i >= len(a) || a[i] != b[i] {
			return i < len(a) && a[i] > b[i]
		}
	}
	return true
}
