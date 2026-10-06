// Package server is `piggery serve`: the single daemon that owns the DB and serves the core Engine
// over a unix socket speaking proto JSON lines.
package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/proto"
	"github.com/sting8k/piggery/internal/store"
	"github.com/sting8k/piggery/manifests"
)

// Config locates the data dir. There is no env override; tests pass their own Dir.
type Config struct {
	Dir string
	// Version is the build version the daemon reports in ps (top shows it); nothing reads it back.
	Version string
	// Integrations, when set (the daemon of the real binary; tests leave it nil, since the
	// harness directories are the user's), makes the start bring the pi, omp, dsh and opencode extensions
	// setup installed up to this binary's integration version, and log one warning for each
	// outdated part it returns (claude, codex, paseo: only `piggery setup --outdated` changes
	// them). ps reports the same list, read again on each call (files only).
	Integrations func(dir string) []proto.Outdated
	// Latest, when set, is the newest release's tag: the call `piggery update --check` makes. A release
	// build (not server.DevBuild) with update.check on asks it at most once a day (updatecheck.go).
	Latest func(ctx context.Context) (string, error)
}

// DefaultDir is ~/.piggery.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".piggery"), nil
}

func SocketPath(dir string) string     { return filepath.Join(dir, "piggery.sock") }
func AdminTokenPath(dir string) string { return filepath.Join(dir, "admin.token") }
func LockPath(dir string) string       { return filepath.Join(dir, "piggery.lock") }
func DBPath(dir string) string         { return filepath.Join(dir, "piggery.db") }
func ArchiveDir(dir string) string     { return filepath.Join(dir, "archive") }

// noticesShown is how many of the latest notices ps carries (top shows them).
const noticesShown = 5

// maxLine bounds one request line.
const maxLine = 16 << 20

type server struct {
	idents       uint64 // identify counter, guarded by mu
	eng          *core.Engine
	adminToken   string
	log          *slog.Logger
	dir          string
	version      string                                // Config.Version, for ps
	integrations func(dir string) []proto.Outdated     // Config.Integrations, for ps
	latest       func(context.Context) (string, error) // Config.Latest, for the daily update check
	updateAt     time.Time                             // when it last asked (read from the cache at the first tick); tick only
	settings     Settings                              // config.yaml
	startedAt    time.Time
	stop         context.CancelFunc // the normal shutdown (as SIGINT/SIGTERM), for the stop verb

	mu    sync.Mutex
	conns map[*conn]struct{}
	// runs is each participant's current run as last returned by identify. A connection
	// bound to an older run belongs to a superseded process: no wakes, and its close does
	// not affect the participant's state.
	runs  map[string]string
	wg    sync.WaitGroup
	hooks sync.WaitGroup // running notify hooks; they read the DB
}

// pushTimeout bounds a wake push so a client that stopped reading cannot stall the sender.
const pushTimeout = time.Second

// conn is one client connection. Responses and pushes share it, so writes are serialized.
type conn struct {
	nc  net.Conn
	wmu sync.Mutex
	enc *json.Encoder

	// caller is set by identify (guarded by server.mu). Its RunID is used for every
	// participant verb on this connection, so a superseded process gets run.stale.
	caller *core.Caller
	ident  uint64 // order of its identify (server.idents); the newest gets the wake

	hmu   sync.Mutex
	hosts map[string]bool // peerHostOK results by host
}

func (c *conn) write(v any, timeout time.Duration) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if timeout > 0 {
		c.nc.SetWriteDeadline(time.Now().Add(timeout))
		defer c.nc.SetWriteDeadline(time.Time{})
	}
	return c.enc.Encode(v)
}

