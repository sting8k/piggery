//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const exeSuffix = ""

// stopDaemon ends the daemon of bin that autostarted under home, and waits for its socket to go.
func stopDaemon(bin, home string) {
	exec.Command("pkill", "-TERM", "-f", "^"+bin+" serve$").Run()
	sock := filepath.Join(home, ".piggery", "piggery.sock")
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); os.IsNotExist(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}
