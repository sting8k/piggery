package server

import (
	"fmt"
	"os"
	"testing"

	"github.com/sting8k/piggery/internal/driver/local"
)

// Auth by host trusts the peer's process tree, not the host string: a caller that is not the host
// process or its descendant is refused even with the right pid and start time.
func TestHostNeedsPeerInItsTree(t *testing.T) {
	self := os.Getpid()
	host := func(pid int) string { return fmt.Sprintf("claude:%d:%d", pid, local.ProcessStartTime(pid)) }

	other := liveProcess() // a live process the test is not a descendant of
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer other.Process.Kill()
	if hostMatches(self, host(other.Process.Pid), hostDepth) {
		t.Fatal("a process outside the host's tree was accepted")
	}
	if !hostMatches(self, host(os.Getppid()), hostDepth) {
		t.Fatal("a child of the host was refused")
	}
	if hostMatches(self, fmt.Sprintf("claude:%d:%d", os.Getppid(), local.ProcessStartTime(os.Getppid())+1000), hostDepth) {
		t.Fatal("a host with another start time (a reused pid) was accepted")
	}

	// The peer pid read from the daemon's listener is the connecting process.
	dir, err := os.MkdirTemp("", "pg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := Dial(dir); err == nil {
			defer c.Close()
			c.Read(make([]byte, 1))
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if pid, err := peerPID(c); err != nil || pid != self {
		t.Fatalf("peer pid = %d, %v; want %d", pid, err, self)
	}
}
