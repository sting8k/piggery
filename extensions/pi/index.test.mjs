// Loads the real extension (index.ts, types stripped by node) against a fake daemon on a unix socket.
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
		await new Promise((r) => setTimeout(r, 20));
	}
};

test("the extension against a fake daemon", async (t) => {
	setHome(mkdtempSync(join(tmpdir(), "pgext")));
	delete process.env.PIGGERY_ID;
	delete process.env.PIGGERY_TOKEN;
	delete process.env.PIGGERY_DISABLED;

	// Fake daemon: the first join.auto places the session in team t1 (p1), later ones make a solo (p2).
	const calls = [];
	const joins = [{ id: "p1", token: "t1", run_id: "r1" }, { id: "p2", token: "t2", run_id: "r2" }];
	const identity = {
		p1: { team_id: "T", name: "lead", role: "peer", tools: ["send", "inbox", "who", "agent"], role_card: "team card", protocol_version: 1 },
		p2: { team_id: "", name: "pi-1", tools: ["send", "inbox", "who", "agent"], role_card: "solo card" },
	};
	const answer = (f) => {
		calls.push({ verb: f.verb, as: f.auth?.id, args: f.args });
		if (f.verb === "join.auto") return joins.shift();
		if (f.verb === "identify") return { ...identity[f.auth.id], run_id: f.args.run_id };
		if (f.verb === "agent" && f.args.action === "close") return { team_id: "T", team_name: "t1", stopped: ["w1"] };
		if (f.verb === "agent" && f.args.action === "reopen")
			return { participant_id: "p1", token: "t1b", run_id: "r1b", team_id: "T", team_name: "t1", text: "Reopened team t1" };
		if (f.verb === "inbox") return [];
		return {};
	};
	const srv = net.createServer((s) => {
		s.on("error", () => {}); // a client that has gone makes a write fail (EPIPE on a Windows pipe): not the fake's concern
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

	const tools = {};
	const handlers = {};
	let active = [];
	let level = "xhigh"; // pi's own level: piggery passes it on as is
	const pi = new Proxy(
		{
			registerTool: (d) => (tools[d.name] = d),
			on: (e, fn) => (handlers[e] = fn),
			getActiveTools: () => active,
			setActiveTools: (a) => (active = a),
			getThinkingLevel: () => level,
		},
		{ get: (o, k) => o[k] ?? (() => {}) },
	);
	const { default: piggery } = await import("./index.ts");
	piggery(pi);
	const glm = { provider: "HP", id: "glm-5.3-flash" };
	const ctx = { mode: "rpc", hasUI: false, cwd: process.env.HOME, isIdle: () => true, model: glm, sessionManager: { getSessionId: () => "sess-1", getSessionFile: () => "/s/sess-1.jsonl" } };
	handlers.session_start({}, ctx);
	await until(() => calls.some((c) => c.verb === "identify" && c.as === "p1"));

	await t.test("the built-in tools are tools.json's, no more and no fewer", () => {
		const file = JSON.parse(readFileSync(new URL("./tools.json", import.meta.url), "utf8")).tools;
		const builtins = Object.keys(tools).filter((n) => n.startsWith("piggery_"));
		assert.deepEqual(builtins.sort(), file.map((f) => "piggery_" + f.name).sort());
		for (const f of file) {
			const d = tools["piggery_" + f.name];
			assert.deepEqual(Object.keys(d.parameters.properties).sort(), Object.keys(f.parameters.properties).sort(), f.name);
			assert.deepEqual(d.parameters.required ?? [], f.parameters.required ?? [], f.name);
			assert.doesNotMatch(d.description, /\{tool:/, f.name);
		}
	});

	await t.test("identify and every model or thinking change report both, pi's level unmapped", async () => {
		const id = calls.find((c) => c.verb === "identify" && c.as === "p1");
		assert.equal(id.args.model, "HP/glm-5.3-flash");
		assert.deepEqual(id.args.capabilities, ["abort", "wake", "steer", "system_prompt"]); // else abort is refused, the card repeated
		assert.equal(id.args.thinking, "xhigh");
		assert.equal(id.args.protocol_version, 1);
		const presence = () => calls.filter((c) => c.verb === "harness.event" && c.args.event === "model_changed").map((c) => c.args);
		handlers.thinking_level_select({ level: "minimal", previousLevel: "xhigh" }, ctx);
		level = "minimal";
		handlers.model_select({ model: { provider: "HP", id: "kimi-k3" }, previousModel: glm, source: "set" }, ctx);
		await until(() => presence().length === 2);
		assert.deepEqual(presence(), [
			{ event: "model_changed", model: "HP/glm-5.3-flash", thinking: "minimal" },
			{ event: "model_changed", model: "HP/kimi-k3", thinking: "minimal" },
		]);
	});

	await t.test("after its own close the gate joins again as a solo, in the same pi session", async () => {
		const res = await tools.piggery_agent.execute("c1", { action: "close" });
		assert.match(res.content[0].text, /closed team t1; stopped w1\. You are solo now/);
		await until(() => calls.some((c) => c.verb === "identify" && c.as === "p2"));

		const rejoin = calls.filter((c) => c.verb === "join.auto");
		assert.equal(rejoin.length, 2);
		assert.equal(rejoin[1].args.harness_ref, "sess-1"); // the same pi session, placed again
		assert.deepEqual(rejoin[1].args.transcript, { path: "/s/sess-1.jsonl", format: "pi" }); // its file, for top
		const afterClose = calls.slice(calls.findIndex((c) => c.args?.action === "close") + 1);
		assert.ok(afterClose.every((c) => c.as !== "p1"), "nothing more as the closed team's participant");
		assert.deepEqual(active.sort(), ["piggery_agent", "piggery_inbox", "piggery_send", "piggery_who"]);
	});
	await t.test("reopen by the old gate's session: it acts as its old participant, with the role's tools", async () => {
		identity.p1.tools = ["send", "inbox"];
		// The solo's daemon sent no protocol_version (an older one): silent while solo, told in the team.
		delete identity.p1.protocol_version;
		const errs = [];
		const consoleError = console.error;
		console.error = (m) => errs.push(String(m));
		t.after(() => (console.error = consoleError));
		const res = await tools.piggery_agent.execute("c2", { action: "reopen", team: "t1" });
		console.error = consoleError;
		assert.match(errs.join("\n"), /daemon speaks protocol 0, this extension 1: run `piggery restart`/);
		assert.deepEqual(calls.findLast((c) => c.verb === "agent").args, { action: "reopen", team: "t1" });
		const id = calls.findLast((c) => c.verb === "identify");
		assert.equal(id.as, "p1");
		assert.equal(id.args.run_id, "r1b");
		assert.match(res.content[0].text, /^Reopened team t1\nyour tools: piggery_send, piggery_inbox$/);
		assert.deepEqual(active.sort(), ["piggery_inbox", "piggery_send"]);
	});
	handlers.session_shutdown?.({ reason: "quit" });
});
