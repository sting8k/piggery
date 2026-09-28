// Package local is the local headless runtime driver: a
// process runner shared by every harness (process group, stdin, per-run log for tail,
// stop/kill, start time, recovery) plus one codec per harness that builds the command and
// reads its stdout. It never touches presence or mail: the harness adapter owns both.
package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

var _ core.RuntimeDriver = (*Driver)(nil)

// codec is the harness-specific half of a Driver: how a run starts, what its stdout records
// mean, and how control commands reach it over stdin.
type codec interface {
	harness() string
	toolPrefix() string
	// defaults are the profile's model and thinking level ("" none or unreadable).
	defaults() (model, thinking string)
	// launch builds the command of run s.
	launch(s core.Spec) (launch, error)
	// started runs once the process exists; an error kills the process group and fails Start.
	started(ctx context.Context, w *worker, l launch) error
	// record reads one stdout line of w.
	record(w *worker, line []byte) record
	abort(w *worker) error
	setModel(ctx context.Context, w *worker, model string) error
	setThinking(ctx context.Context, w *worker, level string) error
}

// launch is one run's command. env overrides the daemon's environment by key.
type launch struct {
	cmd      string
	args     []string
	env      []string
	model    string // the model the run was asked to run ("" the harness default), for started
	thinking string // the level the run was asked to run ("" none), for started
	data     any    // the codec's own, from launch to started
	cleanup  func() // after the run has ended or failed to start; may be nil
}

// record is what the runner does with one stdout line: keep it in the run log, add the
// standard records it means, hand it to the request waiting for id answers, and write reply on
// stdin.
//
// Standard records are what tail and top read, whatever the harness: pi's rpc
// shapes, which pi writes as is and another codec translates to (claude.go):
//   - {"type":"message_end","message":{"role","content":[{"type":"text","text"}],"usage":{"input","output","cacheRead","cacheWrite"},"stopReason","errorMessage"}}
//     (usage of an assistant message: the context in use)
//   - {"type":"tool_execution_start","toolName","args"}, {"type":"tool_execution_end","toolName","isError","result":{"content"}}
//   - {"type":"turn_end"}: one model call; {"type":"agent_end"}: the turn is over
type record struct {
	keep    bool
	std     [][]byte
	answers string
	reply   []byte
	// failed is the key of a turn the harness ended in failure without an end of its adapter
	// (Codex: turn/completed failed, no Stop hook); the runner reports it (Options.OnTurnFailed).
	failed string
	// ended are delivered batches this line ends (a deliverer codec); the runner reports each
	// (Options.OnDelivery).
	ended []core.DeliveryEnd
	// fatal ends the run: the runner writes it to the tail log as an error and kills the
	// worker (e.g. a harness too old for the codec).
	fatal string
	// turn is core.TurnStarted or core.TurnEnded for a turn no delivered batch started; the
	// runner reports it (Options.OnUnbatchedTurn).
	turn string
}

// Options tunes a Driver.
type Options struct {
	// OnExit is called once per run when its process ends (stopped or on its own), before
	// Stop/StopAll return. It must not call back into the driver.
	OnExit func(participantID, runID string, e core.Exit)
	// OnTurnFailed is called when a run's turn failed with no end from its adapter
	// (core.Engine.RuntimeTurnFailed). It must not call back into the driver.
	OnTurnFailed func(participantID, runID, key string)
	// OnDelivery is called when a delivered batch of a run ended (core.Engine.DeliveryEnded),
	// in the order the harness reported them.
	OnDelivery func(participantID, runID string, batch int64, outcome string)
	// OnUnbatchedTurn is called when a turn no delivered batch started begins or ends
	// (core.Engine.UnbatchedTurn).
	OnUnbatchedTurn func(participantID, runID, event string)
	// StopWait is how long Stop waits after closing stdin before SIGTERM (default 10s);
	// TermWait how long after SIGTERM before SIGKILL (default 5s).
	StopWait, TermWait time.Duration
}

