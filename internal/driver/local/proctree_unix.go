//go:build unix

package local

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/sting8k/piggery/internal/core"
)

// processTable is every process (`ps -A`, on macOS and Linux); nil when it cannot be read.
func processTable() []proc {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=,lstart=").Output()
	if err != nil {
		return nil
	}
	var rows []proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		rows = append(rows, proc{pid, ppid, pgid, strings.Join(f[3:], " ")})
	}
	return rows
}

// signalTree sends sig to the worker's group pgid (when group: the leader has not been reaped, or
// was alive when this stop began) and to each member of tree.
func (d *Driver) signalTree(pgid int, group bool, tree []proc, sig syscall.Signal) {
	if group {
		d.kill(-pgid, sig)
	}
	own := syscall.Getpgrp()
	leader := map[int]bool{}
	for _, p := range tree {
		leader[p.pid] = true
	}
	sent := map[int]bool{pgid: group}
	for _, p := range tree {
		if p.pgid > 1 && p.pgid != own && !sent[p.pgid] && leader[p.pgid] {
			sent[p.pgid] = true
			d.kill(-p.pgid, sig)
		}
	}
	for _, p := range tree {
		if !sent[p.pgid] && p.pid > 1 {
			d.kill(p.pid, sig)
		}
	}
}

// hasTerm: the OS can ask a process to end (SIGTERM) before it is killed, so stop and kill give it
// a grace period.
const hasTerm = true

// newKill is the kill the runner uses: kill(2); a negative pid is a process group.
func newKill() func(pid int, sig syscall.Signal) error { return syscall.Kill }

// isolate puts cmd in a process group of its own, so the worker's tree is signalled as one.
func isolate(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// The worker's tree is its process group: nothing to attach, begin or end.
func attachTree(pid int)    {}
func stopTreeBegin(pid int) {}
func stopTreeEnd(pid int)   {}
func treeExited(pid int)    {}

// killedExit is the exit a worker is recorded with: on unix the wait status already says it died of
// SIGKILL, so it is as exitOf read it.
func killedExit(e core.Exit, _ bool) core.Exit { return e }
