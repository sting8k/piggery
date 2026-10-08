//go:build windows

package local

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"github.com/sting8k/piggery/internal/core"
	"golang.org/x/sys/windows"
)

// Windows has no process groups. A worker's tree is a Job Object: TerminateJobObject ends the
// worker and everything it started, including children that appeared after the last process-table
// read (a table read still finds the rest). The job has NO kill-on-close limit: workers outlive
// the daemon, as on unix. Its handle is held from Start until the worker ends (treeExited) or, when a
// stop is running, until that stop is done (stopTreeEnd); after a daemon restart there is no
// handle, and recovery kills by the table read and the pid + start-time check alone. The job is
// not reopened by name: nothing holds a handle once the daemon exits, and a named object is
// expected to lose its name with its last handle (to confirm on Windows).
//
// The job is keyed by the worker's pid, which doubles as its pgid (the runner's "group").

const hasTerm = false // no SIGTERM: Stop's stdin close is the soft step, then the job is terminated

type job struct {
	h        windows.Handle
	stopping bool // a stop or kill is using it: treeExited leaves it to stopTreeEnd
}

var (
	jobsMu sync.Mutex
	jobs   = map[int]*job{}
)

// attachTree puts the worker pid in a new job. A failure leaves it unassigned: signalTree still
// ends it and the tree the table shows, by pid. Children it starts before this call are not in the
// job (os/exec cannot start a process suspended); the table read finds them.
func attachTree(pid int) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		windows.CloseHandle(h)
		return
	}
	defer windows.CloseHandle(p)
	if err := windows.AssignProcessToJobObject(h, p); err != nil {
		windows.CloseHandle(h)
		return
	}
	jobsMu.Lock()
	jobs[pid] = &job{h: h}
	jobsMu.Unlock()
}

// stopTreeBegin and stopTreeEnd bracket a stop or kill of the worker: its job stays open through
// the final terminate even when the leader has exited meanwhile, and is closed after.
func stopTreeBegin(pid int) {
	jobsMu.Lock()
	if j := jobs[pid]; j != nil {
		j.stopping = true
	}
	jobsMu.Unlock()
}

func stopTreeEnd(pid int) { closeJob(pid) }

// treeExited: the worker ended by itself (or its stop is over): close its job handle unless a stop
// still needs it. Whatever it left running keeps running.
func treeExited(pid int) {
	jobsMu.Lock()
	j := jobs[pid]
	stopping := j != nil && j.stopping
	jobsMu.Unlock()
	if !stopping {
		closeJob(pid)
	}
}

func closeJob(pid int) {
	jobsMu.Lock()
	j := jobs[pid]
	delete(jobs, pid)
	jobsMu.Unlock()
	if j != nil {
		windows.CloseHandle(j.h)
	}
}

// processTable is every process, from a toolhelp snapshot; nil when it cannot be read. pgid is the
// pid. start is the creation time ("" for a process that cannot be opened). Windows keeps a dead
// parent's pid in its children, and the pid may be reused by an unrelated process: a parent
// created after its child is not its parent, so that link is dropped.
func processTable() []proc {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var rows []proc
	created := map[int]int64{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := int(e.ProcessID)
		start, ct := procStart(pid)
		created[pid] = ct
		rows = append(rows, proc{pid: pid, ppid: int(e.ParentProcessID), pgid: pid, start: start})
	}
	for i, r := range rows {
		if pc, ok := created[r.ppid]; ok && pc > created[r.pid] && created[r.pid] != 0 {
			rows[i].ppid = 0
		}
	}
	return rows
}

// procStart is pid's creation time, as an identity string and in nanoseconds; "", 0 when it cannot
// be read.
func procStart(pid int) (string, int64) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", 0
	}
	defer windows.CloseHandle(h)
	var ct, et, kt, ut windows.Filetime
	if err := windows.GetProcessTimes(h, &ct, &et, &kt, &ut); err != nil {
		return "", 0
	}
	return strconv.FormatInt(ct.Nanoseconds(), 10), ct.Nanoseconds()
}

// signalTree ends the worker (its job, when group: the leader was alive when the stop began, or its
// job is still open) and every member of tree, by pid. Only SIGKILL means anything here.
func (d *Driver) signalTree(pgid int, group bool, tree []proc, sig syscall.Signal) {
	if sig != syscall.SIGKILL {
		return
	}
	if group {
		d.kill(-pgid, sig)
	}
	for _, p := range tree {
		if p.pid > 4 { // never the System process or Idle
			d.kill(p.pid, sig)
		}
	}
}

// newKill is the kill the runner uses: SIGKILL ends a process (TerminateProcess), or with a
// negative pid the job of that worker, else that worker's process alone; no other signal exists.
func newKill() func(pid int, sig syscall.Signal) error {
	return func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGKILL {
			return nil
		}
		if pid < 0 {
			pid = -pid
			jobsMu.Lock()
			j := jobs[pid]
			jobsMu.Unlock()
			if j != nil && windows.TerminateJobObject(j.h, 1) == nil {
				return nil
			}
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		return windows.TerminateProcess(h, 1)
	}
}

// isolate keeps a worker's console window from appearing; its tree is its job (attachTree).
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// killedExit is the exit a worker is recorded with: Windows has no signal in a wait status (a
// terminated process just has exit code 1), so a worker that was killed and did not exit 0 is
// recorded as unix records it, code -1 and SIGKILL.
func killedExit(e core.Exit, killed bool) core.Exit {
	if killed && e.Signal == "" && e.Code != 0 {
		return core.Exit{Code: -1, Signal: "SIGKILL", At: e.At}
	}
	return e
}