// Driver runs local headless workers of one harness. Data lives under dir (~/.piggery).
type Driver struct {
	dir   string
	opts  Options
	codec codec

	mu    sync.Mutex
	procs map[string]*worker // by participant id; kept after exit so tail still works

	kill func(pid int, sig syscall.Signal) error // syscall.Kill; replaced in tests
}

type worker struct {
	participantID string
	runID         string
	harnessRef    string // the session id the harness chose (Codex's thread), set by started
	logPth        string
	pgid          int
	cmd           *exec.Cmd

	inMu  sync.Mutex // serializes stdin writes (replies, control commands) with its close (stop)
	stdin *os.File
	// inBroken: a write ran past sendWait, maybe after part of its line (a pipe write over
	// PIPE_BUF is not atomic), so the harness's JSON stream is broken; later sends fail at once.
	inBroken bool

	pendMu  sync.Mutex
	pending map[string]chan []byte // requests waiting for the stdout line that answers them, by id

	done chan struct{} // closed when the process has been reaped
	exit core.Exit
}

// Builtin is every built-in runtime driver over dir, one per harness.
func Builtin(dir string, opts Options) []*Driver {
	return []*Driver{New(dir, opts), NewClaude(dir, "", opts), NewCodex(dir, "", opts)}
}

// Harnesses names the harness of each built-in driver, in Builtin's order.
func Harnesses() []string {
	var out []string
	for _, d := range Builtin("", Options{}) {
		out = append(out, d.codec.harness())
	}
	return out
}

func newWith(dir string, opts Options, c codec) *Driver {
	if opts.StopWait == 0 {
		opts.StopWait = 10 * time.Second
	}
	if opts.TermWait == 0 {
		opts.TermWait = 5 * time.Second
	}
	return &Driver{dir: dir, opts: opts, codec: c, procs: map[string]*worker{}, kill: syscall.Kill}
}

// LogPath is the normalized stdout log of one run.
func LogPath(dir, participantID, runID string) string {
	return filepath.Join(dir, "logs", participantID, runID+".jsonl")
}

// Defaults implements core.RuntimeDriver: the codec's harness, and its profile's model and
// thinking level ("" when none or the profile is unreadable; Start reports that).
func (d *Driver) Defaults() (string, string, string) {
	m, t := d.codec.defaults()
	return d.codec.harness(), m, t
}

// ToolPrefix implements core.RuntimeDriver.
func (d *Driver) ToolPrefix() string { return d.codec.toolPrefix() }

// Capabilities: what a worker on the shared runner supports, pi and Claude alike (abort and
// model over stdin, wake and steer through its adapter, the role card in the system prompt,
// usage in the run log).
// Wake: the codec's, when it can start a turn itself (Codex); else the adapter wakes (ErrNoWake).
func (d *Driver) Wake(participantID string) error {
	wk, ok := d.codec.(waker)
	if !ok {
		return core.ErrNoWake
	}
	w, err := d.live(participantID)
	if err != nil {
		return err
	}
	return wk.wake(w)
}

// waker is a codec that starts a turn of an idle worker itself (no adapter can).
type waker interface{ wake(w *worker) error }

// Deliver hands the live worker a batch of mail through a deliverer codec.
func (d *Driver) Deliver(participantID string, dl core.Delivery) error {
	dv, ok := d.codec.(deliverer)
	if !ok {
		return fmt.Errorf("the %s driver does not deliver mail", d.codec.harness())
	}
	w, err := d.live(participantID)
	if err != nil {
		return err
	}
	return dv.deliver(w, dl)
}

// deliverer is a codec that gives the worker its mail itself (core.CapDeliver) and
// reports each batch's end in record.ended. deliver must not wait for an answer on
// stdout (it may run on the stdout reader's goroutine's behalf).
type deliverer interface {
	deliver(w *worker, d core.Delivery) error
}

