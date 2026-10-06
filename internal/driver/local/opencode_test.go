package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sting8k/piggery/internal/core"
)

const ocFixtures = "../../../testdata/fixtures/opencode/1.18.34/"

// TestOpencodeHelperProcess is the fake `opencode serve` (not a test): the routes the driver uses,
// answering with what the captures hold (testdata/fixtures/opencode/1.18.34/serve), PGDRV_OPENCODE=1.
// It checks the basic-auth password, prints the listening line, ignores stdin, keeps a detached
// `sleep` child as a tool's shell (killed only by an abort, as opencode does), and writes what it
// is asked and its environment to PGDRV_LOG, one JSON line each. An SSE client gets PGDRV_EVENTS
// (an SSE capture; its session id replaced by the one created) and whatever is POSTed to /_emit.
func TestOpencodeHelperProcess(t *testing.T) {
	if os.Getenv("PGDRV_OPENCODE") == "" {
		return
	}
	var port string
	for i, a := range os.Args {
		if a == "--port" {
			port = os.Args[i+1]
		}
	}
	logf, _ := os.OpenFile(os.Getenv("PGDRV_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	var logMu sync.Mutex
	logLine := func(v any) {
		b, _ := json.Marshal(v)
		logMu.Lock()
		logf.Write(append(b, '\n'))
		logMu.Unlock()
	}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "OPENCODE_") || strings.HasPrefix(k, "PIGGERY_") {
			if k == "OPENCODE_SERVER_PASSWORD" {
				v = "len=" + strconv.Itoa(len(v))
			}
			env[k] = v
		}
	}
	logLine(map[string]any{"env": env, "args": os.Args})
	// a tool's shell: its own process group, so a signal to serve's group misses it
	child := sleeper()
	isolate(child)
	child.Start()
	os.WriteFile(os.Getenv("PGDRV_CHILD"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)

	var mu sync.Mutex
	session, sessionBody := "", ""
	subs := map[chan string]bool{}
	broadcast := func(data string) {
		mu.Lock()
		for c := range subs {
			c <- data
		}
		mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_emit", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		broadcast(string(b))
	})
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"healthy":true,"version":"1.18.34"}`)
	})
	mux.HandleFunc("/config/providers", func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile(os.Getenv("PGDRV_PROVIDERS"))
		w.Write(b)
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"model":"hp/glm-5.3-flash"}`) })
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		logLine(map[string]any{"req": "POST /session", "dir": r.Header.Get("x-opencode-directory"), "body": json.RawMessage(b)})
		mu.Lock()
		session, sessionBody = "ses_fake1", string(b)
		mu.Unlock()
		io.WriteString(w, `{"id":"ses_fake1"}`)
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		id, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/session/"), "/")
		b, _ := io.ReadAll(r.Body)
		logLine(map[string]any{"req": r.Method + " " + r.URL.Path, "body": json.RawMessage(append([]byte(nil), firstNonEmptyBytes(b, []byte("null"))...))})
		switch {
		case rest == "" && r.Method == "GET":
			if id != os.Getenv("PGDRV_KNOWN") {
				http.Error(w, `{"name":"NotFoundError"}`, 404)
				return
			}
			mu.Lock()
			session = id
			mu.Unlock()
			io.WriteString(w, `{"id":"`+id+`","model":{"id":"glm-5.3-flash","providerID":"hp","variant":"default"}}`)
		case rest == "abort":
			if pid, err := strconv.Atoi(strings.TrimSpace(readFile(os.Getenv("PGDRV_CHILD")))); err == nil {
				killPID(pid)
			}
			io.WriteString(w, "true")
		case rest == "prompt_async":
			w.WriteHeader(204)
		}
	})
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("content-type", "text/event-stream")
		c := make(chan string, 256)
		mu.Lock()
		subs[c] = true
		mu.Unlock()
		io.WriteString(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		fl.Flush()
		if ev := os.Getenv("PGDRV_EVENTS"); ev != "" {
			go func() {
				mu.Lock()
				id := session
				mu.Unlock()
				f, _ := os.Open(ev)
				defer f.Close()
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 1<<20), 16<<20)
				for sc.Scan() {
					var l struct{ Data json.RawMessage }
					if json.Unmarshal(sc.Bytes(), &l) == nil && len(l.Data) > 0 {
						old := os.Getenv("PGDRV_OLDSID")
						c <- strings.ReplaceAll(string(l.Data), old, id)
					}
				}
			}()
		}
		for {
			select {
			case d := <-c:
				io.WriteString(w, "data: "+d+"\n\n")
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	password := os.Getenv("OPENCODE_SERVER_PASSWORD")
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); r.URL.Path != "/_emit" && (!ok || u != "opencode" || p != password) {
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
	_ = sessionBody
	fmt.Println("Warning: OPENCODE_SERVER_PASSWORD is not set; server is unsecured.") // a first line the driver must skip, as serve prints it without a password
	l, err := net.Listen("tcp", "127.0.0.1:"+port)                                    // listening before the line, as serve is
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("opencode server listening on http://127.0.0.1:" + port)
	http.Serve(l, h)
	os.Exit(0)
}

func firstNonEmptyBytes(a, b []byte) []byte {
	if len(a) > 0 {
		return a
	}
	return b
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

type ocEnv struct {
	d     *Driver
	dir   string
	log   string
	child string
}

// newOpencodeDriver is the driver over a temp dir whose `opencode` is the test binary standing in
// for serve, answering with the captures; events is an SSE capture to replay (and its session id).
func newOpencodeDriver(t *testing.T, prof OpencodeProfile, events, oldSID string, opts ...Options) ocEnv {
	t.Helper()
	dir := t.TempDir()
	prof.Cmd = useFakeHarness(t, "TestOpencodeHelperProcess")
	b, _ := json.Marshal(prof)
	os.MkdirAll(filepath.Dir(OpencodeProfilePath(dir)), 0o700)
	if err := os.WriteFile(OpencodeProfilePath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	e := ocEnv{dir: dir, log: filepath.Join(t.TempDir(), "fake.log"), child: filepath.Join(t.TempDir(), "child.pid")}
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	t.Setenv("PGDRV_OPENCODE", "1")
	t.Setenv("PGDRV_LOG", e.log)
	t.Setenv("PGDRV_CHILD", e.child)
	t.Setenv("PGDRV_PROVIDERS", abs(ocFixtures+"serve/08-models.config-providers.json"))
	t.Setenv("PGDRV_KNOWN", "ses_resume")
	t.Setenv("PGDRV_EVENTS", "")
	if events != "" {
		t.Setenv("PGDRV_EVENTS", abs(ocFixtures+"serve/"+events))
		t.Setenv("PGDRV_OLDSID", oldSID)
	}
	e.d = NewOpencode(dir, append(opts, Options{StopWait: 5 * time.Second})[0])
	return e
}

// fakeLog is the lines the fake serve wrote.
func (e ocEnv) fakeLog(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(readFile(e.log)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (e ocEnv) requests(t *testing.T, prefix string) []map[string]any {
	var out []map[string]any
	for _, m := range e.fakeLog(t) {
		if r, _ := m["req"].(string); strings.HasPrefix(r, prefix) {
			out = append(out, m)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A spawn runs `opencode serve --port N` on a port the driver chose, with the password only in the
// environment, the plugin in OPENCODE_CONFIG_CONTENT (no config file read or written), every
// permission allowed but the profile's denied tools minus the role's allow_tools; it makes the
// session (title, the participant in its metadata for the plugin, the same rules, the model) and
// returns opencode's own ses_ id as the harness ref; the events of the run reach the log as standard
// records. Stop aborts the session first (that is what ends a tool's shell) and does not wait out a
// stdin EOF that serve ignores.
func TestOpencodeStartStop(t *testing.T) {
	e := newOpencodeDriver(t, OpencodeProfile{Model: "inherit", Thinking: "inherit", DisabledTools: []string{"question", "task"},
		Blacklist: []string{"fetch"}, DisableClaudeCode: true, Env: []string{"FOO=bar"}}, "04-steer.sse.jsonl", "ses_ef557cd24ffep0Hh01aL1hQede")
	ctx := context.Background()
	proc, err := e.d.Start(ctx, core.Spec{ParticipantID: "p1", RunID: "r1", Token: "tok", Cwd: e.dir, HarnessRef: "uuid-1",
		Model: "hp/glm-5.3-flash", AllowTools: []string{"task"}})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			e.d.Stop(ctx, "p1")
		}
	})
	if proc.HarnessRef != "ses_fake1" {
		t.Errorf("harness ref %q: the ses_ id opencode made", proc.HarnessRef)
	}
	line := strings.Join(proc.Cmdline[1:], " ")
	if !strings.Contains(line, "serve --port ") || !strings.Contains(line, "--hostname 127.0.0.1") || strings.Contains(line, "password") {
		t.Errorf("cmdline %q", proc.Cmdline)
	}
	first := e.fakeLog(t)[0]
	env := first["env"].(map[string]any)
	if env["OPENCODE_SERVER_PASSWORD"] != "len=48" || env["OPENCODE_PERMISSION"] != `{"*":"allow","question":"deny"}` ||
		env["OPENCODE_DISABLE_AUTOUPDATE"] != "1" || env["OPENCODE_DISABLE_CLAUDE_CODE"] != "1" || env["PIGGERY_ID"] != "p1" || env["PIGGERY_RUN_ID"] != "r1" {
		t.Errorf("env %v", env)
	}
	if _, ok := env["PIGGERY_OPENCODE_SESSION"]; ok {
		t.Error("a new session was given as a session to resume")
	}
	var cfg struct {
		Plugin     []string
		Share      string
		Autoupdate bool
		MCP        map[string]struct{ Enabled bool }
	}
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"].(string)), &cfg); err != nil || !slices.Equal(cfg.Plugin, []string{OpencodePluginSpec(OpencodeEntry(e.dir))}) ||
		cfg.Share != "disabled" || cfg.Autoupdate || cfg.MCP["fetch"].Enabled {
		t.Errorf("config content %v: %v", env["OPENCODE_CONFIG_CONTENT"], err)
	}
	if v, ok := OpencodeExtVersion(OpencodeExtDir(e.dir)); !ok || v != IntegrationVersion("opencode") {
		t.Errorf("plugin copy version %d managed %v", v, ok)
	}
	create := e.requests(t, "POST /session")[0]
	if create["dir"] != e.dir {
		t.Errorf("project directory header %v", create["dir"])
	}
	body, _ := json.Marshal(create["body"])
	if string(body) != `{"metadata":{"piggery_participant":"p1"},"model":{"id":"glm-5.3-flash","providerID":"hp"},`+
		`"permission":[{"action":"allow","pattern":"*","permission":"*"},{"action":"deny","pattern":"*","permission":"question"}],"title":"piggery p1"}` {
		t.Errorf("session body %s", body)
	}
	waitRecord(t, e.d, "p1", "agent_end")
	childPID, _ := strconv.Atoi(readFile(e.child))
	if !procAlive(childPID) {
		t.Fatal("the fake tool shell is gone before the stop")
	}
	t0 := time.Now()
	exit, err := e.d.Stop(ctx, "p1")
	stopped = true
	if err != nil || time.Since(t0) > 3*time.Second {
		t.Errorf("stop took %v: %v (stdin EOF does not end serve, so it must not be waited for)", time.Since(t0), err)
	}
	if exit.Signal == "" {
		t.Errorf("exit %+v: serve does not exit by itself", exit)
	}
	if got := e.requests(t, "POST /session/ses_fake1/abort"); len(got) != 1 {
		t.Errorf("abort requests at stop: %d", len(got))
	}
	waitFor(t, "the tool shell to be gone", func() bool { return !procAlive(childPID) })
}

// Resume continues the session core names: the plugin is told which (PIGGERY_OPENCODE_SESSION), no
// session is made, and one that opencode no longer has fails the start.
func TestOpencodeResume(t *testing.T) {
	e := newOpencodeDriver(t, DefaultOpencodeProfile, "", "")
	ctx := context.Background()
	proc, err := e.d.Start(ctx, core.Spec{ParticipantID: "p1", RunID: "r1", Cwd: e.dir, HarnessRef: "ses_resume", Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.d.Stop(ctx, "p1") })
	env := e.fakeLog(t)[0]["env"].(map[string]any)
	if proc.HarnessRef != "ses_resume" || env["PIGGERY_OPENCODE_SESSION"] != "ses_resume" || len(e.requests(t, "POST /session")) != 0 {
		t.Errorf("ref %q env %v: a resumed session is bound by id and not created again", proc.HarnessRef, env)
	}
	if _, err := e.d.Start(ctx, core.Spec{ParticipantID: "p2", RunID: "r2", Cwd: e.dir, HarnessRef: "ses_gone", Resume: true}); err == nil ||
		!strings.Contains(err.Error(), "cannot resume session ses_gone") {
		t.Fatalf("a missing session: %v", err)
	}
	if _, err := e.d.live("p2"); err == nil {
		t.Error("a worker whose start failed is still running")
	}
}

// The models list is GET /config/providers in opencode's order; set_model refuses what it does not
// offer before anything is sent, and a thinking level the model has no variant for. A switch is a
// prompt with noReply whose only part is synthetic and ignored; asked while the session is busy it
// lands at the next idle.
func TestOpencodeModels(t *testing.T) {
	e := newOpencodeDriver(t, DefaultOpencodeProfile, "", "")
	ctx := context.Background()
	if _, err := e.d.Start(ctx, core.Spec{ParticipantID: "p1", RunID: "r1", Cwd: e.dir, HarnessRef: "x", Model: "hp/alt", Thinking: "low"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.d.Stop(ctx, "p1") })
	models, err := e.d.Models(ctx, "p1")
	if err != nil || len(models) < 3 || !slices.Contains(models, "hp/glm-5.3-flash") || !slices.Contains(models, "hp/alt") ||
		slices.Index(models, "hp/glm-5.3-flash") > slices.Index(models, "hp/alt") {
		t.Fatalf("models %v, %v", models, err)
	}
	if err := e.d.SetModel(ctx, "p1", "hp/nonexistent"); err == nil || !strings.Contains(err.Error(), "does not offer") {
		t.Fatalf("unknown model: %v", err)
	}
	if err := e.d.SetModel(ctx, "p1", "nomodel"); err == nil {
		t.Fatal("a model without a provider was accepted")
	}
	if err := e.d.SetThinking(ctx, "p1", "max"); err == nil || !strings.Contains(err.Error(), "variants: low, high") {
		t.Fatalf("a level the model does not run: %v", err)
	}
	if n := len(e.requests(t, "POST /session/ses_fake1/prompt_async")); n != 0 {
		t.Fatalf("%d prompts were sent for refused changes", n)
	}
	if err := e.d.SetThinking(ctx, "p1", "high"); err != nil {
		t.Fatal(err)
	}
	sw := e.requests(t, "POST /session/ses_fake1/prompt_async")
	b, _ := json.Marshal(sw[0]["body"])
	if len(sw) != 1 || string(b) != `{"model":{"modelID":"alt","providerID":"hp"},"noReply":true,"parts":[{"ignored":true,"synthetic":true,"text":"piggery: model switch","type":"text"}],"variant":"high"}` {
		t.Fatalf("switch %s", b)
	}
	// busy: accepted, applied at the idle
	emit := func(status string) {
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%s/_emit", e.port(t)), "application/json",
			strings.NewReader(`{"type":"session.status","properties":{"sessionID":"ses_fake1","status":{"type":"`+status+`"}}}`))
		if err == nil {
			resp.Body.Close()
		}
	}
	emit("busy")
	waitFor(t, "busy", func() bool {
		r := e.d.codec.(*opencodeCodec).run(e.d.procs["p1"])
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.busy
	})
	if err := e.d.SetModel(ctx, "p1", "hp/glm-5.3-flash"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(e.requests(t, "POST /session/ses_fake1/prompt_async")); n != 1 {
		t.Fatalf("a switch was sent while busy (%d prompts)", n)
	}
	emit("idle")
	waitFor(t, "the switch at idle", func() bool { return len(e.requests(t, "POST /session/ses_fake1/prompt_async")) == 2 })
	b, _ = json.Marshal(e.requests(t, "POST /session/ses_fake1/prompt_async")[1]["body"])
	if !strings.Contains(string(b), `"modelID":"glm-5.3-flash"`) || strings.Contains(string(b), "variant") {
		t.Errorf("second switch %s: a model change clears the variant", b)
	}
}

// port is the port the fake serve listens on, from its command line.
func (e ocEnv) port(t *testing.T) string {
	t.Helper()
	args := e.fakeLog(t)[0]["args"].([]any)
	for i, a := range args {
		if a == "--port" {
			return args[i+1].(string)
		}
	}
	t.Fatal("no --port")
	return ""
}

// The records the driver makes from the SSE captures are the contract with the plugin's own
// (testdata/fixtures/opencode/1.18.34/records-golden/<capture>.jsonl; PGOLDEN=1 rewrites them from
// opencodeRun.standard, review the diff): a plain turn and a sync one (03), a tool call with a
// steered-in message (04), an abort in a tool call with a queued message and a later turn (05), a
// model that does not exist (08b), a permission that was asked (09d). The worker's session is the
// one of the capture's first message.updated (03 made another session first, from the create whose
// id opencode ignored).
// Part deltas and instance-start events are dropped; a turn ends at the busy to idle edge only, the
// duplicate idles of an abort do not repeat it, and an error's stack-trace twin is skipped.
func TestOpencodeRecordsGolden(t *testing.T) {
	for _, c := range []string{"03-prompt", "04-steer", "05-abort", "08b-bad-model", "09d-permission-ask"} {
		t.Run(c, func(t *testing.T) {
			f, err := os.Open(ocFixtures + "serve/" + c + ".sse.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var sid string
			var events [][]byte
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 16<<20)
			for sc.Scan() {
				var l struct{ Data json.RawMessage }
				if json.Unmarshal(sc.Bytes(), &l) != nil || len(l.Data) == 0 {
					continue
				}
				var p struct {
					Type       string
					Properties struct{ SessionID string }
				}
				json.Unmarshal(l.Data, &p)
				if sid == "" && p.Type == "message.updated" {
					sid = p.Properties.SessionID
				}
				events = append(events, l.Data)
			}
			r := &opencodeRun{sessionID: sid, roles: map[string]string{}, seen: map[string]bool{}}
			var got bytes.Buffer
			for _, ev := range events {
				for _, rec := range r.standard(ev) {
					got.Write(append(rec, '\n'))
				}
			}
			golden := ocFixtures + "records-golden/" + c + ".jsonl"
			if os.Getenv("PGOLDEN") != "" {
				os.MkdirAll(filepath.Dir(golden), 0o755)
				if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Errorf("records differ from %s:\n%s", golden, got.String())
			}
			if got.Len() == 0 || bytes.Contains(got.Bytes(), []byte("delta")) {
				t.Errorf("records: %d bytes", got.Len())
			}
		})
	}
}

// The model never sees a model switch. Capture 11: the same body the driver sends (noReply, an
// ignored synthetic text, a model and variant) set the session to alt/high, made no model request,
// and the next prompt's LLM request carried reasoning_effort high and no trace of the switch text
// (only the system prompt and that prompt: the switch's user message is not in it).
func TestOpencodeSwitchInvisible(t *testing.T) {
	script := readFile(ocFixtures + "serve/11-ignored-switch.sh")
	r := &opencodeRun{}
	r.l.cwd = t.TempDir()
	var sent []byte
	srv := http.NewServeMux()
	srv.HandleFunc("/session/s1/prompt_async", func(w http.ResponseWriter, req *http.Request) {
		sent, _ = io.ReadAll(req.Body)
		w.WriteHeader(204)
	})
	ts := newLocalServer(t, srv)
	r.base, r.sessionID = ts, "s1"
	if err := r.sendSwitch(context.Background(), opencodeSwitch{model: "hp/alt", variant: "high"}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		NoReply bool              `json:"noReply"`
		Parts   []map[string]any  `json:"parts"`
		Model   map[string]string `json:"model"`
		Variant string            `json:"variant"`
	}
	json.Unmarshal(sent, &body)
	if !body.NoReply || len(body.Parts) != 1 || body.Parts[0]["ignored"] != true || body.Parts[0]["synthetic"] != true ||
		!strings.Contains(script, `"parts":[{"type":"text","text":"piggery: model switch","synthetic":true,"ignored":true}]`) {
		t.Errorf("switch body %s differs from the captured one", sent)
	}
	var reqs []struct {
		NeedleFound     bool   `json:"needle_found"`
		ReasoningEffort string `json:"reasoning_effort"`
		Messages        []struct {
			Role  string
			Chars int
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(readFile(ocFixtures+"serve/11-ignored-switch.proxy.jsonl")), "\n") {
		var q struct {
			NeedleFound     bool   `json:"needle_found"`
			ReasoningEffort string `json:"reasoning_effort"`
			Messages        []struct {
				Role  string
				Chars int
			}
		}
		json.Unmarshal([]byte(l), &q)
		reqs = append(reqs, q)
	}
	if len(reqs) == 0 {
		t.Fatal("no captured LLM request")
	}
	for i, q := range reqs {
		if q.NeedleFound || q.ReasoningEffort != "high" || len(q.Messages) != 2 || q.Messages[0].Role != "system" || q.Messages[1].Role != "user" {
			t.Errorf("request %d: %+v", i, q)
		}
	}
	if !strings.Contains(readFile(ocFixtures+"serve/11-ignored-switch.txt"), "session model: {'id': 'alt', 'providerID': 'hp', 'variant': 'high'}") {
		t.Error("the capture does not show the session model changed by the switch")
	}
}

// newLocalServer serves h on a loopback port and returns its base URL.
func newLocalServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
