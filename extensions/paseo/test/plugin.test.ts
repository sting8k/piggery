import assert from "node:assert/strict";
import { chmodSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { piggeryPath } from "../server/installed.ts";
import { pickBin, realDir, runPiggery } from "../server/piggery.ts";
import { RPC_NAMES } from "../shared/rpc.ts";
import { farm, latestEvents, tailLines, tokens, type Ps } from "../shared/view.ts";

// A real `piggery ps --json`, paths made neutral.
const ps = JSON.parse(readFileSync(new URL("./fixtures/ps.json", import.meta.url), "utf8")) as Ps;

test("farm keeps piggery's grouping and order, with top's facts per row", () => {
  const view = farm(ps);
  assert.deepEqual(
    view.map((p) => [p.title, p.units.map((u) => [u.kind, u.title ?? u.rows[0].name])]),
    [
      ["labs/shop", [["solo", "vital-plain"], ["solo", "icy-oasis"]]],
      ["labs/api", [["team", "api"]]],
    ],
  );
  const team = view[1].units[0];
  assert.deepEqual([team.gate, team.held], ["", 0]);
  assert.deepEqual(
    team.rows.map((r) => [r.name, r.depth, r.role, r.harness, r.model, r.dim, r.logged]),
    [
      ["pi-1366de", 0, "planner", "pi", "glm-5.3", true, false],
      ["dev-1", 1, "dev", "pi·worker", "glm-5.3", true, true],
      ["dev-2", 1, "dev", "pi·worker", "glm-5.3", true, true],
    ],
  );
  assert.equal(team.rows[1].kind, "pi headless worker");
  assert.match(team.rows[1].came, /^spawned \d\d:\d\d by pi-1366de$/);
  const solo = view[0].units[0].rows[0];
  assert.deepEqual([solo.role, solo.kind, solo.root, solo.logged], ["solo", "solo", "/home/dev/src/labs/shop", false]);
});

test("farm for a workspace keeps the projects at, inside or around its directory", () => {
  assert.deepEqual(farm(ps, "/home/dev/src/labs/api").map((p) => p.title), ["labs/api"]);
  assert.deepEqual(farm(ps, "/home/dev/src/labs/api/web").map((p) => p.title), ["labs/api"]);
  assert.deepEqual(farm(ps, "/home/dev/src/labs").map((p) => p.title), ["labs/shop", "labs/api"]);
  assert.deepEqual(farm(ps, "/home/dev/src/labs/ap"), []);
});

test("ctx, turns, thinking and recently closed teams come from ps --json as top reads them", () => {
  const team = ps.teams![0];
  const [lead, w1] = team.members;
  const project = ps.projects![1];
  const withTop: Ps = {
    ...ps,
    teams: [{ ...team, members: team.members.map((m) => (m.id === w1.id ? { ...m, thinking: "high" } : m)) }],
    closed: [{ id: "T-old", name: "old", members: [{ ...lead, id: "M-old", name: "gone-lead" }], closed_at: 1, closed_by: "boss" }],
    projects: [
      ps.projects![0],
      {
        ...project,
        units: [
          { ...project.units[0], members: project.units[0].members!.map((m) => (m.id === w1.id ? { ...m, ctx: 12345, turns: 7 } : m)) },
          { kind: "closed", id: "T-old", members: [{ id: "M-old", depth: 0, prefix: "", cwd: "" }] },
        ],
      },
    ],
  };
  const [open, closed] = farm(withTop)[1].units;
  const worker = open.rows.find((r) => r.id === w1.id)!;
  assert.deepEqual([worker.ctx, worker.turns, worker.modelFull], ["12.3k", "7", "zai/glm-5.3 · high"]);
  assert.deepEqual([open.rows[0].ctx, open.rows[0].turns], ["", ""]);
  assert.deepEqual([closed.kind, closed.title, closed.closedBy, closed.rows.map((r) => [r.name, r.dim])], ["closed", "old", "boss", [["gone-lead", true]]]);
  assert.deepEqual([tokens(950), tokens(12345), tokens(2_500_000)], ["950", "12.3k", "2.5M"]);
});

test("events are top's: newest first, who and target by name, denied/held and exited/gone coloured", () => {
  const [lead, w1] = ps.teams![0].members;
  const events = latestEvents(
    {
      ...ps,
      events: [
        { seq: 1, ts: 1, type: "spawned", participant: lead.id, ref_id: w1.id },
        { seq: 2, ts: 2, type: "held", participant: w1.id, ref_id: "01MSGIDXYZ123456" },
        { seq: 3, ts: 3, type: "exited", participant: w1.id },
      ],
    },
    8,
  );
  assert.deepEqual(
    events.map((e) => [e.who, e.type, e.target, e.tone]),
    [
      ["dev-1", "exited", "", "danger"],
      ["dev-1", "held", "123456", "warning"],
      ["pi-1366de", "spawned", "dev-1", "muted"],
    ],
  );
  assert.equal(latestEvents(ps, 8).length, 8);
});

test("tail lines take their kind from piggery tail's prefixes", () => {
  const text = [
    "-- agent end",
    "user: [piggery] 1 new message",
    "> bash ls -la",
    "< bash: 3 files",
    "< bash error: exit 1",
    "! retrying",
    "assistant: Done.",
    "",
  ].join("\n");
  assert.deepEqual(tailLines(text), [
    { kind: "rule", text: "-- agent end" },
    { kind: "user", text: "user: [piggery] 1 new message" },
    { kind: "tool", text: "▸ bash ls -la" },
    { kind: "result", text: "✓ 3 files" },
    { kind: "error", text: "✗ bash  exit 1" },
    { kind: "warning", text: "! retrying" },
    { kind: "text", text: "Done." },
  ]);
});

test("RPC names are ones Paseo accepts (one bad name fails the whole plugin)", () => {
  for (const name of RPC_NAMES) assert.match(name, /^[a-z][a-z0-9._-]*$/);
});

test("runPiggery always passes --no-start and says why piggery gave nothing", async () => {
  const dir = mkdtempSync(join(tmpdir(), "piggery-paseo-"));
  const fake = (name: string, body: string) => {
    const path = join(dir, name);
    writeFileSync(path, `#!/bin/sh\n${body}\n`);
    chmodSync(path, 0o755);
    return path;
  };
  const echo = await runPiggery(fake("echo", 'echo "$@"'), ["ps", "--json"]);
  assert.deepEqual(echo, { ok: true, out: "ps --json --no-start\n" });

  const down = await runPiggery(fake("down", 'echo "the piggery daemon is not running" >&2; exit 1'), ["ps"]);
  assert.equal(down.ok || down.code, "down");

  const old = await runPiggery(fake("old", 'echo "ps: flag provided but not defined: -no-start" >&2; exit 2'), ["ps"]);
  assert.match(old.ok ? "" : old.error, /older than this plugin/);

  const missing = await runPiggery(join(dir, "nope"), ["ps"]);
  assert.equal(missing.ok || missing.code, "missing");

  const slow = await runPiggery(fake("slow", "sleep 5"), ["ps"], 200);
  assert.equal(slow.ok || slow.code, "failed");
  assert.match(slow.ok ? "" : slow.error, /did not answer/);
});

test("the binary is the user's setting, else the one setup installed, else piggery on PATH", () => {
  assert.equal(piggeryPath, "", "the source tree ships no machine path; setup writes it");
  assert.equal(pickBin(" /opt/piggery ", "/usr/local/bin/piggery"), "/opt/piggery");
  assert.equal(pickBin("", "/usr/local/bin/piggery"), "/usr/local/bin/piggery");
  assert.equal(pickBin("", ""), "piggery");
});

test("components take text sizes and weights from the text roles only", () => {
  const dir = new URL("../client/", import.meta.url);
  const files = readdirSync(dir, { recursive: true }).map(String).filter((f) => f.endsWith(".tsx"));
  assert.ok(files.some((f) => f.startsWith("kit")), "the kit's components are checked too");
  for (const file of files) {
    assert.doesNotMatch(readFileSync(new URL(file, dir), "utf8"), /fontSize|fontWeight/, file);
  }
});

test("a workspace reached through a symlink matches the directory piggery recorded", async () => {
  const base = realpathSync(mkdtempSync(join(tmpdir(), "piggery-paseo-")));
  mkdirSync(join(base, "shop"));
  symlinkSync(join(base, "shop"), join(base, "link"));
  const recorded: Ps = { projects: [{ label: "…/shop", path: join(base, "shop"), units: [] }] };
  assert.deepEqual(farm(recorded, join(base, "link")), [], "the workspace path as given does not match");
  assert.deepEqual(farm(recorded, await realDir(join(base, "link"))).map((p) => p.path), [join(base, "shop")]);
  assert.equal(await realDir(join(base, "nowhere")), join(base, "nowhere"));
});
