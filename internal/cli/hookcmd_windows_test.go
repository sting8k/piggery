//go:build windows

package cli

import "testing"

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"2.1.283": true, "2.1.139": true, "2.1.138": false, "2.0.999": false, "3.0.0": true, "2.1": false} {
		if got := versionAtLeast(v, claudeExecFormSince); got != want {
			t.Errorf("versionAtLeast(%s) = %v, want %v", v, got, want)
		}
	}
}

// Codex's hook command has no quote in it unless piggery's path needs one: a Codex that escapes the
// quotes of a command (openai/codex #32402) runs none that has any.
func TestCodexHookCommandQuotesOnlyAPathThatNeedsIt(t *testing.T) {
	for self, want := range map[string]string{
		`C:\Users\me\.local\bin\piggery.exe`:     `C:\Users\me\.local\bin\piggery.exe hook codex Stop`,
		`C:\Users\Jo Doe\.local\bin\piggery.exe`: `"C:\Users\Jo Doe\.local\bin\piggery.exe" hook codex Stop`,
		`C:\Tools (x86)\piggery.exe`:             `"C:\Tools (x86)\piggery.exe" hook codex Stop`,
	} {
		if got := codexHookCommand(self, "Stop"); got != want {
			t.Errorf("codexHookCommand(%s) = %s, want %s", self, got, want)
		}
	}
}
