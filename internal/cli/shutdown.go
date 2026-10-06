package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
)

// shutdownWait bounds how long shutdown waits for the daemon to finish (each worker's stop escalation
// runs inside it).
const shutdownWait = 60 * time.Second

// shutdown asks the daemon to shut down the normal way (as on SIGTERM: stop workers, record their
// exits, close the DB) and returns once it has exited. It never starts a daemon.
func (e *env) shutdown(args []string) error {
	pos, err := parse(e.flags("shutdown"), args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%w: shutdown takes no arguments", errUsage)
	}
	c, err := e.dial(false)
	if daemonDown(err) {
		fmt.Fprintln(e.stdout, "not running")
		return nil
	}
	if err != nil {
		return err
	}
	if err := e.stopDaemon(c); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, "stopped")
	return nil
}

// stopDaemon shuts down the daemon c is connected to (and closes c), and returns once it has
// exited: lock and socket released.
func (e *env) stopDaemon(c *Client) error {
	_, err := c.Call(proto.VerbShutdown, nil)
	c.Close()
	if err != nil {
		return err
	}
	// The socket goes as soon as shutdown starts; the singleton lock only when the daemon has
	// stopped its workers and closed the DB.
	for deadline := time.Now().Add(shutdownWait); ; {
		if free, err := server.LockFree(server.LockPath(e.dir)); err != nil {
			return err
		} else if free {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon still running after %s", shutdownWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if left := server.Leftover(e.dir); left != "" {
		return fmt.Errorf("daemon exited but left %s", left)
	}
	return nil
}

// restart is shutdown (workers stopped, exits recorded), then a daemon of this binary started
// the way any command starts one, once the old one has exited. Not running: it only starts.
func (e *env) restart(args []string) error {
	pos, err := parse(e.flags("restart"), args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%w: restart takes no arguments", errUsage)
	}
	var old proto.PsResult
	var workers []string
	c, err := e.dial(false)
	switch {
	case daemonDown(err):
	case err != nil:
		return err
	default:
		if _, err := c.CallInto(proto.VerbPs, core.StateArgs{}, &old); err != nil {
			c.Close()
			return err
		}
		for _, t := range old.Teams { // the live workers shutdown stops
			for _, m := range t.Members {
				if m.Headless && m.State != "gone" {
					workers = append(workers, m.Name)
				}
			}
		}
		if err := e.stopDaemon(c); err != nil {
			return err
		}
	}
	c, err = e.dial(true)
	if err != nil {
		return err
	}
	defer c.Close()
	var cur proto.PsResult
	if _, err := c.CallInto(proto.VerbPs, core.StateArgs{}, &cur); err != nil {
		return err
	}
	if old.PID == 0 {
		fmt.Fprintf(e.stdout, "was not running; started pid %d\n", cur.PID)
		return nil
	}
	fmt.Fprintf(e.stdout, "restarted: pid %d -> %d", old.PID, cur.PID)
	if len(workers) == 0 {
		fmt.Fprintln(e.stdout, "; no worker was running")
		return nil
	}
	fmt.Fprintf(e.stdout, "; stopped %d worker(s): %s (start one again: piggery resume <name>)\n",
		len(workers), strings.Join(workers, ", "))
	return nil
}
