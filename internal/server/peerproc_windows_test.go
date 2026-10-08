//go:build windows

package server

import (
	"os"
	"os/exec"
)

// liveProcess is a process the test is not a descendant of, alive for a while: this test binary in
// the worker mode of TestServerWorker that sleeps (Windows has no sleep command).
func liveProcess() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestServerWorker$")
	cmd.Env = append(os.Environ(), "PGTEST_WORKER=sleep")
	return cmd
}