// Run acquires the singleton lock, opens the DB, binds the socket and serves until ctx is done.
// Only the lock holder may unlink a stale socket.
func Run(ctx context.Context, cfg Config) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock(LockPath(cfg.Dir))
	if err != nil {
		return err
	}
	defer unlock()
	settings, err := LoadSettings(cfg.Dir)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	token, err := loadOrCreateAdminToken(AdminTokenPath(cfg.Dir))
	if err != nil {
		return err
	}
	db, err := store.Open(DBPath(cfg.Dir))
	if err != nil {
		return err
	}
	defer db.Close()

	s := &server{adminToken: token, log: log, dir: cfg.Dir, version: cfg.Version, integrations: cfg.Integrations, latest: cfg.Latest, settings: settings, startedAt: time.Now(), stop: stop, conns: map[*conn]struct{}{}, runs: map[string]string{}}
	opts := local.Options{OnExit: func(participantID, runID string, e core.Exit) {
		// Every worker exit is recorded: one that ends on its own, and the ones StopAll ends
		// while the daemon shuts down (so not the request/daemon ctx). Idempotent after Stop.
		if err := s.eng.ProcessExited(context.Background(), participantID, runID, e); err != nil {
			s.log.Warn("process exited", "participant", participantID, "run", runID, "err", err)
		}
	}, OnTurnFailed: func(participantID, runID, key string) {
		if err := s.eng.RuntimeTurnFailed(context.Background(), participantID, runID, key); err != nil {
			s.log.Warn("turn failed", "participant", participantID, "run", runID, "err", err)
		}
	}, OnDelivery: func(participantID, runID string, batch int64, outcome string) {
		if err := s.eng.DeliveryEnded(context.Background(), participantID, runID, batch, outcome); err != nil {
			s.log.Warn("delivery ended", "participant", participantID, "run", runID, "batch", batch, "err", err)
		}
	}, OnUnbatchedTurn: func(participantID, runID, event string) {
		if err := s.eng.UnbatchedTurn(context.Background(), participantID, runID, event); err != nil {
			s.log.Warn("turn", "participant", participantID, "run", runID, "event", event, "err", err)
		}
	}}
	// Every built-in runtime driver; a worker runs on the one its harness names.
	drivers := local.Builtin(cfg.Dir, opts)
	// The built-in templates into ~/.piggery/templates, never over the user's edits.
	if err := manifests.Unpack(cfg.Dir); err != nil {
		return fmt.Errorf("templates: %w", err)
	}
	// The Human's shared prompts: what cannot work is logged and left out, never a reason not to start.
	var promptWarns []string
	settings.Prompts, promptWarns = CheckPrompts(cfg.Dir, settings.Prompts)
	for _, w := range append(settings.Warnings, promptWarns...) {
		log.Warn("shared prompts", "problem", w)
	}
	// Every config file piggery owns gets the keys it lacks, as `piggery setup` does; a file its
	// parser refuses is only logged here, the parser says why where it is read.
	filled, errs := EnsureFiles(cfg.Dir)
	for _, f := range filled {
		log.Info("config keys added", "file", f.Path, "keys", f.Added)
	}
	for _, err := range errs {
		log.Warn("config file", "err", err)
	}
	if cfg.Integrations != nil {
		// The extensions `piggery setup pi|omp|dsh|opencode` installed: rewritten when their integration
		// version is lower than this binary's, and only then.
		for _, x := range []struct {
			what, ext string
			update    func(string) (bool, error)
		}{
			{"pi extension", local.PiExtDir(cfg.Dir), local.UpdatePiExt},
			{"omp extension", local.OmpExtDir(cfg.Dir), local.UpdateOmpExt},
			{"dsh plugin", local.DshExtDir(cfg.Dir), local.UpdateDshExt},
			{"opencode plugin", local.OpencodeExtDir(cfg.Dir), local.UpdateOpencodeExt},
		} {
			if updated, err := x.update(x.ext); err != nil {
				log.Warn(x.what, "dir", x.ext, "err", err)
			} else if updated {
				log.Info(x.what+" updated", "dir", x.ext)
			}
		}
		// What is installed in a harness's own config or app is never changed on its own.
		for _, o := range cfg.Integrations(cfg.Dir) {
			log.Warn("outdated integration: " + o.String() + ": piggery setup --outdated")
		}
	}
	// A solo uses the limits of the built-in p2p template.
	soloTemplate, err := manifests.Builtin("p2p")
	if err != nil {
		return fmt.Errorf("built-in p2p: %w", err)
	}
	runtimes := make([]core.Option, len(drivers))
	for i, d := range drivers {
		runtimes[i] = core.WithRuntime(d)
	}
	s.eng = core.New(db, append(runtimes, core.WithNotify(s.wake),
		core.WithDefaultHarness(settings.Harness), core.WithAllowedRoots(settings.AllowedRoots),
		core.WithSharedPrompts(promptsFor(s.dir, settings.Prompts, func(m string) { s.log.Warn("shared prompts", "problem", m) })), core.WithNotifySink(s.notifyHook),
		core.WithTemplates(func(name, _ string) (string, error) { return manifests.Resolve(name, s.dir) }),
		core.WithTemplateList(func(string) ([]core.TemplateRef, error) {
			ls, err := manifests.List(s.dir)
			refs := make([]core.TemplateRef, len(ls))
			for i, l := range ls {
				refs[i] = core.TemplateRef(l)
			}
			return refs, err
		}),
		core.WithSoloTemplate(soloTemplate),
		core.WithRoleChanged(func(id string) { s.push(id, proto.EventRole) }),
		core.WithAbortPush(func(id string) int { return s.push(id, proto.EventAbort) }))...)
	// Recovery before anyone can connect: decide every worker and session the previous daemon left
	// behind.
	if err := s.eng.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	ln, err := Listen(cfg.Dir)
	if err != nil {
		return err
	}

	log.Info("serving", "socket", Address(cfg.Dir))

	s.wg.Add(1)
	go s.tick(ctx)
	s.wg.Add(1)
	go s.autoGC(ctx) // after reconcile, now and then every day

	go func() {
		<-ctx.Done()
		ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.nc.Close()
		}
		s.mu.Unlock()
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Error("accept", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		cn := &conn{nc: c, enc: json.NewEncoder(c)}
		s.mu.Lock()
		s.conns[cn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveConn(ctx, cn)
	}
	s.wg.Wait()
	// Graceful stop: end every worker (agent stop escalation) and record the exits before the
	// DB closes.
	var stopping sync.WaitGroup
	for _, d := range drivers {
		stopping.Add(1)
		go func() {
			defer stopping.Done()
			d.StopAll(context.Background())
		}()
	}
	stopping.Wait()
	s.hooks.Wait() // bounded by hookTimeout

	log.Info("stopped")
	return nil
}

