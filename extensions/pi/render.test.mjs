// The pi adapter's model-facing text and tool helpers.
import assert from "node:assert/strict";
import test from "node:test";
import { afterRetire, render, renderWho } from "./render.mjs";

test("a mail header shows #N and names, the local send time, and an age only when late", () => {
	const now = new Date(2026, 8, 27, 14, 30, 0).getTime();
	const m = { id: "01M…", seq: 1042, from: "01P…", from_name: "a1", from_label: "a1 (peer)", body: "x" };
	const fresh = { ...m, reply_to: "01R…", reply_to_seq: 1040, created_at: now - 30_000 };
	const late = { ...m, cc_of: "01O…", cc_of_seq: 1041, cc_to: "b1 (peer)", created_at: new Date(2026, 8, 26, 23, 58).getTime() };
	const [a, b] = render([fresh, late], undefined, now).split("\n\n").slice(1, 3);
	assert.match(a, /^<message id="#1042" from="a1 \(peer\)" sender="a1" reply_to="#1040" at="14:29:30">/);
	assert.match(b, /cc_of="#1041" cc_of_mail_to="b1 \(peer\)" at="2026-09-26 23:58" age="14h32m">/);
	assert.doesNotMatch(a + b, /01[A-Z]…/);
});

test("a redelivered mail says so, whatever its age", () => {
	const now = Date.now();
	const m = { id: "01M…", seq: 7, from_name: "a1", from_label: "a1 (peer)", body: "x", created_at: now - 1_000 };
	assert.match(render([{ ...m, redelivered: true }], undefined, now), / redelivered="true">/);
	assert.doesNotMatch(render([m], undefined, now), /redelivered/);
});

test("who: own team in full, one line per other team and per solo, admittable marked", () => {
	const out = renderWho(
		[
			{ kind: "member", id: "1", name: "lead", role: "peer", state: "idle", team: "web", gate: true },
			{ kind: "member", id: "2", name: "w1", role: "peer", state: "working", team: "web", gate: false },
			{ kind: "team", id: "t2", name: "api", cwd: "/r/api", gate_name: "api-lead" },
			{ kind: "solo", id: "3", name: "pi-ab12", cwd: "/r/web", state: "idle", gate: true, admittable: true },
		],
		"1",
	).split("\n");
	assert.deepEqual(out, [
		"Your team web:",
		"  lead (peer) idle [gate] (you)",
		"  w1 (peer) working",
		"Other teams (write to the team name; it reaches the gate):",
		"  api (root /r/api) gate api-lead",
		"Solo sessions (each is its own gate):",
		"  pi-ab12 (cwd /r/web) idle [admittable]",
	]);
});

test("after team down an interactive session becomes solo; a worker or any other refusal stops", () => {
	assert.equal(afterRetire({ rule_id: "team.closed" }, false), "solo");
	assert.equal(afterRetire({ rule_id: "team.closed" }, true), "stale");
	assert.equal(afterRetire({ rule_id: "run.stale" }, false), "stale");
	assert.equal(afterRetire({ code: "unauthorized", rule_id: "token.invalid" }, false), "stale");
});

