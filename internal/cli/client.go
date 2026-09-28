package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/server"
)

// Client is one socket connection to the daemon. It never removes the socket.
type Client struct {
	conn  net.Conn
	r     *bufio.Reader
	next  int
	auth  *proto.Auth
	admin string
}

// Dial connects to the daemon in dir. When autostart is set and the connect fails, it starts
// `<this binary> serve` detached (log: dir/serve.log) once, then retries with backoff.
func Dial(dir string, autostart bool) (*Client, error) {
	sock := server.SocketPath(dir)
	conn, err := net.Dial("unix", sock)
	if err != nil && autostart {
		exited, serr := startDaemon(dir)
		if serr != nil {
			return nil, fmt.Errorf("connect %s: %v; start daemon: %v", sock, err, serr)
		}
		delay := 20 * time.Millisecond
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			select {
			case why := <-exited:
				// serve ended: another daemon won the lock (connect works), or it could not start
				// (say why, not "no such file").
				if conn, err = net.Dial("unix", sock); err != nil {
					return nil, fmt.Errorf("the daemon did not start: %s (log: %s)", why, serveLog(dir))
				}
			case <-time.After(delay):
			}
			if conn != nil {
				break
			}
			if conn, err = net.Dial("unix", sock); err == nil {
				break
			}
			delay = min(delay*2, 500*time.Millisecond)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", sock, err)
	}
	return &Client{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}, nil
}

// AsAdmin sends the admin token on every request; AsParticipant sends id/token.
func (c *Client) AsAdmin(token string) { c.admin, c.auth = token, nil }
func (c *Client) AsParticipant(id, token string) {
	c.auth, c.admin = &proto.Auth{ID: id, Token: token}, ""
}

// AsHost authenticates as the live session of a harness process (a Claude session's hooks and
// MCP server: "claude:<pid>:<start>"), which has no token of its own.
func (c *Client) AsHost(host string) { c.auth, c.admin = &proto.Auth{Host: host}, "" }

func (c *Client) Close() error { return c.conn.Close() }

// Call sends one request and returns the raw result, or the daemon's *core.Error.
func (c *Client) Call(verb string, args any) (json.RawMessage, error) {
	c.next++
	req := proto.Request{ID: strconv.Itoa(c.next), Verb: verb, Auth: c.auth, AdminToken: c.admin}
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		req.Args = raw
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	b, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp proto.Response
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("bad response: %w", err)
	}
	if resp.ID != req.ID {
		return nil, fmt.Errorf("response id %q for request %q", resp.ID, req.ID)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	if !resp.OK {
		return nil, errors.New("daemon returned neither ok nor error")
	}
	return resp.Result, nil
}

// CallInto is Call plus decoding into out.
func (c *Client) CallInto(verb string, args, out any) (json.RawMessage, error) {
	raw, err := c.Call(verb, args)
	if err != nil || out == nil {
		return raw, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return raw, &core.Error{Code: core.CodeInternal, Message: "decode result: " + err.Error()}
	}
	return raw, nil
}

func serveLog(dir string) string { return filepath.Join(dir, "serve.log") }

// startDaemon starts `<this binary> serve` detached, logging to serve.log. exited gets why it
// ended if it ends (its last line written to the log), which a daemon that started never does
// while this client waits.
func startDaemon(dir string) (exited <-chan string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(serveLog(dir), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	var from int64
	if st, err := logf.Stat(); err == nil {
		from = st.Size()
	}
	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan string, 1)
	go func() {
		err := cmd.Wait()
		ch <- lastLogLine(serveLog(dir), from, err)
	}()
	return ch, nil
}

// lastLogLine is the last line serve wrote to the log after offset from (its error), without
// the "piggery serve: " prefix; else how it exited.
func lastLogLine(path string, from int64, exit error) string {
	b, _ := os.ReadFile(path)
	if int64(len(b)) >= from {
		b = b[from:]
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return strings.TrimPrefix(l, "piggery serve: ")
	}
	if exit == nil {
		return "serve exited"
	}
	return "serve " + exit.Error()
}
