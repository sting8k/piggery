// piggery adapter for opencode 1.x (tested 1.18.34): one plugin for both ways opencode runs.
//
//   - the TUI (and `opencode serve` by hand): every root session joins piggery as its own participant
//     (join.auto by its session id), with the piggery tools and its role card. A subagent's child
//     session (the task tool, `parentID`) is nobody's participant.
//   - a worker piggery spawns: `opencode serve` with this plugin and PIGGERY_ID/TOKEN/RUN_ID in its
//     environment. The plugin binds exactly one root session: PIGGERY_OPENCODE_SESSION when the worker
//     resumes one (bound at once: it may need waking with no event ever coming), else the one whose
//     metadata.piggery_participant is PIGGERY_ID (session.created or client.session.get). Every other
//     session and every child is ignored. The driver does the rest (process, session, model, records)
//     over opencode's HTTP API.
//
// Mail goes the way pi's does: the daemon counts batches and acks, Turns (extensions/pi/adapter.mjs)
// maps events to calls, bridge.mjs maps opencode's events onto Turns, and the text the daemon hands
// back is a prompt the plugin sends to its own session (client.session.promptAsync: a run when idle,
// a steer while busy). No npm dependencies. PIGGERY_DISABLED=1 makes it inert.
//
// opencode may call plugin() twice for one directory (the TUI with --port) and only the second
// instance gets events and hooks, so plugin() opens nothing: a session joins at its first hook or
// event, and the daemon connection is made then.
import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { appendFileSync, closeSync, existsSync, fstatSync, mkdirSync, openSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { Turns } from "../pi/adapter.mjs";
import { Client } from "../pi/client.mjs";
import { afterRetire, render, renderWho, sentText } from "../pi/render.mjs";
import { Bridge, NUDGE } from "./bridge.mjs";
import { Records } from "./records.mjs";

const BUILTINS = JSON.parse(readFileSync(new URL("../pi/tools.json", import.meta.url), "utf8")).tools;
const TOOLS = BUILTINS.map((t) => t.name);
const PREFIX = "piggery_"; // model-facing names; verbs, manifests and identify.tools use the short ones
const PROTOCOL_VERSION = 1; // core.ProtocolVersion when this plugin was written
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const errorText = (e) => (e?.message ?? String(e)).slice(0, 300);

// The events that belong to a session of this plugin's (the rest, and the startup noise, are not looked at).
const SCOPED = new Set(["session.created", "session.status", "session.idle", "session.error", "message.updated", "message.part.updated", "permission.asked", "permission.replied"]);

// Per process, surviving a reload of this plugin: what the environment said at first load, and each
// session's daemon identity (a reloaded plugin re-identifies as the same run).
const proc = (globalThis.__piggeryOpencode ??= { saved: new Map() });

const description = (t) => t.description.replace(/\{tool:([^{}]*)\}/g, (_, n) => PREFIX + n);

export default {
	id: "piggery",
	server: async (input, options) => {
		// Decided once per process: this plugin sets PIGGERY_DISABLED=1 for its children, so a reload
		// must not read it back as "disabled". A helper process of a session is not a member.
		proc.disabled ??= process.env.PIGGERY_DISABLED === "1";
		process.env.PIGGERY_DISABLED = "1";
		if (!("worker" in proc)) {
			// What the driver started this process with; nothing of it may reach a subprocess of the session.
			const e = process.env;
			proc.worker = e.PIGGERY_ID && e.PIGGERY_TOKEN
				? { auth: { id: e.PIGGERY_ID, token: e.PIGGERY_TOKEN }, run: e.PIGGERY_RUN_ID, session: e.PIGGERY_OPENCODE_SESSION || undefined }
				: null;
			for (const k of Object.keys(e)) if (k.startsWith("PIGGERY_") && k !== "PIGGERY_DISABLED") delete e[k];
		}
		if (proc.disabled) return {};
		const host = new Host(input, options ?? {});
		// A resumed worker's session is known: join now, not at an event that may never come.
		if (proc.worker?.session) setTimeout(() => host.partFor(proc.worker.session).catch(() => {}), 0);
		return host.hooks();
	},
};

class Host {
	constructor(input, options) {
		this.client = input.client;
		this.directory = input.directory;
		this.sessions = options.sessions ?? join(homedir(), ".piggery", "sessions", "opencode"); // where a TUI session keeps its records, <id>/records.jsonl
		this.worker = proc.worker;
		this.sockPath = join(homedir(), ".piggery", "piggery.sock");
		this.info = new Map(); // session id -> opencode's session info, from session.created
		this.roots = new Map(); // session id -> Promise<Part | null>
		this.models = new Map(); // session id -> the model its latest user message named
	}

	log(msg) {
		// Never stderr: in the TUI it lands on the screen.
		Promise.resolve(this.client.app?.log?.({ body: { service: "piggery", level: "warn", message: msg } })).catch(() => {});
	}

	hooks() {
		const tool = {};
		for (const t of BUILTINS) {
			tool[PREFIX + t.name] = {
				description: description(t),
				// Raw JSON-schema args take opencode's legacy path (every key required): tool.definition below
				// gives the model the real schema, and execute checks nothing it did not ask for.
				args: t.parameters.properties ?? {},
				execute: (args, ctx) => this.runTool(t.name, args ?? {}, ctx),
			};
		}
		return {
			tool,
			event: async ({ event }) => this.onEvent(event).catch((e) => this.log(errorText(e))),
			"chat.message": async (input, output) => this.onUserMessage(input, output).catch((e) => this.log(errorText(e))),
			"experimental.chat.system.transform": async (input, output) => {
				try {
					const part = input.sessionID ? await this.partFor(input.sessionID) : null;
					if (part?.roleCard) output.system.push(part.roleCard);
				} catch (e) {
					this.log(errorText(e));
				}
			},
			"tool.definition": async (input, output) => {
				const t = input.toolID.startsWith(PREFIX) && BUILTINS.find((b) => PREFIX + b.name === input.toolID);
				if (!t) return;
				output.description = description(t);
				output.jsonSchema = t.parameters;
			},
			dispose: async () => {
				for (const p of this.roots.values()) (await p)?.stop();
			},
		};
	}

	async onEvent(ev) {
		const p = ev.properties ?? {};
		if (ev.type === "session.created" && p.info?.id) this.info.set(p.info.id, p.info);
		if (ev.type === "session.deleted") {
			const part = await this.roots.get(p.info?.id);
			if (part) part.end();
			this.roots.delete(p.info?.id);
			return;
		}
		if (!SCOPED.has(ev.type)) return;
		const sid = p.sessionID ?? p.info?.sessionID ?? p.part?.sessionID ?? p.info?.id;
		const part = sid ? await this.partFor(sid) : null;
		part?.event(ev);
	}

	async onUserMessage(input, output) {
		const sid = input.sessionID;
		if (input.model) this.models.set(sid, { model: `${input.model.providerID}/${input.model.modelID}`, thinking: input.variant ?? "" });
		const part = await this.partFor(sid);
		part?.userMessage(input, output);
	}

	// The participant of a root session, once it is identified (a moment: a local socket); null for
	// a subagent's session, another worker's, or when this process does not drive piggery.
	partFor(sid) {
		let p = this.roots.get(sid);
		if (!p) {
			p = this.adopt(sid);
			this.roots.set(sid, p);
		}
		return p;
	}

	async adopt(sid) {
		const w = this.worker;
		let info = this.info.get(sid);
		if (!info && w?.session === sid) info = { id: sid, directory: this.directory }; // the one session of a resumed worker: nothing to look up
		if (!info) {
			try {
				info = (await this.client.session.get({ path: { id: sid } })).data;
			} catch {
				this.roots.delete(sid); // not known to be a root: ask again next time
				return null;
			}
		}
		if (!info || info.parentID) return null; // a child session
		// A worker is one session: the one it was told, else the one the driver made for it.
		if (w && (w.session ? sid !== w.session : info.metadata?.piggery_participant !== w.auth.id)) return null;
		const part = new Part(this, sid, info);
		part.start();
		if (existsSync(this.sockPath)) await Promise.race([part.identified, sleep(1500)]);
		return part;
	}

	async runTool(name, args, ctx) {
		const part = await this.partFor(ctx.sessionID);
		if (!part) throw new Error("this session is not a piggery participant (a subagent's session, or another worker's): piggery tools are for the main session");
		if (!part.tools.has(name)) throw new Error(`${PREFIX}${name} is not available to your role`);
		return TOOL_BODIES[name](part, args);
	}
}

class Part {
	constructor(host, id, info) {
		this.host = host;
		this.id = id;
		this.cwd = info.directory ?? host.directory;
		const w = host.worker;
		this.isWorker = !!w;
		this.envAuth = w?.auth; // a worker's credentials; cleared if it founds a team (a new participant)
		this.saved = proc.saved.get(id) ?? {};
		this.runId = w?.run ?? this.saved.runId;
		this.auth = w?.auth ?? this.saved.auth;
		this.client = undefined;
		this.stale = false;
		this.inTeam = false;
		this.notice = undefined;
		this.roleCard = "";
		this.tools = new Set(TOOLS); // until the daemon places it: the solo tools, which start the daemon on use
		// The model the session names: its latest user message's (a hook saw it), else the session's own.
		const m = host.models.get(id) ?? (info.model ? { model: `${info.model.providerID}/${info.model.id}`, thinking: info.model.variant && info.model.variant !== "default" ? info.model.variant : "" } : undefined);
		this.model = m?.model ?? "";
		this.thinking = m?.thinking ?? "";
		this.sent = []; // texts delivered as prompts whose user message has not come back yet
		let write;
		const file = !this.isWorker && host.sessions ? join(host.sessions, id, "records.jsonl") : undefined;
		this.recordsFile = file;
		if (file) {
			mkdirSync(dirname(file), { recursive: true, mode: 0o700 });
			write = (l) => appendFileSync(file, l + "\n", { mode: 0o600 });
		}
		this.records = file ? new Records(write) : undefined;
		this.turns = this.saved.turns ?? new Turns({});
		this.turns.io = this.io();
		this.bridge = new Bridge(this.turns, { nudge: () => this.prompt(NUDGE) });
		this.saved.turns = this.turns;
		proc.saved.set(id, this.saved);
		this.identified = new Promise((r) => (this.markIdentified = r));
	}

	io() {
		// A stopped part has no client: what its queue still holds fails as "not connected", silently.
		const call = (verb, args) => (this.client ? this.client.call(verb, args) : Promise.reject(Object.assign(new Error("daemon not connected"), { notConnected: true })));
		return {
			event: (args) => call("harness.event", args),
			presence: (event) => call("presence", { event }),
			deliver: (text) => this.prompt(text),
			newKey: () => randomUUID(),
			onError: (err) => {
				if (!err?.notConnected && !err?.fatal) this.host.log(`piggery: ${err?.message ?? err}`);
			},
		};
	}

	// Text as a user message of the session: a run when idle, read at the loop's next step while busy.
	// Never a model (the session keeps its own: the driver switches it with a message of its own), but the
	// session's variant: a prompt without one would drop the thinking level the Human set. In order.
	prompt(text) {
		if (text !== NUDGE) this.sent.push(text);
		this.pq = (this.pq ?? Promise.resolve()).then(async () => {
			const variant = await this.variant();
			await this.host.client.session.promptAsync({ path: { id: this.id }, body: { parts: [{ type: "text", text }], ...(variant ? { variant } : {}) } });
		}).catch((err) => {
			this.sent = this.sent.filter((t) => t !== text);
			this.host.log(`piggery: could not send mail to the session: ${errorText(err)}`);
		});
	}

	// The session's model variant (thinking level); "default" or none: no variant.
	async variant() {
		try {
			const v = (await this.host.client.session.get({ path: { id: this.id } })).data?.model?.variant;
			return v && v !== "default" ? v : undefined;
		} catch {
			return undefined;
		}
	}

	abort() {
		Promise.resolve(this.host.client.session.abort({ path: { id: this.id } })).catch((err) => this.host.log(`piggery: abort: ${errorText(err)}`));
	}

	event(ev) {
		this.records?.event(ev);
		this.bridge.event(ev);
	}

	// A user message of this session: the person's prompt, or the mail this plugin sent.
	userMessage(input, output) {
		if (input.model) this.observe(`${input.model.providerID}/${input.model.modelID}`, input.variant ?? "");
		const parts = output.parts ?? [];
		// The driver's model switch ("piggery: model switch") is a noReply message of synthetic, ignored parts: it is
		// no mail and no turn, and no record. It still carries the new model, read above.
		if (parts.length && parts.every((p) => p.synthetic && p.ignored)) return;
		const text = parts.filter((p) => p.type === "text" && p.text).map((p) => p.text).join("\n");
		const i = this.sent.indexOf(text);
		if (i >= 0) {
			this.sent.splice(i, 1);
			this.bridge.delivered(output.message.id);
		}
	}

	harness() {
		return {
			harness: "opencode",
			mode: this.isWorker ? "rpc" : "interactive",
			harness_ref: this.id,
			tool_prefix: PREFIX,
			model: this.model,
			thinking: this.thinking,
			// The abort push aborts the run, a wake push starts one, mail goes in as a prompt (a steer while
			// busy), the role card goes in the system prompt. A worker's are declared by its runtime driver.
			capabilities: ["abort", "wake", "steer", "system_prompt"],
		};
	}

	start() {
		this.startClient(this.auth);
	}

	stop() {
		this.client?.stop();
		this.client = undefined;
	}

	// The session is gone (deleted): tell the daemon, then let go.
	end() {
		const cl = this.client;
		const done = cl?.ready && !this.stale ? cl.call("harness.event", { event: "session_end" }).catch(() => {}) : Promise.resolve();
		done.then(() => this.stop());
	}

	// Tools and role card of the current identity (solo or a role).
	apply(res) {
		this.roleCard = res.role_card ?? "";
		this.tools = new Set(res.tools ?? []);
		this.inTeam = !!res.team_id;
		const daemon = res.protocol_version ?? 0;
		if (daemon !== PROTOCOL_VERSION && !this.warned && this.inTeam) {
			this.warned = true;
			this.host.log(`piggery: the daemon speaks protocol ${daemon}, this plugin ${PROTOCOL_VERSION}: ` + (daemon < PROTOCOL_VERSION ? "run `piggery restart`" : "run `piggery setup opencode`") + ", then restart opencode");
		}
	}

	startClient(auth) {
		const cl = new Client({
			path: this.host.sockPath,
			auth,
			onPush: (f) => {
				if (f.event === "wake") this.turns.wake();
				// piggery -a abort: abort the run as Esc does. It ends interrupted, so the daemon acks
				// nothing and its mail comes again.
				else if (f.event === "abort") this.abort();
				else if (f.event === "role") {
					// Admitted into a team: same participant and run, new role.
					this.reidentify()
						.then((res) => this.prompt(`[piggery] you were admitted to a team as ${res.name} (role ${res.role}); your piggery tools and role card are updated (see piggery_who)`))
						.catch((err) => this.host.log(`piggery: ${err.message}`));
				} else if (f.event === "retire" && !this.stale) {
					cl.stop();
					this.retire({ rule_id: f.reason, message: f.reason });
				}
			},
			onConnect: () => this.identify(cl),
			onReady: () => {
				// identify closed any turn left open: a run still going continues under a new key.
				// Idle: mail that may have arrived while disconnected or before this session joined.
				if (this.turns.running) this.turns.resumeRunning();
				else this.turns.wake();
				this.markIdentified();
			},
			onFatal: (err) => this.retire(err),
			log: (m) => this.host.log(m.replace("pi keeps working", "opencode keeps working")),
		});
		this.client = cl;
		cl.start();
	}

	// cl is the client whose socket just connected (never whatever this.client points to now).
	async identify(cl) {
		const h = this.harness();
		if (!cl.auth) {
			// No PIGGERY_*: the daemon places this session: its old participant if it was in a
			// still-open team (resume), else a new solo participant.
			let res;
			try {
				const transcript = this.recordsFile ? { path: this.recordsFile, format: "driver" } : undefined;
				res = await cl.early("join.auto", { cwd: this.cwd, ...h, transcript });
			} catch (err) {
				if (err?.code) {
					// Live elsewhere (invalid) or a headless worker's session (not_found): inert, silently.
					this.stale = true;
					cl.stop();
					this.tools = new Set();
					this.markIdentified();
					return;
				}
				throw err;
			}
			cl.auth = this.auth = this.saved.auth = { id: res.id, token: res.token };
			this.runId = this.saved.runId = res.run_id;
		}
		const newRun = !this.runId;
		const args = { new_run: newRun, protocol_version: PROTOCOL_VERSION, ...h };
		// Same run: the turn ends the daemon missed while it was down go with identify; a new run
		// drops them (its old turns can never ack).
		if (newRun) this.turns.reset();
		else {
			args.run_id = this.runId;
			if (this.turns.ended.length) args.ended = this.turns.ended;
		}
		const res = await cl.early("identify", args);
		this.turns.takeEnded();
		if (newRun) this.runId = this.saved.runId = res.run_id;
		this.apply(res);
		if (this.notice) {
			this.prompt(this.notice);
			this.notice = undefined;
		}
	}

	// Same run, new identity (admitted, or founded a team): identify again for tools and role card.
	async reidentify() {
		const res = await this.client.call("identify", { new_run: false, run_id: this.runId, protocol_version: PROTOCOL_VERSION, ...this.harness() });
		this.apply(res);
		return res;
	}

	// This process may no longer act for the participant (another process took it over, or the
	// token is bad): stop driving for good, never re-identify as a new run.
	retire(err) {
		if (afterRetire(err, this.isWorker) === "solo") return this.becomeSolo();
		this.stale = true;
		this.tools = new Set();
		this.host.log(
			err.rule_id === "run.stale"
				? "piggery: another process now owns this participant; this session stops driving it"
				: err.rule_id === "team.closed"
					? "piggery: left the team: team closed (team down)"
					: `piggery: ${err.message}; this session stops driving it`,
		);
	}

	// Team down (or its own close): drop the closed team's participant and let join.auto place
	// the same session again as a solo.
	becomeSolo(tell = true) {
		this.client?.stop();
		this.client = undefined;
		this.envAuth = undefined;
		this.auth = this.runId = this.saved.auth = this.saved.runId = undefined;
		this.turns.reset();
		this.inTeam = false;
		this.roleCard = "";
		this.tools = new Set(TOOLS);
		if (tell) this.notice = "[piggery] your team was closed (team down); you are solo now";
		this.startClient(undefined);
	}

	// A tool call needs the daemon: start it if it is not running (the model called a piggery
	// tool, so the human asked), then wait for the client to connect and identify.
	async ensureConnected() {
		if (this.stale) throw new Error("this opencode session no longer drives a piggery participant");
		if (this.client?.ready) return;
		if (!existsSync(this.host.sockPath)) await startDaemon(this.host.sockPath);
		this.client?.stop();
		this.startClient(this.envAuth ?? this.auth);
		for (let i = 0; i < 100 && !this.client?.ready && !this.stale; i++) await sleep(100);
		if (!this.client?.ready) throw new Error("could not reach the piggery daemon");
	}

	// The model or thinking level of the session's latest user message: tell the daemon, which keeps
	// both for workers this session spawns. opencode has no event for a change in the TUI: it shows
	// at the next prompt.
	observe(model, thinking) {
		if (model === this.model && thinking === this.thinking) return;
		this.model = model;
		this.thinking = thinking;
		if (this.client?.ready) this.client.call("harness.event", { event: "model_changed", model, thinking }).catch(() => {});
	}
}

// What each tool does here (tools.json says what it is).
const TOOL_BODIES = {
	async send(part, p) {
		await part.ensureConnected();
		return sentText(await part.client.call("send", p));
	},
	async inbox(part, p) {
		await part.ensureConnected();
		// No batch: in a turn the daemon records the delivery in that turn; a view is read-only.
		const msgs = await part.client.call("inbox", p.view ? { view: p.view } : {});
		if (!msgs.length) return p.view ? "Nothing in this view." : "No new messages.";
		return p.view ? render(msgs, `view ${p.view}, ${msgs.length} message(s), nothing marked read`) : render(msgs);
	},
	async who(part) {
		await part.ensureConnected();
		return renderWho(await part.client.call("who", undefined), (part.envAuth ?? part.auth)?.id);
	},
	async agent(part, p) {
		await part.ensureConnected();
		const cl = () => part.client;
		if (p.action === "found" || p.action === "reopen") {
			const r = await cl().call("agent", p.action === "found" ? { action: "found", template: p.template || undefined } : { action: "reopen", team: p.team });
			if (r.token) {
				// Another participant for this session: founded from inside a team (the old one left),
				// or reopened by the old gate's own session (its old participant is back).
				cl().auth = part.auth = part.saved.auth = { id: r.participant_id, token: r.token };
				part.envAuth = undefined;
				part.runId = part.saved.runId = r.run_id;
				part.turns.reset();
			}
			const res = await part.reidentify();
			const tools = "your tools: " + (res.tools ?? []).map((t) => PREFIX + t).join(", ");
			if (p.action === "reopen") return `${r.text}\n${tools}`;
			return `founded team ${r.team_name}; you are ${res.name} (${res.role}), its gate; ${tools}`;
		}
		if (p.action === "templates") return (await cl().call("agent", { action: "templates" })).text;
		if (!part.inTeam && !(p.action === "spawn" && p.template) && !(p.action === "close" && p.team) && !p.action.startsWith("gate_")) throw new Error("you are a solo session: only actions templates, found, reopen, gate_close, gate_open, and spawn with template or close with team for a taskforce");
		const r = await cl().call("agent", p);
		if (p.action === "close" && !p.team) {
			// The team is closed and this participant left it (no retire push to the caller).
			part.becomeSolo(false);
			const failed = r.failed?.length ? `; could not stop: ${r.failed.join(", ")}` : "";
			return `closed team ${r.team_name}` + (r.stopped?.length ? `; stopped ${r.stopped.join(", ")}` : "") + `${failed}. You are solo now; your tools: ${TOOLS.map((t) => PREFIX + t).join(", ")}`;
		}
		if (p.action === "close") return `closed taskforce ${r.team_name}` + (r.stopped?.length ? `; stopped ${r.stopped.join(", ")}` : "");
		if (p.action === "tail") return (r.records ?? []).map((x) => JSON.stringify(x)).join("\n") || "(no output)";
		if (r.exit) return `stopped (exit ${JSON.stringify(r.exit)})`;
		// Names and #N only, no ids.
		if (p.action === "spawn") return p.template ? `called up taskforce ${r.team_name}; its task is #${r.task_seq}; write to it as ${r.team_name}, its result comes to you as mail` : `spawned ${p.name}; its task is #${r.task_seq} (its reply comes to you as mail)`;
		if (p.action === "resume" && r.task_seq) return `resumed ${p.target}; its task is #${r.task_seq} (its reply comes to you as mail)`;
		if (p.action === "gate_close" || p.action === "gate_open") return r.text;
		if (p.action === "admit") return `admitted ${p.target} as ${p.role}`;
		return `${p.action} ok: ${p.target}`;
	},
};

async function startDaemon(sock) {
	const dir = dirname(sock);
	mkdirSync(dir, { recursive: true, mode: 0o700 });
	// Like the CLI autostart: append to serve.log (0600), own session (detached = setsid), released.
	const logFile = join(dir, "serve.log");
	const out = openSync(logFile, "a", 0o600);
	const from = fstatSync(out).size;
	let exited = false; // serve ended before its socket appeared: it could not start
	await new Promise((resolve, reject) => {
		const d = spawn("piggery", ["serve"], { detached: true, stdio: ["ignore", out, out] });
		d.on("error", (e) => reject(new Error(e.code === "ENOENT" ? "the piggery binary is not on PATH (see https://github.com/sting8k/piggery/blob/main/docs/guide.md)" : e.message)));
		d.on("exit", () => (exited = true));
		d.on("spawn", () => {
			d.unref();
			resolve();
		});
	}).finally(() => closeSync(out));
	for (let i = 0; i < 50 && !existsSync(sock) && !exited; i++) await sleep(100);
	if (existsSync(sock)) return;
	// Its reason is the last line it wrote to the log (e.g. a bad config.yaml).
	const lines = readFileSync(logFile).subarray(from).toString("utf8").trim().split("\n");
	const why = (lines[lines.length - 1] ?? "").replace(/^piggery serve: /, "").trim() || "piggery serve exited";
	throw new Error(`the daemon did not start: ${why} (log: ${logFile})`);
}
