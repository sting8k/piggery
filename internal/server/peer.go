package server

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sting8k/piggery/internal/driver/local"
)

// hostDepth bounds the walk from the peer up to the host process. Measured (Claude 2.1.283 live):
// every `piggery hook claude <event>` and `piggery mcp` is claude's direct child; one level more
// allows a shell wrapper.
const hostDepth = 2

// workerDepth bounds the walk from the peer up to a worker's process: its hooks and MCP server are
// its direct children (measured Claude -p, Codex app-server; pi's extension runs in it). No more: a
// shell tool execs its command in place (`zsh -lc`, `bash -c`), so a harness nested in the worker
// is its child and that harness's hooks are grandchildren (live, Codex).
const workerDepth = 1

// hostMatches reports whether a caller whose socket peer is pid may speak for host
// ("<harness>:<pid>:<start ms>", core.JoinAutoArgs.Host): the host process is alive with that
// start time (a reused pid is another process) and pid is it or one of its descendants within
// depth. The host string alone is no secret (ps shows it); the peer's process tree is.
func hostMatches(pid int, host string, depth int) bool {
	parts := strings.Split(host, ":")
	if len(parts) != 3 {
		return false
	}
	hpid, err1 := strconv.Atoi(parts[1])
	start, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || hpid <= 1 || pid <= 0 {
		return false
	}
	if st := local.ProcessStartTime(hpid); st == 0 || st != start {
		return false
	}
	for i := 0; i <= depth && pid > 1; i++ {
		if pid == hpid {
			return true
		}
		pid = local.ParentPID(pid)
	}
	return false
}

// peerWorkerOK reports whether the peer of nc is the worker process (pid, start time) or its
// descendant within workerDepth (core.Engine.WorkerProcess).
func (c *conn) peerWorkerOK(pid int, start int64) bool {
	return c.peerOK(fmt.Sprintf("worker:%d:%d", pid, start), workerDepth)
}

// peerHostOK checks host against the peer of nc once per connection and host.
func (c *conn) peerHostOK(host string) bool { return c.peerOK(host, hostDepth) }

func (c *conn) peerOK(host string, depth int) bool {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	if ok, seen := c.hosts[host]; seen {
		return ok
	}
	pid, err := peerPID(c.nc)
	ok := err == nil && hostMatches(pid, host, depth)
	if c.hosts == nil {
		c.hosts = map[string]bool{}
	}
	c.hosts[host] = ok
	return ok
}
