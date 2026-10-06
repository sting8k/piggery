//go:build windows

package local

import (
	"os"
	"path/filepath"
)

// claudeCardArgs is how a Claude worker gets its role card: in a file of the run's directory dir
// (--append-system-prompt-file, which every Claude Code piggery runs has, and which puts the text
// where the argument puts it: measured with 2.1.289). CreateProcess takes a command line of at most
// 32,767 characters, and a card that carries the user's own rules (prompts) can be longer.
func claudeCardArgs(dir, card string) ([]string, error) {
	path := filepath.Join(dir, "role-card.md")
	if err := os.WriteFile(path, []byte(card), 0o600); err != nil {
		return nil, err
	}
	return []string{"--append-system-prompt-file", path}, nil
}