func (s *server) tick(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.eng.FireDue(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("fire_due", "err", err)
			}
			if _, err := s.eng.Watch(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("watch", "err", err)
			}
			s.checkUpdate(ctx, time.Now())
		}
	}
}

func (s *server) serveConn(ctx context.Context, c *conn) {
	defer s.wg.Done()
	defer s.closeConn(ctx, c)
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		var req proto.Request
		var resp proto.Response
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			resp = errResponse("", &core.Error{Code: core.CodeInvalid, Message: "bad request: " + err.Error()})
		} else {
			resp = s.handle(ctx, c, req)
		}
		if err := c.write(resp, 0); err != nil {
			return
		}
		if resp.OK && req.Verb == proto.VerbShutdown {
			s.stop() // after the answer: shutting down closes this connection
		}
	}
}

// closeConn drops c. If c was identified and no other live connection is identified for the same
// participant, the participant goes gone. Not while the daemon itself is stopping: the sessions are
// still alive and will reconnect (recovery).
func (s *server) closeConn(ctx context.Context, c *conn) {
	c.nc.Close()
	s.mu.Lock()
	delete(s.conns, c)
	caller, others := c.caller, false
	if caller != nil {
		for o := range s.conns {
			if o.caller != nil && o.caller.ParticipantID == caller.ParticipantID && o.caller.RunID == caller.RunID {
				others = true
				break
			}
		}
	}
	current := caller != nil && s.runs[caller.ParticipantID] == caller.RunID
	s.mu.Unlock()
	if !current || others || ctx.Err() != nil {
		return
	}
	if err := s.eng.Presence(ctx, *caller, core.PresenceArgs{Event: core.PresenceShutdown}); err != nil && ctx.Err() == nil {
		s.log.Warn("presence on close", "participant", caller.ParticipantID, "err", err)
	}
}

// retireTeam tells every connection identified in the (now closed) team to stop driving,
// except (a gate closing its own team: its response is its signal).
func (s *server) retireTeam(teamID string, except *conn) {
	s.mu.Lock()
	var targets []*conn
	for c := range s.conns {
		if c != except && c.caller != nil && c.caller.TeamID == teamID {
			targets = append(targets, c)
		}
	}
	s.mu.Unlock()
	for _, c := range targets {
		if err := c.write(proto.Push{Event: proto.EventRetire, Reason: "team.closed"}, pushTimeout); err != nil {
			s.log.Warn("retire push", "team", teamID, "err", err)
		}
	}
}

