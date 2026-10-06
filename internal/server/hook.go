package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// hookTimeout bounds one run of a hook, including its child processes.
var hookTimeout = 10 * time.Second

// NotifyHooksDir is where the notify hooks live: every executable file in it is run for each notice.
func NotifyHooksDir(dir string) string { return filepath.Join(dir, "hooks", "notify.d") }

// NotifyHooks are the hooks the daemon runs for a notice: the regular executable files of
// hooks/notify.d, by name. legacy reports a hooks/notify file, which is no longer run (it belongs in
// notify.d); `piggery check` warns about it.
func NotifyHooks(dir string) (files []string, legacy bool) {
	if _, err := os.Lstat(filepath.Join(dir, "hooks", "notify")); err == nil {
		legacy = true
	}
	entries, _ := os.ReadDir(NotifyHooksDir(dir))
	for _, e := range entries {
		if fi, err := os.Stat(filepath.Join(NotifyHooksDir(dir), e.Name())); err == nil && hookRunnable(fi) {
			files = append(files, filepath.Join(NotifyHooksDir(dir), e.Name()))
		}
	}
	sort.Strings(files)
	return files, legacy
}

// notifyHook is core's notify sink: run every hook of hooks/notify.d asynchronously and in
// parallel, each with the message as one JSON line on stdin, its own timeout and process group.
// A failure is logged to stderr as JSON (message id, the hook's file name and the error, never the
// body); nothing else depends on it.
func (s *server) notifyHook(messageID string) {
	files, _ := NotifyHooks(s.dir)
	s.warnSkippedHooks()
	if len(files) == 0 {
		return
	}
	s.hooks.Add(1)
	go func() {
		defer s.hooks.Done()
		mail, err := s.eng.NotifyMail(context.Background(), messageID)
		if err != nil {
			s.log.Error("notify hook", "message", messageID, "err", err)
			return
		}
		line, _ := json.Marshal(mail)
		line = append(line, '\n')
		var wg sync.WaitGroup
		for _, path := range files {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
				defer cancel()
				if err := s.runHook(ctx, path, line); err != nil {
					s.log.Error("notify hook", "message", messageID, "hook", filepath.Base(path), "err", err, "timed_out", ctx.Err() != nil)
				}
			}()
		}
		wg.Wait()
	}()
}

// runHook runs one hook with line on its stdin until it ends or ctx does; the timeout ends the hook
// and what it started (hookKill).
func (s *server) runHook(ctx context.Context, path string, line []byte) error {
	cmd := hookCommand(ctx, path)
	cmd.Stdin = bytes.NewReader(line)
	attach, release := hookKill(cmd)
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	defer release()
	if err := attach(); err != nil {
		s.log.Warn("notify hook: its processes are not tracked", "hook", filepath.Base(path), "err", err)
	}
	return cmd.Wait()
}
