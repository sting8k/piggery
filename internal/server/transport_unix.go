//go:build unix

package server

import (
	"errors"
	"net"
	"os"
)

// The daemon's address is the unix socket SocketPath(dir) (mode 0600); the peer gate reads the
// client's pid from the socket (peer_linux.go, peer_darwin.go).

// Listen makes the daemon's listener at dir's address, replacing a socket file left behind (the
// singleton lock is held, so nobody listens on it).
func Listen(dir string) (net.Listener, error) {
	sock := SocketPath(dir)
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Dial connects to the daemon at dir's address.
func Dial(dir string) (net.Conn, error) { return net.Dial("unix", SocketPath(dir)) }

// Address is dir's daemon address as people and logs see it.
func Address(dir string) string { return SocketPath(dir) }

// Leftover is something a daemon that exited should have removed and did not: its address while
// it is still there ("" = nothing).
func Leftover(dir string) string {
	if _, err := os.Stat(SocketPath(dir)); err == nil {
		return SocketPath(dir)
	}
	return ""
}

// WritePipeFile is for Windows (the file the Node clients read the pipe name from): nothing on unix.
func WritePipeFile(dir string) error { return nil }