// wake (core notify hook) pushes a wake frame, naming the participant's current session id, to its newest
// identified connection only: a harness that briefly runs two adapters of one session (Codex
// after /clear: the old MCP server lives ~30 s more) must not get two nudges.
func (s *server) wake(participantID string) {
	ref := s.eng.SessionRef(context.Background(), participantID)
	s.mu.Lock()
	var newest *conn
	run := s.runs[participantID]
	for c := range s.conns {
		if c.caller != nil && c.caller.ParticipantID == participantID && c.caller.RunID == run &&
			(newest == nil || c.ident > newest.ident) {
			newest = c
		}
	}
	s.mu.Unlock()
	if newest != nil {
		if err := newest.write(proto.Push{Event: proto.EventWake, Ref: ref}, pushTimeout); err != nil {
			s.log.Warn("push", "event", proto.EventWake, "participant", participantID, "err", err)
		}
	}
}

// push sends event to every connection identified as participantID's current run.
func (s *server) push(participantID, event string) int {
	s.mu.Lock()
	var targets []*conn
	run := s.runs[participantID]
	for c := range s.conns {
		if c.caller != nil && c.caller.ParticipantID == participantID && c.caller.RunID == run {
			targets = append(targets, c)
		}
	}
	s.mu.Unlock()
	n := 0
	for _, c := range targets {
		if err := c.write(proto.Push{Event: event}, pushTimeout); err != nil {
			s.log.Warn("push", "event", event, "participant", participantID, "err", err)
		} else {
			n++
		}
	}
	return n
}