func (d *Driver) Capabilities() []string {
	caps := []string{core.CapAbort, core.CapSetModel, core.CapWake, core.CapSteer, core.CapSystemPrompt, core.CapUsage}
	if _, ok := d.codec.(deliverer); ok {
		caps = append(caps, core.CapDeliver)
	}
	return caps
}

// Start launches the worker for s. A participant has at most one live process. The process
// outlives ctx: it belongs to the daemon, not to the request that started it.
func (d *Driver) Start(_ context.Context, s core.Spec) (core.Proc, error) {
	d.mu.Lock()
	if w := d.procs[s.ParticipantID]; w != nil && !w.exited() {
		d.mu.Unlock()
		return core.Proc{}, fmt.Errorf("participant %s already has a running worker", s.ParticipantID)
	}
	d.mu.Unlock()
	l, err := d.codec.launch(s)
	if err != nil {
		return core.Proc{}, err
	}
	cleanup := func() {
		if l.cleanup != nil {
			l.cleanup()
		}
	}
	logPth := LogPath(d.dir, s.ParticipantID, s.RunID)
	if err := os.MkdirAll(filepath.Dir(logPth), 0o700); err != nil {
		cleanup()
		return core.Proc{}, err
	}
	stderr, err := os.OpenFile(strings.TrimSuffix(logPth, ".jsonl")+".stderr", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		cleanup()
		return core.Proc{}, err
	}
	defer stderr.Close()
	// Own pipes (not StdoutPipe) so reaping the process never races the stdout reader.
	inR, inW, err := os.Pipe()
	if err != nil {
		cleanup()
		return core.Proc{}, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		cleanup()
		return core.Proc{}, err
	}

	cmd := exec.Command(l.cmd, l.args...)
	cmd.Dir = s.Cwd
	cmd.Env = workerEnv(os.Environ(), s, l.env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		cleanup()
		return core.Proc{}, err
	}

	w := &worker{participantID: s.ParticipantID, runID: s.RunID, logPth: logPth, pgid: cmd.Process.Pid, cmd: cmd, stdin: inW, done: make(chan struct{})}
	d.mu.Lock()
	d.procs[s.ParticipantID] = w
	d.mu.Unlock()

	go d.normalize(w, outR)
	go func() {
		err := cmd.Wait()
		w.exit = exitOf(cmd, err)
		w.inMu.Lock()
		w.stdin.Close()
		w.inMu.Unlock()
		// Report before done: Stop and StopAll return only once the exit is recorded.
		if d.opts.OnExit != nil {
			d.opts.OnExit(s.ParticipantID, s.RunID, w.exit)
		}
		cleanup() // the run has ended: nothing uses its files any more
		close(w.done)
	}()

	// After the reaper above: the kill waits on w.done.
	if err := d.codec.started(context.Background(), w, l); err != nil {
		d.kill(-w.pgid, syscall.SIGKILL)
		<-w.done
		return core.Proc{}, err
	}

	return core.Proc{
		PID:        cmd.Process.Pid,
		PGID:       cmd.Process.Pid,
		StartTime:  ProcessStartTime(cmd.Process.Pid),
		Cmdline:    append([]string{l.cmd}, l.args...),
		HarnessRef: w.harnessRef,
	}, nil
}

// workerEnv is the daemon's environment minus every PIGGERY_* variable and every key the
// launch sets, plus the launch's own variables and the worker's identity and run (the adapter
// identifies with PIGGERY_RUN_ID so its run matches the process row and the tail log). The
// admin token is a file, never an env var.
func workerEnv(base []string, s core.Spec, own []string) []string {
	set := map[string]bool{}
	for _, kv := range own {
		k, _, _ := strings.Cut(kv, "=")
		set[k] = true
	}
	env := make([]string, 0, len(base)+len(own)+3)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "PIGGERY_") && !set[k] {
			env = append(env, kv)
		}
	}
	env = append(env, "PIGGERY_ID="+s.ParticipantID, "PIGGERY_TOKEN="+s.Token, "PIGGERY_RUN_ID="+s.RunID)
	return append(env, own...)
}

