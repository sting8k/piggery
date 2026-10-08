package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

// The omp tests replay real omp 18.4.2 captures (testdata/fixtures/omp/, README there): the fake
// omp answers as the capture did and takes its clamping of thinking levels from it.
const ompFixtures = "../../../testdata/fixtures/omp/"

func ompCapture(dir, name string) [][]byte {
	b, err := os.ReadFile(filepath.Join(dir, "omp-18.4.2-"+name))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
}

// ompHelper is the fake omp (TestHelperProcess modes "omp" and "omp-fail"):
//   - omp: report env, args and the --config file, then the frames omp sent at startup (ready,
//     setWidget, advisor_cost_changed, available_commands_update), then answer each command with
//     the response the capture has for it: get_state (sessionId of a --resume, else the
//     captured one; the level omp ran), set_model (refused unless HP/glm-5.3-flash),
//     set_thinking_level (accepted, the level kept as the capture shows omp clamped it; at
//     launch, as probes.json shows for `--thinking bogus` and `minimal`).
//   - omp-fail: what omp wrote when it exited before `ready` (unknown session id), exit 1.
func ompHelper(mode string) {
	dir := os.Getenv("PGDRV_OMP_FIXTURES")
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	if mode == "omp-fail" {
		var errs []struct{ Stderr string }
		b, _ := os.ReadFile(filepath.Join(dir, "omp-18.4.2-launch-errors.json"))
		json.Unmarshal(b, &errs)
		fmt.Fprint(os.Stderr, errs[2].Stderr)
		os.Exit(1)
	}
	var resume, config, model, thinking string
	for i, a := range os.Args {
		if i+1 < len(os.Args) {
			switch a {
			case "--resume":
				resume = os.Args[i+1]
			case "--config":
				config = os.Args[i+1]
			case "--model":
				model = os.Args[i+1]
			case "--thinking":
				thinking = os.Args[i+1]
			}
		}
	}
	overlay, _ := os.ReadFile(config)
	agentDir := os.Getenv("PI_CODING_AGENT_DIR")
	_, agentDirErr := os.Stat(agentDir)
	profile := map[string]string{}
	for _, k := range []string{"OMP_PROFILE", "PI_PROFILE"} {
		v, set := os.LookupEnv(k)
		profile[k] = fmt.Sprintf("%v:%q", set, v)
	}
	emit(map[string]any{"type": "probe_env", "args": os.Args, "agent_dir": agentDir, "agent_dir_exists": agentDirErr == nil,
		"overlay": string(overlay), "profile": profile, "id": os.Getenv("PIGGERY_ID"), "model": model})

	// What the capture shows: the startup frames, the responses by id, and the level each
	// set_thinking_level left (t-<level> answered, then g-<level> reports it).
	var byID = map[string]map[string]any{}
	for i, line := range ompCapture(dir, "rpc-control.jsonl") {
		var f map[string]any
		json.Unmarshal(line, &f)
		if f["type"] == "response" {
			byID[f["id"].(string)] = f
		} else if len(byID) == 0 {
			fmt.Println(string(line)) // before the first response: the startup frames
		}
		_ = i
	}
	level := func(id string) string {
		l, _ := byID[id]["data"].(map[string]any)["thinkingLevel"].(string)
		return l
	}
	clamp := map[string]string{}
	for _, lv := range []string{"high", "bogus", "off", "minimal", "xhigh", "max", "medium"} {
		clamp[lv] = level("g-" + lv)
	}
	// omp at launch (probes.json, launch_flags: one run per --thinking): what get_state said.
	var probes struct {
		LaunchFlags map[string]struct {
			GetState struct{ ThinkingLevel string } `json:"get_state"`
		} `json:"launch_flags"`
	}
	pb, _ := os.ReadFile(filepath.Join(dir, "omp-18.4.2-probes.json"))
	json.Unmarshal(pb, &probes)
	cur := level("g0")
	if thinking != "" {
		cur = thinking // a level the captures did not clamp runs as given
		if v, ok := clamp[thinking]; ok {
			cur = v
		}
		if p, ok := probes.LaunchFlags["thinking-"+thinking]; ok {
			cur = p.GetState.ThinkingLevel
		}
	}
	var afterSetModel string
	for _, line := range ompCapture(dir, "rpc-set-model-resets-thinking.jsonl") {
		var f struct {
			Type          string
			ThinkingLevel string
		}
		json.Unmarshal(line, &f)
		if f.Type == "thinking_level_changed" {
			afterSetModel = f.ThinkingLevel // the last one: what set_model left
		}
	}
	state := byID["g0"]["data"].(map[string]any)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var cmd struct{ ID, Type, ModelId, Level string }
		json.Unmarshal(in.Bytes(), &cmd)
		var resp map[string]any
		switch cmd.Type {
		case "get_state":
			d := map[string]any{}
			for k, v := range state {
				d[k] = v
			}
			d["thinkingLevel"] = cur
			if resume != "" {
				d["sessionId"] = resume
			}
			resp = map[string]any{"type": "response", "command": "get_state", "success": true, "data": d}
		case "set_model":
			resp = cloneMap(byID["m2"])
			if cmd.ModelId == "glm-5.3-flash" {
				resp = cloneMap(byID["m1"])
				cur = afterSetModel // omp resets the level (rpc-set-model-resets-thinking)
			}
		case "get_available_models": // the capture (omp 18.4.2), under this request's id
			json.Unmarshal(ompCapture(dir, "get-available-models.jsonl")[0], &resp)
		case "set_thinking_level":
			cur = cmd.Level
			if v, ok := clamp[cmd.Level]; ok {
				cur = v
			}
			resp = cloneMap(byID["t-high"])
		default:
			resp = map[string]any{"type": "response", "command": cmd.Type, "success": true}
		}
		resp["id"] = cmd.ID
		emit(resp)
	}
	os.Exit(0)
}

func cloneMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// newOmpDriver is a driver whose omp is the fake one.
func newOmpDriver(t *testing.T, mode string, opts Options) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	prof, _ := json.Marshal(Profile{Cmd: os.Args[0], Args: []string{"-test.run=^TestHelperProcess$", "--"}, Model: "HP/glm-5.3-flash"})
	if err := os.MkdirAll(filepath.Dir(OmpProfilePath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(OmpProfilePath(dir), prof, 0o600); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(ompFixtures)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGDRV_HELPER", mode)
	t.Setenv("PGDRV_OMP_FIXTURES", abs)
	t.Setenv("PIGGERY_DISABLED", "1")
	setHome(t, t.TempDir())
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("OMP_PROFILE", "work") // a named profile of the daemon's own: it must not reach the worker
	t.Setenv("PI_PROFILE", "work")
	return NewOmp(dir, opts), dir
}

func probeEnv(t *testing.T, d *Driver, pid string) map[string]any {
	t.Helper()
	rec, _ := waitRecord(t, d, pid, "probe_env")
	return rec
}

// An omp worker: no --session-id (omp has none), the session id it reports in get_state is the
// worker's harness_ref, and a resume passes it back as --resume; sessions go to piggery's own
// --session-dir, which outlives the run's agent dir; the agent dir holds only the settings
// filter, an empty extensions/ and the overlay that keeps the project's .mcp.json servers off;
// the worker's env has no named profile; the ready frame is in the log first.
func TestOmpStartAndResume(t *testing.T) {
	d, dir := newOmpDriver(t, "omp", Options{})
	ctx := context.Background()
	spec := core.Spec{ParticipantID: "w1", RunID: "r1", Token: "tok", Cwd: dir, HarnessRef: "minted-by-core", Thinking: "low"}
	proc, err := d.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	var st struct{ SessionID string }
	c1 := ompCapture(ompFixturesAbs(t), "rpc-control.jsonl")
	for _, l := range c1 {
		if strings.Contains(string(l), `"command":"get_state"`) {
			var f struct{ Data struct{ SessionId string } }
			json.Unmarshal(l, &f)
			st.SessionID = f.Data.SessionId
			break
		}
	}
	if st.SessionID == "" || proc.HarnessRef != st.SessionID {
		t.Fatalf("harness_ref %q; want the session id get_state reported (%q)", proc.HarnessRef, st.SessionID)
	}
	for _, bad := range []string{"--session-id", "--resume", "--no-extensions"} {
		if slices.Contains(proc.Cmdline, bad) {
			t.Errorf("a new omp worker got %s: %v", bad, proc.Cmdline)
		}
	}
	env := probeEnv(t, d, "w1")
	sessions := OmpSessionsDir(dir, "w1")
	args := fmt.Sprint(env["args"])
	if !strings.Contains(args, "--session-dir "+sessions) || !strings.Contains(args, "--thinking low") || !strings.Contains(args, "--model HP/glm-5.3-flash") ||
		!strings.Contains(args, "-e "+filepath.Join(OmpWorkerExtDir(dir), "omp", "index.ts")) {
		t.Errorf("args %s; want --session-dir %s, --thinking low, --model, and -e the worker's extension", args, sessions)
	}
	if env["agent_dir"] != filepath.Join(OmpAgentDirRoot(dir), "w1", "r1") || env["agent_dir_exists"] != true {
		t.Errorf("agent dir %v (exists %v); want the run's own", env["agent_dir"], env["agent_dir_exists"])
	}
	if env["overlay"] != ompWorkerConfig || !strings.Contains(ompWorkerConfig, "enableProjectConfig: false") {
		t.Errorf("--config overlay %q", env["overlay"])
	}
	if p := fmt.Sprint(env["profile"]); p != `map[OMP_PROFILE:true:"" PI_PROFILE:true:""]` {
		t.Errorf("profile env %s; want both empty", p)
	}
	recs, _ := d.Tail("w1", 0)
	var first struct{ Type string }
	if json.Unmarshal(recs[0], &first); first.Type != "probe_env" || !bytes.Contains(recs[1], []byte(`"type":"ready"`)) {
		t.Errorf("log starts %s, %s; want the probe, then omp's ready frame", recs[0], recs[1])
	}

	d.Stop(ctx, "w1")
	if _, err := os.Stat(filepath.Join(OmpAgentDirRoot(dir), "w1", "r1")); !os.IsNotExist(err) {
		t.Errorf("the run's agent dir outlived the run: %v", err)
	}
	if fi, err := os.Stat(sessions); err != nil || !fi.IsDir() {
		t.Errorf("the session dir is gone with the run: %v", err)
	}

	spec.RunID, spec.Resume, spec.HarnessRef = "r2", true, proc.HarnessRef
	proc2, err := d.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(proc2.Cmdline, "--resume"); i < 0 || proc2.Cmdline[i+1] != st.SessionID || proc2.HarnessRef != st.SessionID {
		t.Errorf("resume: %v, harness_ref %q; want --resume %s and the same ref", proc2.Cmdline, proc2.HarnessRef, st.SessionID)
	}
	d.Stop(ctx, "w1")
}

func ompFixturesAbs(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(ompFixtures)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// omp clamps a thinking level it does not have and says nothing (`minimal` ran low, `--thinking
// bogus` ran max, set_thinking_level bogus leaves none): start with a level omp does not run
// fails, and a live set that omp does not keep is refused with the level it had set again.
func TestOmpThinkingLevelIsChecked(t *testing.T) {
	d, dir := newOmpDriver(t, "omp", Options{})
	ctx := context.Background()
	spec := func(id, level string) core.Spec {
		return core.Spec{ParticipantID: id, RunID: "r-" + id, Token: "tok", Cwd: dir, HarnessRef: "x", Thinking: level}
	}
	if _, err := d.Start(ctx, spec("p1", "minimal")); err == nil || !strings.Contains(err.Error(), `omp ran "low", not "minimal"`) {
		t.Fatalf("start with a level omp clamps: %v", err)
	}
	if err := d.Abort("p1"); err == nil {
		t.Fatal("the refused worker is still running")
	}
	if _, err := d.Start(ctx, spec("p2", "max")); err != nil {
		t.Fatalf("start with max: %v", err)
	}
	if err := d.SetThinking(ctx, "p2", "off"); err != nil {
		t.Fatalf("set off: %v", err)
	}
	if err := d.SetThinking(ctx, "p2", "medium"); err == nil || !strings.Contains(err.Error(), `omp ran "low", not "medium"`) {
		t.Fatalf("set medium (omp runs low): %v; want refused", err)
	}
	w, _ := d.live("p2")
	if ran, err := w.thinking(ctx, commandTimeout); err != nil || ran != "off" {
		t.Fatalf("after the refused set omp runs %q, %v; want off back", ran, err)
	}
	if err := d.SetModel(ctx, "p2", "HP/no-such-model"); err == nil || !strings.Contains(err.Error(), "Model not found") {
		t.Fatalf("set_model omp refused: %v", err)
	}
	// omp's set_model resets the level to max (seen live: a worker running off ran max after a
	// model change while ps said off): the level the worker ran is set again.
	if err := d.SetModel(ctx, "p2", "HP/glm-5.3-flash"); err != nil {
		t.Fatalf("set_model: %v", err)
	}
	if ran, err := w.thinking(ctx, commandTimeout); err != nil || ran != "off" {
		t.Fatalf("after set_model omp runs %q, %v; want off", ran, err)
	}
	// omp answers get_available_models as pi does: its codec is pi's.
	if got, err := d.Models(ctx, "p2"); err != nil || !slices.Equal(got, []string{"example/glm-5.3-flash", "example/glm-5.3-pro"}) {
		t.Fatalf("models = %v, %v", got, err)
	}
	d.Stop(ctx, "p2")
}

// omp exits before `ready` on an unknown session id: the run does not start, and the error says
// what omp wrote on stderr.
func TestOmpStartFailureCarriesStderr(t *testing.T) {
	d, dir := newOmpDriver(t, "omp-fail", Options{})
	_, err := d.Start(context.Background(), core.Spec{ParticipantID: "p3", RunID: "r3", Token: "tok", Cwd: dir, HarnessRef: "ffffffff-0000-0000-0000-000000000000", Resume: true})
	if err == nil || !strings.Contains(err.Error(), `Session "ffffffff-0000-0000-0000-000000000000" not found`) {
		t.Fatalf("start: %v; want omp's stderr in it", err)
	}
}

// What the log keeps of a real omp run (the steer capture: a steer while bash runs, then the
// job's result starts a second run): pi's records without the streaming deltas, omp's command
// list and advisor ticks, the agent_end that is not the run's end (isTerminal false) and the
// answer to get_state (12 KB of prompt and tool schemas); the requests waiting for a response are
// answered all the same.
func TestOmpRecords(t *testing.T) {
	var c ompCodec
	kept := map[string]int{}
	var answered []string
	for _, line := range ompCapture(ompFixturesAbs(t), "rpc-steer.jsonl") {
		r := c.record(nil, line)
		var f struct{ Type string }
		json.Unmarshal(line, &f)
		if r.keep {
			kept[f.Type]++
		}
		if r.answers != "" {
			answered = append(answered, r.answers)
		}
	}
	for _, typ := range []string{"message_update", "available_commands_update", "advisor_cost_changed"} {
		if kept[typ] != 0 {
			t.Errorf("%d %s records kept", kept[typ], typ)
		}
	}
	if kept["agent_end"] != 1 || kept["turn_end"] != 3 || kept["message_end"] != 7 || kept["tool_execution_end"] != 1 || kept["prompt_result"] != 1 || kept["response"] != 2 {
		t.Errorf("kept %v; want one agent_end (the run's end), 3 turn_end, 7 message_end, a tool result, the prompt_result and 2 responses (prompt, steer: not get_state)", kept)
	}
	if !slices.Equal(answered, []string{"p1", "s1", "g1"}) {
		t.Errorf("answered %v; want the prompt, the steer and get_state", answered)
	}
}