// handle authenticates by verb class and dispatches. Admin verbs check only AdminToken;
// participant verbs check only Auth. Neither falls back to the other.
func (s *server) handle(ctx context.Context, cn *conn, req proto.Request) proto.Response {
	switch req.Verb {
	case proto.VerbJoinAuto:
		// No token: a pi session outside PIGGERY_* registers as a solo, or resumes its participant
		// of an open team. The socket's 0600 mode is the boundary.
		return call(req, func(a core.JoinAutoArgs) (any, error) { return s.eng.JoinAuto(ctx, a) })
	case proto.VerbTeamUp, proto.VerbJoin, proto.VerbLog, proto.VerbRelease,
		proto.VerbWhy, proto.VerbDoctor, proto.VerbLabels, proto.VerbTeamDown, proto.VerbGC, proto.VerbPs, proto.VerbTail, proto.VerbShutdown, proto.VerbAbort, proto.VerbKill, proto.VerbResume, proto.VerbModel, proto.VerbModels:
		if !s.isAdmin(req.AdminToken) {
			return errResponse(req.ID, s.unauthorized(req.Verb, "admin token required"))
		}
		switch req.Verb {
		case proto.VerbTeamUp:
			return call(req, func(a core.TeamUpArgs) (any, error) {
				team, err := s.eng.TeamUp(ctx, a)
				for _, w := range team.Warnings {
					s.log.Warn("team up", "team", team.Name, "warning", w)
				}
				return team, err
			})
		case proto.VerbPs:
			return call(req, func(a core.StateArgs) (any, error) {
				st, err := s.eng.State(ctx, a)
				r := proto.PsResult{PID: os.Getpid(), StartedAt: s.startedAt.UnixMilli(), Version: s.version, State: st}
				if s.integrations != nil {
					r.Outdated = s.integrations(s.dir)
				}
				r.Update = UpdateAvailable(s.dir, s.settings, s.version)
				r.Notices, _ = s.eng.Notices(ctx, noticesShown)
				return r, err
			})
		case proto.VerbShutdown:
			return call(req, func(struct{}) (any, error) { return struct{}{}, nil })
		case proto.VerbAbort:
			return call(req, func(a core.AdminTarget) (any, error) { return s.eng.Abort(ctx, a) })
		case proto.VerbKill:
			return call(req, func(a core.AdminTarget) (any, error) { return s.eng.Kill(ctx, a) })
		case proto.VerbResume:
			return call(req, func(a core.AdminTarget) (any, error) { return s.eng.Resume(ctx, a) })
		case proto.VerbModel:
			return call(req, func(a core.ModelArgs) (any, error) { return s.eng.SetModel(ctx, a) })
		case proto.VerbModels:
			return call(req, func(a core.AdminTarget) (any, error) { return s.eng.Models(ctx, a) })
		case proto.VerbTail:
			return call(req, func(a core.WorkerLogArgs) (any, error) {
				w, err := s.eng.WorkerLog(ctx, a)
				r := proto.TailResult{WorkerLog: w}
				if w.RunID != "" {
					r.Path = local.LogPath(s.dir, w.ParticipantID, w.RunID)
				}
				return r, err
			})
		case proto.VerbJoin:
			return call(req, func(a core.JoinArgs) (any, error) { return s.eng.Join(ctx, a) })
		case proto.VerbRelease:
			return call(req, func(a core.ReleaseArgs) (any, error) { return nil, s.eng.Release(ctx, a) })
		case proto.VerbWhy:
			return call(req, func(a core.WhyArgs) (any, error) { return s.eng.Why(ctx, a) })
		case proto.VerbDoctor:
			return call(req, func(struct{}) (any, error) { return s.eng.Doctor(ctx) })
		case proto.VerbLabels:
			return call(req, func(a core.LabelsArgs) (any, error) { return s.eng.Labels(ctx, a) })
		case proto.VerbTeamDown:
			return call(req, func(a core.TeamDownArgs) (any, error) {
				r, err := s.eng.TeamDown(ctx, a)
				if err == nil {
					s.retireTeam(r.TeamID, nil)
				}
				return r, err
			})
		case proto.VerbGC:
			return call(req, func(a core.GCArgs) (any, error) {
				res, err := s.eng.GC(ctx, gcPlace(s.dir), a)
				if err != nil || a.DryRun {
					return res, err
				}
				res.ExpiredArchives, err = s.eng.ExpireArchives(ArchiveDir(s.dir), s.settings.GCArchiveKeep)
				return res, err
			})
		default:
			return call(req, func(a core.LogArgs) (any, error) { return s.eng.Log(ctx, a) })
		}
	case proto.VerbSend, proto.VerbInbox, proto.VerbCompletion, proto.VerbWho, proto.VerbBoard,
		proto.VerbWatchAdd, proto.VerbWatchList, proto.VerbAgent, proto.VerbIdentify, proto.VerbPresence,
		proto.VerbHarnessEvent:
	default:
		return errResponse(req.ID, &core.Error{Code: core.CodeInvalid, Message: "unknown verb " + req.Verb})
	}

	byHost := req.Auth != nil && req.Auth.ID == "" && req.Auth.Token == "" && req.Auth.Host != ""
	if !byHost && (req.Auth == nil || req.Auth.ID == "" || req.Auth.Token == "") {
		msg := "participant id and token required"
		if req.AdminToken != "" { // --admin on a participant verb: say which verb family it is
			msg = req.Verb + " is a participant verb (PIGGERY_ID/PIGGERY_TOKEN), not an admin one; " +
				"for an overview use `piggery ps`"
		}
		return errResponse(req.ID, s.unauthorized(req.Verb, msg))
	}
	var c core.Caller
	var err error
	if byHost {
		if !cn.peerHostOK(req.Auth.Host) {
			return errResponse(req.ID, &core.Error{Code: core.CodeUnauthorized, RuleID: "host.peer",
				Message: "this connection does not come from the host process", Layer: "token"})
		}
		c, err = s.eng.AuthenticateHost(ctx, req.Auth.Host)
	} else {
		c, err = s.eng.Authenticate(ctx, req.Auth.ID, req.Auth.Token)
		if err == nil && (req.Verb == proto.VerbIdentify || req.Verb == proto.VerbHarnessEvent) {
			// The token is inherited by everything the worker runs; only its own process (and its
			// hooks) may be it in a turn. Other verbs from its shell need only the token.
			pid, start, bound, werr := s.eng.WorkerProcess(ctx, c.ParticipantID)
			if werr != nil {
				err = werr
			} else if bound && !cn.peerWorkerOK(pid, start) {
				err = &core.Error{Code: core.CodeUnauthorized, RuleID: "worker.peer", Layer: "token",
					Message: "this connection does not come from the worker's process: a harness nested in a worker cannot speak for it"}
			}
		}
	}
	if err != nil {
		return errResponse(req.ID, err)
	}
	s.mu.Lock()
	if b := cn.caller; b != nil && b.ParticipantID == c.ParticipantID {
		c.RunID = b.RunID // the run this connection identified as, not the token's latest
	}
	s.mu.Unlock()
	switch req.Verb {
	case proto.VerbSend:
		return call(req, func(a core.SendArgs) (any, error) { return s.eng.Send(ctx, c, a) })
	case proto.VerbInbox:
		return call(req, func(a core.InboxArgs) (any, error) { return s.eng.Inbox(ctx, c, a) })
	case proto.VerbCompletion:
		return call(req, func(a core.CompletionArgs) (any, error) { return s.eng.Completion(ctx, c, a) })
	case proto.VerbWho:
		return call(req, func(struct{}) (any, error) { return s.eng.Who(ctx, c) })
	case proto.VerbBoard:
		return call(req, func(struct{}) (any, error) { return s.eng.Board(ctx, c) })
	case proto.VerbWatchAdd:
		return call(req, func(a core.TimerArgs) (any, error) { return s.eng.WatchAdd(ctx, c, a) })
	case proto.VerbWatchList:
		return call(req, func(struct{}) (any, error) { return s.eng.WatchList(ctx, c) })
	case proto.VerbIdentify:
		return call(req, func(a core.IdentifyArgs) (any, error) {
			s.mu.Lock()
			if b := cn.caller; b != nil && b.ParticipantID == c.ParticipantID && b.RunID == c.RunID && !a.NewRun {
				a.Again = true // the same connection after a role change, not a reconnect
			}
			s.mu.Unlock()
			res, err := s.eng.Identify(ctx, c, a)
			if err != nil {
				return nil, err
			}
			bound := c
			bound.RunID = res.RunID
			s.mu.Lock()
			cn.caller = &bound
			s.idents++
			cn.ident = s.idents
			s.runs[bound.ParticipantID] = bound.RunID
			s.mu.Unlock()
			return res, nil
		})
	case proto.VerbPresence:
		return call(req, func(a core.PresenceArgs) (any, error) { return nil, s.eng.Presence(ctx, c, a) })
	case proto.VerbHarnessEvent:
		return call(req, func(a core.HarnessEventArgs) (any, error) { return s.eng.HarnessEvent(ctx, c, a) })
	default: // VerbAgent
		return call(req, func(a core.AgentArgs) (any, error) {
			r, err := s.eng.Agent(ctx, c, a)
			if err == nil && a.Action == core.AgentClose {
				s.retireTeam(r.TeamID, cn)
			}
			for _, w := range r.Warnings {
				s.log.Warn("found", "team", r.TeamName, "warning", w)
			}
			return r, err
		})
	}
}

