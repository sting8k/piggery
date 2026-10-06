//go:build unix

package local

import "os"

// symlinkOrCopy makes dst a link to src.
func symlinkOrCopy(src, dst string) error { return os.Symlink(src, dst) }
