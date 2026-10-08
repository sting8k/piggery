//go:build windows

package local

import (
	"os"
	"time"
)

// writeWithin writes b to a worker's stdin, giving up once d has passed (os.ErrDeadlineExceeded).
// A pipe of os.Pipe is a synchronous handle here: it takes no deadline, its buffer is 4 KB, and a
// write to a worker that has stopped reading would block its caller for good. So the write runs on
// a goroutine of its own and only the wait for it is bounded. A write given up on is still in the
// pipe's way until the worker reads again, its stdin is closed (Close cancels it) or it dies: the
// caller must let no other write follow it (worker.inBroken).
func writeWithin(f *os.File, b []byte, d time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, err := f.Write(b)
		done <- err
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		return os.ErrDeadlineExceeded
	}
}
