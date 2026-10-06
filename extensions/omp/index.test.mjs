// Loads the real omp extension (index.ts, types stripped by node) against a fake daemon on a unix
// socket and a fake omp API, and replays the event captures of testdata/fixtures/omp/ into its
// handlers: what the daemon is told (harness.event, presence) and what is shown to the model.
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync } from "node:fs";
import { register } from "node:module";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fakeDaemonPath, setHome } from "../testutil/daemon.mjs";

// pi-ai is only used for the tools' parameter schemas; Unsafe keeps tools.json's schema as is.
const piAi = "export const Type = new Proxy({}, { get: (_, k) => (k === 'Unsafe' ? (s) => s : (...a) => ({ a })) });";
register(
	"data:text/javascript," +
		encodeURIComponent(`export async function resolve(s, c, next) {
			if (s === "@earendil-works/pi-ai") return { url: "data:text/javascript," + ${JSON.stringify(encodeURIComponent(piAi))}, shortCircuit: true };
			return next(s, c);
		}`),
);

const until = async (pred, ms = 5000) => {
	for (const end = Date.now() + ms; !pred(); ) {
		if (Date.now() > end) throw new Error("timeout");
		await new Promise((r) => setTimeout(r, 10));
	}
};
const fixture = (name) =>
	readFileSync(new URL(`../../testdata/fixtures/omp/${name}.jsonl`, import.meta.url), "utf8")
		.trim()
		.split("\n")
		.map((l) => JSON.parse(l));

