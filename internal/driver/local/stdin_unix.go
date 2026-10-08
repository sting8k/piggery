//go:build unix

package local

import (
	"os"
	"time"
)

// writeWithin writes b to a worker's stdin, giving up once d has passed (os.ErrDeadlineExceeded):
// a pipe takes a write deadline.
func writeWithin(f *os.File, b []byte, d time.Duration) error {
	f.SetWriteDeadline(time.Now().Add(d))
	_, err := f.Write(b)
	return err
}
