package core_test

import (
	"os"
	"runtime"
)

// permIs: a file's permission bits are what a test set; Windows keeps no such bits.
func permIs(got, want os.FileMode) bool { return runtime.GOOS == "windows" || got == want }