// Stop ends the participant's worker: close stdin, wait StopWait, SIGTERM the process group,
// wait TermWait, SIGKILL the group. Stopping an exited worker returns its recorded exit.
func (d *Driver) Stop(_ context.Context, participantID string) (core.Exit, error) {
	d.mu.Lock()
	w := d.procs[participantID]
	d.mu.Unlock()
	if w == nil {
		return core.Exit{}, fmt.Errorf("no worker for participant %s", participantID)
	}
	if w.exited() {
		// Signal nothing: once the group is empty its pgid can belong to someone else.
		return w.exit, nil
	}
	w.inMu.Lock()
	w.stdin.Close()
	w.inMu.Unlock()
	if !w.wait(d.opts.StopWait) {
		d.kill(-w.pgid, syscall.SIGTERM)
		if !w.wait(d.opts.TermWait) {
			d.kill(-w.pgid, syscall.SIGKILL)
			<-w.done
		}
	}
	// The leader was alive when Stop began and just ended; do not leave the rest of its
	// group (tool subprocesses) behind.
	d.kill(-w.pgid, syscall.SIGKILL)
	return w.exit, nil
}

// Kill SIGKILLs the worker's process group at once (no stdin close, no grace) and returns the
// recorded exit. The group is ours: the driver started it and has not reaped it yet.
func (d *Driver) Kill(_ context.Context, participantID string) (core.Exit, error) {
	w, err := d.live(participantID)
	if err != nil {
		return core.Exit{}, err
	}
	d.kill(-w.pgid, syscall.SIGKILL)
	<-w.done
	return w.exit, nil
}

// Abort asks the harness to cancel the current turn; the worker stays alive.
func (d *Driver) Abort(participantID string) error {
	w, err := d.live(participantID)
	if err != nil {
		return err
	}
	return d.codec.abort(w)
}

// SetModel switches the live worker's model; a refusal by the harness is an error.
func (d *Driver) SetModel(ctx context.Context, participantID, model string) error {
	w, err := d.live(participantID)
	if err != nil {
		return err
	}
	return d.codec.setModel(ctx, w, model)
}

// SetThinking sets the live worker's thinking level; a level the harness does not run is an
// error and leaves the worker as it was.
func (d *Driver) SetThinking(ctx context.Context, participantID, level string) error {
	w, err := d.live(participantID)
	if err != nil {
		return err
	}
	return d.codec.setThinking(ctx, w, level)
}

// Tail returns the last n normalized records of the participant's latest run.
func (d *Driver) Tail(participantID string, n int) ([]json.RawMessage, error) {
	d.mu.Lock()
	w := d.procs[participantID]
	d.mu.Unlock()
	if w == nil {
		return nil, fmt.Errorf("no worker for participant %s", participantID)
	}
	b, err := os.ReadFile(w.logPth)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(b) == 0 {
		lines = nil
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]json.RawMessage, len(lines))
	for i, l := range lines {
		out[i] = json.RawMessage(l)
	}
	return out, nil
}

// live returns the participant's worker while its process runs.
func (d *Driver) live(participantID string) (*worker, error) {
	d.mu.Lock()
	w := d.procs[participantID]
	d.mu.Unlock()
	if w == nil || w.exited() {
		return nil, fmt.Errorf("participant %s: %w", participantID, core.ErrNotRunning)
	}
	return w, nil
}

func (w *worker) exited() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func (w *worker) wait(d time.Duration) bool {
	select {
	case <-w.done:
		return true
	case <-time.After(d):
		return false
	}
}

