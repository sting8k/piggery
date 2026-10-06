//go:build windows

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// The daemon's address on Windows is a named pipe, one per user and data dir:
//
//	\\.\pipe\piggery-<16 hex of sha256(user SID, "\n", lowercased absolute dir)>
//
// The dir is in the name so a private daemon (another HOME) has its own pipe, as it has its own
// piggery.sock on unix; the SID keeps two users of one machine apart. The pipe is created with a
// protected DACL that gives the current user alone full access, and as the first instance of its
// name: if something else holds the name, the daemon does not start. The peer gate reads the
// client's pid from the pipe (peer_windows.go). The Node clients cannot recompute the name, so the
// daemon and `piggery setup` write it, one line, to PipeFilePath(dir).

// PipeFilePath is the file that holds dir's pipe name for the Node clients.
func PipeFilePath(dir string) string { return filepath.Join(dir, "piggery.pipe") }

// PipePath is dir's pipe name; "" when the current user cannot be read.
func PipePath(dir string) string {
	sid, err := userSID()
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	sum := sha256.Sum256([]byte(sid + "\n" + strings.ToLower(abs)))
	return `\\.\pipe\piggery-` + hex.EncodeToString(sum[:8])
}

func userSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read the current user: %w", err)
	}
	return u.User.Sid.String(), nil
}

// Listen makes the daemon's pipe at dir's address, for the current user only, and publishes its
// name in the pipe file.
func Listen(dir string) (net.Listener, error) {
	sid, err := userSID()
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(PipePath(dir), &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + sid + ")",
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", PipePath(dir), err)
	}
	if err := WritePipeFile(dir); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Dial connects to the daemon at dir's address; a pipe whose instances are all busy is waited for
// a short while.
func Dial(dir string) (net.Conn, error) {
	path := PipePath(dir)
	if path == "" {
		return nil, fmt.Errorf("no pipe name: the current user cannot be read")
	}
	timeout := 2 * time.Second
	return winio.DialPipe(path, &timeout)
}

// Address is dir's daemon address as people and logs see it.
func Address(dir string) string { return PipePath(dir) }

// Leftover: a pipe goes away with its daemon, there is nothing to leave behind.
func Leftover(dir string) string { return "" }

// WritePipeFile writes dir's pipe name, one line, to PipeFilePath(dir) (the value depends only on
// the user and the dir, so it never goes stale).
func WritePipeFile(dir string) error {
	path := PipePath(dir)
	if path == "" {
		return fmt.Errorf("no pipe name: the current user cannot be read")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(PipeFilePath(dir), []byte(path+"\n"), 0o600)
}
