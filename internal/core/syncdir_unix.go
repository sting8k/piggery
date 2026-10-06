//go:build unix

package core

import "os"

// syncDir makes the entries just written in dir (a rename) durable: fsync of the directory.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