// normalize reads stdout to EOF (a harness stalls if nobody reads) and acts on each line as
// the codec says: log it, answer a waiting request, reply on stdin.
func (d *Driver) normalize(w *worker, r io.ReadCloser) {
	defer r.Close()
	f, err := os.OpenFile(w.logPth, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		io.Copy(io.Discard, r)
		return
	}
	defer f.Close()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			rec := d.codec.record(w, line)
			if rec.keep {
				f.Write(append(line, '\n'))
			}
			for _, s := range rec.std {
				f.Write(append(s, '\n'))
			}
			if rec.answers != "" {
				w.answer(rec.answers, line)
			}
			if rec.failed != "" && d.opts.OnTurnFailed != nil {
				d.opts.OnTurnFailed(w.participantID, w.runID, rec.failed)
			}
			for _, e := range rec.ended {
				if d.opts.OnDelivery != nil {
					d.opts.OnDelivery(w.participantID, w.runID, e.Batch, e.Outcome)
				}
			}
			if rec.turn != "" && d.opts.OnUnbatchedTurn != nil {
				d.opts.OnUnbatchedTurn(w.participantID, w.runID, rec.turn)
			}
			if rec.fatal != "" {
				f.Write(append(stdError(rec.fatal), '\n'))
				d.kill(-w.pgid, syscall.SIGKILL)
			}
			if rec.reply != nil {
				w.inMu.Lock()
				w.stdin.Write(append(rec.reply, '\n')) // fails harmlessly once stdin is closed
				w.inMu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// send writes one JSON line to w's stdin. Stop closes stdin under the same lock, so a write
// after it fails instead of interleaving.
func (w *worker) send(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.inMu.Lock()
	defer w.inMu.Unlock()
	if w.inBroken {
		return errors.New("worker stdin is broken: an earlier write timed out mid-line; stop the worker")
	}
	w.stdin.SetWriteDeadline(time.Now().Add(sendWait))
	if _, err := w.stdin.Write(append(line, '\n')); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			w.inBroken = true
			slog.Warn("worker stdin write timed out; its stdin is now broken, later commands fail",
				"participant", w.participantID, "run", w.runID, "wait", sendWait)
			return fmt.Errorf("worker stdin: not read within %s", sendWait)
		}
		return fmt.Errorf("worker stdin: %w", core.ErrNotRunning)
	}
	return nil
}

// sendWait bounds one stdin write, so a worker that stops reading cannot hang its caller
// (a wake runs on the send path).
const sendWait = 2 * time.Second

// request sends v, which carries id, and waits up to timeout for the stdout line the codec
// says answers id. name labels the errors.
func (w *worker) request(ctx context.Context, timeout time.Duration, id, name string, v any) ([]byte, error) {
	ch := make(chan []byte, 1)
	w.pendMu.Lock()
	if w.pending == nil {
		w.pending = map[string]chan []byte{}
	}
	w.pending[id] = ch
	w.pendMu.Unlock()
	defer func() {
		w.pendMu.Lock()
		delete(w.pending, id)
		w.pendMu.Unlock()
	}()
	if err := w.send(v); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case line := <-ch:
		return line, nil
	case <-w.done:
		return nil, fmt.Errorf("worker exited before answering %s: %w", name, core.ErrNotRunning)
	case <-ctx.Done():
		return nil, fmt.Errorf("no answer to %s within %s", name, timeout)
	}
}

// answer hands a response line to the request waiting for its id, if any.
func (w *worker) answer(id string, line []byte) {
	w.pendMu.Lock()
	ch := w.pending[id]
	w.pendMu.Unlock()
	if ch != nil {
		ch <- line
	}
}

func exitOf(cmd *exec.Cmd, err error) core.Exit {
	e := core.Exit{Code: -1, At: time.Now().UnixMilli()}
	st := cmd.ProcessState
	if st == nil {
		if err != nil {
			e.Signal = err.Error()
		}
		return e
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		e.Signal = signalName(ws.Signal())
		return e
	}
	e.Code = st.ExitCode()
	return e
}

func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	}
	return fmt.Sprintf("signal %d", int(sig))
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