test("the omp extension against a fake daemon", async (t) => {
	setHome(mkdtempSync(join(tmpdir(), "pgomp")));
	delete process.env.PIGGERY_ID;
	delete process.env.PIGGERY_TOKEN;
	delete process.env.PIGGERY_DISABLED;

	// Fake daemon: join.auto and identify place the session in a team (role card "TEAM CARD");
	// harness.event answers come from `script.event`.
	const calls = [];
	const script = { event: () => ({}) };
	const socks = new Set();
	const answer = (f) => {
		calls.push({ verb: f.verb, as: f.auth?.id, args: f.args });
		if (f.verb === "join.auto") return { id: "p1", token: "t1", run_id: "r1" };
		if (f.verb === "identify")
			return { team_id: "T", name: "lead", role: "peer", tools: ["send", "inbox", "who", "agent"], role_card: "TEAM CARD", protocol_version: 1, run_id: f.args.run_id };
		if (f.verb === "harness.event") return script.event(f.args) ?? {};
		return {};
	};
	const srv = net.createServer((s) => {
		s.on("error", () => {}); // a client that has gone makes a write fail (EPIPE on a Windows pipe): not the fake's concern
		socks.add(s);
		s.on("close", () => socks.delete(s));
		let buf = "";
		s.on("data", (d) => {
			buf += d;
			for (let i; (i = buf.indexOf("\n")) >= 0; ) {
				const f = JSON.parse(buf.slice(0, i));
				buf = buf.slice(i + 1);
				s.write(JSON.stringify({ id: f.id, result: answer(f) }) + "\n");
			}
		});
	});
	await new Promise((r) => srv.listen(fakeDaemonPath(process.env.HOME), r));
	t.after(() => srv.close());
	const push = (frame) => socks.forEach((s) => s.write(JSON.stringify(frame) + "\n"));

	// Fake omp API: what the extension registers, and what it shows the model.
	const tools = {};
	const handlers = {};
	const sent = [];
	let active = [];
	let level = "off";
	const omp = new Proxy(
		{
			registerTool: (d) => (tools[d.name] = d),
			on: (e, fn) => (handlers[e] = fn),
			getActiveTools: () => active,
			setActiveTools: (a) => (active = a),
			getThinkingLevel: () => level,
			sendUserMessage: (text, opts) => sent.push({ text, opts }),
		},
		{ get: (o, k) => o[k] ?? (() => {}) },
	);
	const glm = { provider: "HP", id: "glm-5.3-flash" };
	let idle = false;
	let pending = false;
	const ctx = {
		mode: "rpc",
		hasUI: false,
		cwd: process.env.HOME,
		model: glm,
		isIdle: () => idle,
		hasPendingMessages: () => pending,
		abort: () => {},
		sessionManager: { getSessionId: () => "sess-1", getSessionFile: () => "/s/sess-1.jsonl" },
	};
	const { default: piggery } = await import("./index.ts");
	piggery(omp);
	handlers.session_start({}, ctx);
	await until(() => calls.some((c) => c.verb === "identify"));
	const turns = globalThis.__piggeryOmp.turns;

	// What the daemon was told since the last reset, as `event[:outcome]` and `presence:<event>`.
	const told = () =>
		calls
			.filter((c) => c.verb === "harness.event" || c.verb === "presence")
			.map((c) => (c.verb === "presence" ? "presence:" + c.args.event : c.args.event + (c.args.outcome ? ":" + c.args.outcome : "")));
	const reset = () => {
		calls.length = 0;
		sent.length = 0;
		script.event = () => ({});
	};

	// Replays a capture into the handlers, one event at a time; the ctx answers as it did then. An
	// agent_end carries the last message the capture saw (else the last assistant message_end).
	const replay = async (name, event) => {
		reset();
		if (event) script.event = event; // the daemon's answers, set after the reset
		let last = { role: "assistant", stopReason: "stop" };
		for (const r of fixture(name)) {
			idle = r.ctx?.idle ?? false;
			pending = r.ctx?.pending ?? false;
			if (r.ev === "message_end" && r.role === "assistant") last = { role: "assistant", stopReason: r.stopReason };
			if (r.ev === "before_agent_start") await handlers.before_agent_start({ prompt: "p", systemPrompt: ["BASE"] }, ctx);
			if (r.ev === "agent_start") handlers.agent_start({}, ctx);
			if (r.ev === "turn_end") handlers.turn_end({ turnIndex: r.turnIndex }, ctx);
			if (r.ev === "session_stop") await handlers.session_stop({ last_assistant_message: last }, ctx);
			if (r.ev === "agent_end") {
				const end = r.lastRole ? { role: r.lastRole, stopReason: r.lastStop } : last;
				await handlers.agent_end({ willContinue: r.willContinue, messages: [{ role: "user" }, end] }, ctx);
			}
			await turns.drain();
		}
	};

	await t.test("it joins as omp with omp's session file, and registers tools.json's tools as essential", () => {
		const join = calls.find((c) => c.verb === "join.auto").args;
		assert.equal(join.harness, "omp");
		assert.deepEqual(join.transcript, { path: "/s/sess-1.jsonl", format: "pi" }); // omp's file is pi's format
		const id = calls.find((c) => c.verb === "identify").args;
		assert.deepEqual([id.model, id.thinking, id.protocol_version], ["HP/glm-5.3-flash", "off", 1]);
		assert.deepEqual(id.capabilities, ["abort", "wake", "steer", "system_prompt"]);
		const file = JSON.parse(readFileSync(new URL("../pi/tools.json", import.meta.url), "utf8")).tools;
		assert.deepEqual(Object.keys(tools).sort(), file.map((f) => "piggery_" + f.name).sort());
		for (const d of Object.values(tools)) assert.equal(d.loadMode, "essential", d.name); // else the model needs write xd://
		assert.deepEqual([...active].sort(), Object.keys(tools).sort());
	});

	await t.test("the role card goes in the system prompt, and a model or level change is reported once", async () => {
		reset();
		const r = await handlers.before_agent_start({ prompt: "p", systemPrompt: ["BASE"] }, ctx);
		assert.deepEqual(r, { systemPrompt: ["BASE", "TEAM CARD"] }); // omp's systemPrompt is a list of parts
		assert.deepEqual(told(), []); // nothing changed
		ctx.model = { provider: "HP", id: "kimi-k3" };
		level = "minimal";
		await handlers.before_agent_start({ prompt: "p", systemPrompt: [] }, ctx);
		await handlers.before_agent_start({ prompt: "p", systemPrompt: [] }, ctx);
		await until(() => told().length === 1);
		assert.deepEqual(calls.filter((c) => c.verb === "harness.event").map((c) => c.args), [
			{ event: "model_changed", model: "HP/kimi-k3", thinking: "minimal" },
		]);
		ctx.model = glm;
		level = "off";
		await handlers.before_agent_start({ prompt: "p", systemPrompt: [] }, ctx);
		await until(() => told().length === 2);
	});

	await t.test("a plain turn: start, tool boundary, ok end at session_stop, then settled", async () => {
		await replay("events-normal-rpc");
		assert.deepEqual(told(), ["turn_start", "tool_boundary", "turn_end:ok", "presence:agent_settled"]);
		assert.deepEqual(sent, []);
	});

	await t.test("mail at session_stop is steered in and both runs are one turn, acked only at the last end", async () => {
		const mails = ["MAIL-ONE", "MAIL-TWO"];
		// the capture: session_stop, an agent_end that only looks settled (a message is pending), a new
		// run, twice; then the real end
		await replay("events-steer-rpc-two-mails", (a) =>
			a.event === "turn_end" && a.outcome === "ok" && mails.length ? { block: true, text: mails.shift() } : {},
		);
		assert.deepEqual(told().filter((e) => e !== "tool_boundary"), ["turn_start", "turn_end:ok", "turn_end:ok", "turn_end:ok", "presence:agent_settled"]);
		assert.equal(told().filter((e) => e === "turn_start").length, 1, "one turn across the runs");
		const keys = new Set(calls.filter((c) => c.args?.prompt_id).map((c) => c.args.prompt_id));
		assert.equal(keys.size, 1);
		assert.deepEqual(sent, [
			{ text: "MAIL-ONE", opts: { deliverAs: "steer" } },
			{ text: "MAIL-TWO", opts: { deliverAs: "steer" } },
		]); // sendUserMessage: shown in the TUI, never a hidden block
		assert.equal(told().at(-1), "presence:agent_settled");
	});

	await t.test("an abort ends the turn interrupted (never acked), in a tool and while streaming", async () => {
		for (const name of ["events-abort-in-tool-rpc", "events-abort-while-streaming-rpc"]) {
			// a daemon that would hand mail back at an ok end: an abort must not ask it
			await replay(name, (a) => (a.event === "turn_end" && a.outcome === "ok" ? { block: true, text: "must not be given" } : {}));
			assert.deepEqual(told().filter((e) => e !== "tool_boundary"), ["turn_start", "turn_end:interrupted", "presence:agent_settled"], name);
			assert.deepEqual(sent, [], name);
		}
	});

	await t.test("no session_stop (compaction dead end): the turn ends ok at agent_end, every time", async () => {
		await replay("events-compaction-no-session-stop");
		const prompts = fixture("events-compaction-no-session-stop").filter((r) => r.ev === "agent_end").length;
		assert.ok(prompts > 1);
		const ends = told().filter((e) => e.startsWith("turn_end"));
		assert.deepEqual(ends, Array(prompts).fill("turn_end:ok"));
		assert.equal(told().filter((e) => e === "turn_start").length, prompts);
		assert.equal(told().filter((e) => e === "presence:agent_settled").length, prompts);
	});

	await t.test("a retry chain (agent_start before agent_end with willContinue) is one turn, settled once", async () => {
		await replay("events-empty-output-recovery-retry");
		assert.deepEqual(told().filter((e) => e !== "tool_boundary"), ["turn_start", "turn_end:ok", "presence:agent_settled"]);
	});

	await t.test("a failed run ends failed, not acked", async () => {
		reset();
		handlers.agent_start({}, ctx);
		await handlers.session_stop({ last_assistant_message: { role: "assistant", stopReason: "error" } }, ctx);
		pending = false;
		await handlers.agent_end({ willContinue: undefined, messages: [{ role: "assistant", stopReason: "error" }] }, ctx);
		await turns.drain();
		assert.deepEqual(told(), ["turn_start", "turn_end:failed", "presence:agent_settled"]);
	});

	await t.test("mail for an idle session starts a turn with a visible message; mail during a run is steered in", async () => {
		reset();
		idle = true;
		script.event = (a) => (a.event === "turn_start" ? { text: "MAIL-IDLE" } : {});
		push({ event: "wake" });
		await until(() => sent.length === 1);
		assert.deepEqual(sent[0], { text: "MAIL-IDLE", opts: undefined }); // not a steer: omp starts a turn from it
		handlers.agent_start({}, ctx); // the run that message starts: same turn
		idle = false;
		script.event = (a) => (a.event === "tool_boundary" ? { text: "MAIL-LATE" } : {});
		push({ event: "wake" });
		await until(() => sent.length === 2);
		assert.deepEqual(sent[1], { text: "MAIL-LATE", opts: { deliverAs: "steer" } });
		script.event = () => ({});
		await handlers.session_stop({ last_assistant_message: { role: "assistant", stopReason: "stop" } }, ctx);
		await handlers.agent_end({ messages: [{ role: "assistant", stopReason: "stop" }] }, ctx);
		await turns.drain();
		assert.equal(told().at(-1), "presence:agent_settled");
	});

	await t.test("a shutdown ends the session at the daemon", async () => {
		reset();
		await handlers.session_shutdown({});
		assert.deepEqual(told(), ["session_end"]);
	});
});
