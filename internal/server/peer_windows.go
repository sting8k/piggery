//go:build windows

package server

import (
	"errors"
	"net"

	"golang.org/x/sys/windows"
)

// peerPID is the pid of the process at the other end of a named pipe (GetNamedPipeClientProcessId
// on the server's handle of the pipe).
func peerPID(nc net.Conn) (int, error) {
	f, ok := nc.(interface{ Fd() uintptr })
	if !ok {
		return 0, errors.New("not a named pipe")
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return 0, err
	}
	return int(pid), nil
}
