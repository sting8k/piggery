//go:build unix

package server

import "os/exec"

// liveProcess is a process the test is not a descendant of, alive for a while.
func liveProcess() *exec.Cmd { return exec.Command("sleep", "30") }
