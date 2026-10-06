// Test helpers: a fake piggery daemon on a unix socket (a pipe on Windows), and the little of dsh (a Cordis context and
// its agents) the plugin touches.
import { mkdtempSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fakeDaemonPath, setHome } from "../testutil/daemon.mjs";

/** HOME is a fresh directory with ~/.piggery; returns it. */
export function tempHome() {
	setHome(mkdtempSync(join(tmpdir(), "pgdsh")));
	return process.env.HOME;
}

export const until = async (pred, ms = 5000) => {
	for (const end = Date.now() + ms; !pred(); ) {
		if (Date.now() > end) throw new Error("timeout");
		await new Promise((r) => setTimeout(r, 20));
	}
};

/** answer(frame) is the daemon's reply to each verb; push(event) reaches every connection. */
export async function fakeDaemon(home, answer) {
	const calls = [];
	const socks = new Set();
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
				calls.push({ verb: f.verb, as: f.auth?.id, args: f.args });
				s.write(JSON.stringify({ id: f.id, result: answer(f, calls) }) + "\n");
			}
		});
	});
	await new Promise((r) => srv.listen(fakeDaemonPath(home), r));
	return {
		calls,
		close: () => {
			for (const s of socks) s.destroy();
			srv.close();
		},
		push: (event, extra) => {
			for (const s of socks) s.write(JSON.stringify({ event, ...extra }) + "\n");
		},
	};
}

/** A Cordis-like context: handlers by event, effects, services. */
export function fakeCtx({ agents = [], services = {} } = {}) {
	const handlers = {};
	const cleanups = [];
	const ctx = {
		handlers,
		on: (event, fn, opts) => {
			(handlers[event] ??= []).push({ fn, opts });
			return () => {};
		},
		effect: (fn) => cleanups.push(fn()),
		get: (key) => services[key],
		agents: { list: () => agents, roots: () => agents.filter((a) => !a.child), create: async () => {}, resume: async () => {} },
		/** Runs every listener of a serial event in order, as dsh does (each awaited). */
		async emit(event, payload) {
			for (const { fn } of handlers[event] ?? []) await fn(payload);
		},
		/** A session event of an agent, as ctx.on("session/event") gets it. */
		session: (agent, ev) => (handlers["session/event"] ?? []).forEach(({ fn }) => fn(agent.session, ev)),
		dispose: () => cleanups.forEach((c) => c?.()),
	};
	return ctx;
}

export function fakeAgent(id, { cwd = "/w", model = "glm-5.3-flash", child = false, origin } = {}) {
	const tools = {};
	const sections = {};
	const a = {
		id,
		child,
		status: "idle",
		options: { provider: "hp", model },
		session: { id, header: { cwd, ...(origin ? { origin } : {}) } },
		inbox: { nextStep: [], nextTurn: [], remove(mid) {
			for (const k of ["nextStep", "nextTurn"]) a.inbox[k] = a.inbox[k].filter((m) => m.id !== mid);
			return true;
		} },
		steered: [],
		followed: [],
		cancels: [],
		tools,
		sections,
		steer: (m) => a.steered.push(m),
		followup: (m) => a.followed.push(m),
		cancel: (cause, opts) => a.cancels.push({ cause, opts }),
		ctx: {
			tools: {
				register: (d) => ((tools[d.name] = d), () => delete tools[d.name]),
				denied: [],
				restrict: ({ deny }) => {
					if (deny.includes("unknown")) throw new Error("unknown tool");
					a.ctx.tools.denied.push(...deny);
					return () => {};
				},
			},
			systemPrompt: { section: (s) => ((sections[s.name] = s), () => delete sections[s.name]) },
		},
	};
	return a;
}
