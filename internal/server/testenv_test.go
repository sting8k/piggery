package server_test

import (
	"os"
	"runtime"
)

// shortTmp is where a test makes the short dir of a daemon: unix socket paths are length-limited
// (104 bytes on macOS), so /tmp. Windows has no /tmp, and its pipe name is a hash.
func shortTmp() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "/tmp"
}

// permIs: a file's permission bits are what a test set; Windows keeps no such bits.
func permIs(got, want os.FileMode) bool { return runtime.GOOS == "windows" || got == want }
