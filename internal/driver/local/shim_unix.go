//go:build unix

package local

import "os/exec"

// prepareCommand: nothing to do, a command is exec'd as it is.
func prepareCommand(*exec.Cmd) error { return nil }
