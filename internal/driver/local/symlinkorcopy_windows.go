//go:build windows

package local

import (
	"io/fs"
	"os"
	"path/filepath"
)

// symlinkOrCopy makes dst a link to src; where creating a symlink needs a privilege the user has
// not (Developer Mode off), dst is a copy of src as it is now (a snapshot taken at spawn).
func symlinkOrCopy(src, dst string) error {
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o600)
	})
}