func (s *server) isAdmin(tok string) bool {
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) == 1
}

// call decodes req.Args into A, runs f, and encodes its result.
func call[A any](req proto.Request, f func(A) (any, error)) proto.Response {
	var a A
	if len(req.Args) > 0 {
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return errResponse(req.ID, &core.Error{Code: core.CodeInvalid, Message: "bad args: " + err.Error()})
		}
	}
	out, err := f(a)
	if err != nil {
		return errResponse(req.ID, err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return errResponse(req.ID, err)
	}
	return proto.Response{ID: req.ID, OK: true, Result: raw}
}

func errResponse(id string, err error) proto.Response {
	var ce *core.Error
	if !errors.As(err, &ce) {
		ce = &core.Error{Code: core.CodeInternal, Message: err.Error()}
	}
	return proto.Response{ID: id, Error: ce}
}

// unauthorized is a refusal made before any participant is known, so there is no event to
// write; it goes to stderr as structured JSON (verb and reason only, never tokens or bodies).
func (s *server) unauthorized(verb, msg string) *core.Error {
	s.log.Warn("unauthorized", "verb", verb, "reason", msg)
	return &core.Error{Code: core.CodeUnauthorized, Message: msg, Layer: "token"}
}

func loadOrCreateAdminToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, os.Chmod(path, 0o600)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, os.Chmod(path, 0o600)
}
