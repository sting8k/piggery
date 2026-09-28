package local

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

// Recovery. A recorded worker is identified by pid + OS start time
// (same source and resolution as Start: `ps lstart`, 1s) + program name. The full argv is not
// comparable: pi rewrites process.title, so the OS shows `node …/bin/pi <args>` right after exec
// and only `pi` a moment later. The program name is the basename of the first command token, or
// of the second when an interpreter runs a script (node <script>).

// Inspect reports whether p's process is dead, still ours, or a reused pid.
func (d *Driver) Inspect(ctx context.Context, p core.Proc) (core.ProcState, error) {
	if p.PID <= 0 {
		return core.ProcDead, nil
	}
	if p.StartTime == 0 || len(p.Cmdline) == 0 {
		return "", errors.New("recorded process has no start time or cmdline: cannot verify it")
	}
	start, command, alive, err := psInfo(ctx, p.PID)
	if err != nil {
		return "", err
	}
	if !alive {
		return core.ProcDead, nil
	}
	if start != p.StartTime || !sameProgram(command, p.Cmdline[0]) {
		return core.ProcReused, nil
	}
	return core.ProcOurs, nil
}

// psInfo reads the OS start time (unix ms) and command of pid. alive=false when ps reports no
// such process; err when the process table could not be read.
func psInfo(ctx context.Context, pid int) (start int64, command string, alive bool, err error) {
	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "lstart=,command=", "-p", strconv.Itoa(pid)).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && len(strings.TrimSpace(string(out))) == 0 {
		return 0, "", false, nil // ps: no such process
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("read process table: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) < 6 {
		return 0, "", false, fmt.Errorf("read process table: unexpected ps output %q", out)
	}
	t, err := time.ParseInLocation(lstartLayout, strings.Join(f[:5], " "), time.Local)
	if err != nil {
		return 0, "", false, fmt.Errorf("read process table: %w", err)
	}
	return t.UnixMilli(), strings.Join(f[5:], " "), true, nil
}

func sameProgram(command, recorded string) bool {
	name := filepath.Base(recorded)
	tok := strings.Fields(command)
	return len(tok) > 0 && (filepath.Base(tok[0]) == name || (len(tok) > 1 && filepath.Base(tok[1]) == name))
}

// KillVerified signals p's process group (TERM, then KILL) only while Inspect says it is still
// ours, re-checking before each signal. A dead or reused pid is never signalled.
func (d *Driver) KillVerified(ctx context.Context, p core.Proc) (core.Exit, error) {
	pgid := p.PGID
	if pgid <= 0 {
		pgid = p.PID
	}
	sent := ""
	for _, step := range []struct {
		sig  syscall.Signal
		wait time.Duration
	}{{syscall.SIGTERM, d.opts.TermWait}, {syscall.SIGKILL, 5 * time.Second}} {
		st, err := d.Inspect(ctx, p)
		if err != nil {
			return core.Exit{}, err
		}
		if st != core.ProcOurs {
			return core.Exit{Code: -1, Signal: sent, At: time.Now().UnixMilli()}, nil
		}
		d.kill(-pgid, step.sig)
		sent = signalName(step.sig)
		// Not our child after a daemon restart: no wait status, so poll until it is gone.
		for deadline := time.Now().Add(step.wait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if st, err := d.Inspect(ctx, p); err == nil && st != core.ProcOurs {
				return core.Exit{Code: -1, Signal: sent, At: time.Now().UnixMilli()}, nil
			}
		}
	}
	return core.Exit{}, fmt.Errorf("process %d still alive after SIGKILL", p.PID)
}

// StopAll stops every live worker in parallel with Stop's escalation. It returns once each
// exit has been reported through OnExit, so the caller may close the DB afterwards.
func (d *Driver) StopAll(ctx context.Context) {
	d.mu.Lock()
	var live []string
	for id, w := range d.procs {
		if !w.exited() {
			live = append(live, id)
		}
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Stop(ctx, id)
		}()
	}
	wg.Wait()
}
