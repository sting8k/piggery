import assert from "node:assert/strict";
import { chmodSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { piggeryPath } from "../server/installed.ts";
import { pickBin, realDir, runPiggery } from "../server/piggery.ts";
import { foldsOf, goneOpen, NO_FOLDS, prune, setGone, setTab, setTeam, teamOpen } from "../shared/folds.ts";
import { RPC_NAMES, viewSettings } from "../shared/rpc.ts";
import { readView, related, sameDir, VIEW_VERSION } from "../shared/view.ts";

test("RPC names are ones Paseo accepts (one bad name fails the whole plugin)", () => {
  for (const name of RPC_NAMES) assert.match(name, /^[a-z][a-z0-9._-]*$/);
});

test("runPiggery always passes --no-start and says why piggery gave nothing", { skip: process.platform === "win32" && "its fake piggery is a sh script" }, async () => {
  const dir = mkdtempSync(join(tmpdir(), "piggery-paseo-"));
  const fake = (name: string, body: string) => {
    const path = join(dir, name);
    writeFileSync(path, `#!/bin/sh\n${body}\n`);
    chmodSync(path, 0o755);
    return path;
  };
  const echo = await runPiggery(fake("echo", 'echo "$@"'), ["ps", "--view"]);
  assert.deepEqual(echo, { ok: true, out: "ps --view --no-start\n" });

  const down = await runPiggery(fake("down", 'echo "the piggery daemon is not running" >&2; exit 1'), ["ps"]);
  assert.equal(down.ok || down.code, "down");

  const old = await runPiggery(fake("old", 'echo "ps: flag provided but not defined: -view" >&2; exit 2'), ["ps"]);
  assert.deepEqual([old.ok || old.code, old.ok ? "" : /older than this plugin/.test(old.error)], ["older", true]);

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

test("a workspace reached through a symlink matches the directory piggery recorded", { skip: process.platform === "win32" && "a symlink needs a privilege on Windows, and the plugin compares paths by /" }, async () => {
  const base = realpathSync(mkdtempSync(join(tmpdir(), "piggery-paseo-")));
  mkdirSync(join(base, "shop"));
  symlinkSync(join(base, "shop"), join(base, "link"));
  assert.equal(related(join(base, "shop"), join(base, "link")), false, "the workspace path as given does not match");
  assert.equal(related(join(base, "shop"), await realDir(join(base, "link"))), true);
  assert.equal(related("/w/api", "/w/api/web"), true, "inside it");
  assert.equal(related("/w", "/w/api"), true, "around it");
  assert.equal(related("/w/ap", "/w/api"), false, "a prefix is not a parent");
  assert.equal(await realDir(join(base, "nowhere")), join(base, "nowhere"));
});

test("a Windows directory matches its workspace whatever the separator and the case", () => {
  assert.equal(related("C:\\w\\api", "C:\\w\\api\\web"), true, "inside it");
  assert.equal(related("C:\\w", "c:/W/api"), true, "around it, written the other way");
  assert.equal(related("C:\\w\\ap", "C:\\w\\api"), false, "a prefix is not a parent");
  assert.equal(sameDir("C:\\w\\Api\\", "c:/w/api"), true);
  assert.equal(related("/w/a\\b", "/w/a/b"), false, "in a unix path a backslash is a letter of a name");
});

test("it reads ps --view of the version it knows and refuses another, saying which way", () => {
  assert.equal(readView({ version: VIEW_VERSION, summary: {} }).ok, true);
  const older = readView({ version: VIEW_VERSION - 1 });
  const newer = readView({ version: VIEW_VERSION + 1 });
  const none = readView({ all: {} });
  assert.deepEqual([older.ok || older.code, newer.ok || newer.code, none.ok || none.code], ["older", "newer", "older"]);
  assert.match(newer.ok ? "" : newer.error, /setup paseo/);
  assert.equal(readView(null).ok, false);
});

test("folds are kept as differences from piggery's defaults, restored from the host's setting, and dropped with their team", () => {
  let f = setTeam(NO_FOLDS, "live", false, true); // a live team folded
  f = setTeam(f, "dead", false, false); // the default again: nothing to keep
  f = setGone(f, "live", true);
  f = setGone(setTab(f, "board"), "vanished", true);
  f = setTeam(f, "vanished", false, true);
  assert.deepEqual(f.teams, { live: false, vanished: false });
  const saved = prune(f, ["live", "dead"]);
  assert.deepEqual(saved, { teams: { live: false }, gone: { live: true }, tab: "board" });
  const back = foldsOf(viewSettings.schema.parse(JSON.parse(JSON.stringify(saved))));
  assert.deepEqual([teamOpen(back, "live", true), teamOpen(back, "dead", false), goneOpen(back, "live"), back.tab], [false, false, true, "board"]);
  assert.deepEqual(foldsOf(viewSettings.schema.parse({})), NO_FOLDS, "a new install starts unfolded, on Overview");
  assert.equal(foldsOf(viewSettings.schema.parse({ eventsOpen: true })).tab, "overview", "an old document's keys are ignored");
});
