package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// hookTimeout bounds one run of a hook, including its child processes.
const hookTimeout = 10 * time.Second

// NotifyHookPath is the executable the daemon runs for each new message to notify.
func NotifyHookPath(dir string) string { return filepath.Join(dir, "hooks", "notify") }

// notifyHook is core's notify sink: when ~/.piggery/hooks/notify exists and is executable, run it
// asynchronously with the message as one JSON line on stdin. A failure is logged to stderr as
// JSON (message id and error only, never the body); nothing else depends on it.
func (s *server) notifyHook(messageID string) {
	path := NotifyHookPath(s.dir)
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return
	}
	s.hooks.Add(1)
	go func() {
		defer s.hooks.Done()
		ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
		defer cancel()
		mail, err := s.eng.NotifyMail(ctx, messageID)
		if err != nil {
			s.log.Error("notify hook", "message", messageID, "err", err)
			return
		}
		line, _ := json.Marshal(mail)
		cmd := exec.CommandContext(ctx, path)
		cmd.Stdin = bytes.NewReader(append(line, '\n'))
		// Own process group, so a timeout kills whatever the hook started too.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = time.Second
		if err := cmd.Run(); err != nil {
			s.log.Error("notify hook", "message", messageID, "err", err, "timed_out", ctx.Err() != nil)
		}
	}()
}
