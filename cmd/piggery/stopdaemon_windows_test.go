//go:build windows

package main

import (
	"os"
	"os/exec"
)

const exeSuffix = ".exe"

// stopDaemon ends the daemon of bin that autostarted under home: `piggery shutdown` returns once
// the daemon is gone (Windows has no pkill).
func stopDaemon(bin, home string) {
	cmd := exec.Command(bin, "--admin", "shutdown")
	cmd.Env = append(os.Environ(), homeEnv(home)...)
	cmd.Run()
}
