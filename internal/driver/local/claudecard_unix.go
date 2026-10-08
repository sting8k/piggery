//go:build unix

package local

// claudeCardArgs is how a Claude worker gets its role card: as an argument.
func claudeCardArgs(_, card string) ([]string, error) {
	return []string{"--append-system-prompt", card}, nil
}
