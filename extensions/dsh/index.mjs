// piggery adapter for dsh (DeepSeek Harness 0.2): one Cordis plugin for both ways dsh runs.
//
//   - `dsh web` (or any profile): every root agent (never a subagent) joins piggery as its own
//     participant (join.auto by its session id), with the piggery tools and its role card.
//   - a worker piggery spawns: `dsh --profile sdk` with this plugin, which creates (or resumes) the
//     one agent under the session id piggery chose, and takes abort / set_model from the driver
//     as JSON-RPC notifications on stdin. The driver's log is the stdout records (`piggery/record`).
//
// Mail goes the way pi's does: the daemon counts batches and acks, Turns (extensions/pi/adapter.mjs)
// maps events to calls, and the text the daemon hands back is shown with agent.steer/followup.
// No npm dependencies: dsh's objects are used as they come. PIGGERY_DISABLED=1 makes it inert.
import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { appendFileSync, closeSync, existsSync, fstatSync, mkdirSync, openSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { StringDecoder } from "node:string_decoder";
import { Turns } from "../pi/adapter.mjs";
import { Client, daemonAddress } from "../pi/client.mjs";
import { afterRetire, render, renderWho, sentText } from "../pi/render.mjs";
import { Bridge } from "./bridge.mjs";
import { Records } from "./records.mjs";

export const name = "piggery";
export const inject = ["tools", "agents", "systemPrompt"];

const BUILTINS = JSON.parse(readFileSync(new URL("../pi/tools.json", import.meta.url), "utf8")).tools;
const TOOLS = BUILTINS.map((t) => t.name);
const PREFIX = "piggery_"; // model-facing names; verbs, manifests and identify.tools use the short ones
const PROTOCOL_VERSION = 1; // core.ProtocolVersion when this plugin was written
const MAIL = "piggery-mail"; // the source kind of the messages this plugin adds to an inbox
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Per process, surviving a reload of this plugin: what the environment said at first load, and
// each agent's daemon identity (a reloaded plugin re-identifies as the same run).
const proc = (globalThis.__piggeryDsh ??= { agents: new Map() });

const errorText = (e) => (e?.message ?? String(e)).slice(0, 300);

export function apply(ctx, config) {
	// Decided once per process: this plugin sets PIGGERY_DISABLED=1 for its children, so a reload
	// must not read it back as "disabled". A helper process of an agent is not a member.
	proc.disabled ??= process.env.PIGGERY_DISABLED === "1";
	process.env.PIGGERY_DISABLED = "1";
	if (proc.disabled || proc.applied) return; // applied: another entry of this plugin (setup's row and a worker's overlay) is loaded
	proc.applied = true;
	ctx.effect(() => () => (proc.applied = false));
	if (!("worker" in proc)) {
		// What the driver started this process with; nothing of it may reach a subprocess of the agent.
		const e = process.env;
		proc.worker = e.PIGGERY_ID && e.PIGGERY_TOKEN && e.PIGGERY_DSH_SESSION
			? { auth: { id: e.PIGGERY_ID, token: e.PIGGERY_TOKEN }, run: e.PIGGERY_RUN_ID, session: e.PIGGERY_DSH_SESSION, resume: e.PIGGERY_DSH_RESUME === "1", model: e.PIGGERY_DSH_MODEL ?? "", thinking: e.PIGGERY_DSH_THINKING ?? "", deny: (e.PIGGERY_DSH_DENY ?? "").split(",").filter(Boolean) }
			: null;
		for (const k of Object.keys(e)) if (k.startsWith("PIGGERY_") && k !== "PIGGERY_DISABLED") delete e[k];
	}
	const worker = proc.worker;
	const sockPath = () => daemonAddress();
	const parts = new Map(); // agent id -> Part
	const log = (msg) => console.error(msg);

	// A worker's records are its stdout (the sdk profile's stdout is JSON-RPC frames; the driver
	// unwraps these); a web session's go to a file it reports as its transcript, in the directory
	// piggery wrote into this plugin's row (config.sessions; a row without it keeps no records).
	const rpc = (method, params) => process.stdout.write(JSON.stringify({ jsonrpc: "2.0", method, params }) + "\n");
	const recordsFile = (id) => (config?.sessions ? join(config.sessions, id, "records.jsonl") : undefined);

	class Part {
		constructor(agent, auth, runId) {
			this.agent = agent;
			this.id = String(agent.id);
			this.isWorker = worker !== null;
			this.envAuth = auth; // a worker's credentials; cleared if it founds a team (a new participant)
			this.saved = proc.agents.get(this.id) ?? {};
			this.runId = runId ?? this.saved.runId;
			this.auth = auth ?? this.saved.auth;
			this.client = undefined;
			this.stale = false;
			this.inTeam = false;
			this.notice = undefined;
			this.roleCard = "";
			this.tools = new Map(); // short name -> disposer of its registration
			this.offs = [];
			this.model = modelOf(agent.options);
			this.thinking = agent.options?.reasoningEffort ?? "";
			this.want = undefined; // {provider, model, reasoningEffort}: a switch asked for (worker)
			let write;
			if (this.isWorker) write = (r) => rpc("piggery/record", r);
			else if (recordsFile(this.id)) {
				const file = recordsFile(this.id);
				mkdirSync(dirname(file), { recursive: true, mode: 0o700 });
				write = (r) => appendFileSync(file, JSON.stringify(r) + "\n", { mode: 0o600 });
			} else write = () => {};
			this.records = new Records(write);
			this.turns = this.saved.turns ?? new Turns({});
			this.turns.io = this.io();
			this.bridge = new Bridge(this.turns, () => this.dropMail());
			this.saved.turns = this.turns;
			proc.agents.set(this.id, this.saved);
			this.identified = new Promise((r) => (this.markIdentified = r));
		}

		io() {
			// A stopped part has no client: what its queue still holds fails as "not connected", silently.
			const call = (verb, args) => (this.client ? this.client.call(verb, args) : Promise.reject(Object.assign(new Error("daemon not connected"), { notConnected: true })));
			return {
				event: (args) => call("harness.event", args),
				presence: (event) => call("presence", { event }),
				deliver: (text, steer) => this.deliver(text, steer),
				newKey: () => randomUUID(),
				onError: (err) => {
					if (!err?.notConnected && !err?.fatal) log(`piggery: ${err?.message ?? err}`);
				},
			};
		}

		// Mail text as a message of the agent: a steer while it runs (claimed at the next step), else
		// a turn of its own.
		deliver(text, steer) {
			const msg = { id: randomUUID(), role: "user", content: [{ type: "text", text }], source: { kind: MAIL } };
			if (steer || this.agent.status === "running") this.agent.steer(msg);
			else this.agent.followup(msg);
		}

		// The mail this plugin steered in and no step claimed. Its turn was not acked, so the daemon
		// gives it again: the copy left in dsh's inbox would run twice.
		dropMail() {
			const inbox = this.agent.inbox;
			for (const target of ["nextStep", "nextTurn"])
				for (const m of [...(inbox[target] ?? [])]) if (m.source?.kind === MAIL) inbox.remove(m.id);
		}

		abort() {
			this.agent.cancel({ kind: "hook", reason: "piggery abort" }, { keepInbox: true });
		}

		harness() {
			return {
				harness: "dsh",
				mode: this.isWorker ? "rpc" : "interactive",
				harness_ref: this.id,
				tool_prefix: PREFIX,
				model: this.model,
				thinking: this.thinking,
				// The abort push cancels the turn, a wake push starts one, mail goes in as a steer, the
				// role card goes in the system prompt. A worker's are declared by its runtime driver.
				capabilities: ["abort", "wake", "steer", "system_prompt"],
			};
		}

		start() {
			this.offs.push(this.agent.ctx.systemPrompt.section({ name: "piggery-role", order: 9000, text: () => this.roleCard }));
			this.setTools(TOOLS); // until the daemon places it: the solo tools, which start the daemon on use
			// The native tools a worker must not have (its profile's list, minus what its role keeps): a name
			// dsh does not have is refused by dsh, and left out.
			for (const name of worker?.deny ?? []) {
				try {
					this.offs.push(this.agent.ctx.tools.restrict({ deny: [name] }));
				} catch {}
			}
			this.startClient(this.auth);
		}

		stop() {
			this.client?.stop();
			this.client = undefined;
			for (const off of this.tools.values()) off();
			this.tools.clear();
			for (const off of this.offs.splice(0)) off();
		}

		// Tools and role card of the current identity (solo or a role).
		apply(res) {
			this.roleCard = res.role_card ?? "";
			this.setTools(res.tools ?? []);
			this.inTeam = !!res.team_id;
			const daemon = res.protocol_version ?? 0;
			if (daemon !== PROTOCOL_VERSION && !this.warned && this.inTeam) {
				this.warned = true;
				log(`piggery: the daemon speaks protocol ${daemon}, this plugin ${PROTOCOL_VERSION}: ` + (daemon < PROTOCOL_VERSION ? "run `piggery restart`" : "run `piggery setup dsh`") + ", then restart dsh");
			}
		}

		setTools(allowed) {
			for (const t of TOOLS) {
				const has = this.tools.has(t);
				if (allowed.includes(t) && !has) this.tools.set(t, this.register(t));
				else if (!allowed.includes(t) && has) {
					this.tools.get(t)();
					this.tools.delete(t);
				}
			}
		}

		register(name) {
			const t = BUILTINS.find((b) => b.name === name);
			const run = TOOL_BODIES[name];
			return this.agent.ctx.tools.register({
				name: PREFIX + name,
				description: t.description.replace(/\{tool:([^{}]*)\}/g, (_, n) => PREFIX + n),
				parameters: t.parameters,
				output: { schema: { type: "string" }, render: (_args, value) => [{ type: "text", text: value }] },
				execute: async (args) => run(this, args ?? {}),
			});
		}

		startClient(auth) {
			const cl = new Client({
				path: sockPath(),
				auth,
				onPush: (f) => {
					if (f.event === "wake") this.turns.wake();
					// piggery -a abort: cancel the turn as Esc does. It ends with no "completed" outcome,
					// so the daemon acks nothing and its mail comes again.
					else if (f.event === "abort") this.abort();
					else if (f.event === "role") {
						// Admitted into a team: same participant and run, new role.
						this.reidentify()
							.then((res) => this.deliver(`[piggery] you were admitted to a team as ${res.name} (role ${res.role}); your piggery tools and role card are updated (see piggery_who)`, false))
							.catch((err) => log(`piggery: ${err.message}`));
					} else if (f.event === "retire" && !this.stale) {
						cl.stop();
						this.retire({ rule_id: f.reason, message: f.reason });
					}
				},
				onConnect: () => this.identify(cl),
				onReady: () => {
					// identify closed any turn left open: a run still going continues under a new key.
					// Idle: mail that may have arrived while disconnected or before this agent started.
					if (this.turns.running) this.turns.resumeRunning();
					else this.turns.wake();
					this.markIdentified();
				},
				onFatal: (err) => this.retire(err),
				log: (m) => log(m.replace("pi keeps working", "dsh keeps working")),
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
					const file = this.isWorker ? undefined : recordsFile(this.id);
					const transcript = file ? { path: file, format: "driver" } : undefined;
					res = await cl.early("join.auto", { cwd: this.agent.session.header.cwd, ...h, transcript });
				} catch (err) {
					if (err?.code) {
						// Live elsewhere (invalid) or a headless worker's session (not_found): inert, silently.
						this.stale = true;
						cl.stop();
						this.setTools([]);
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
				this.deliver(this.notice, false);
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
			this.setTools([]);
			log(
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
			this.setTools(TOOLS);
			if (tell) this.notice = "[piggery] your team was closed (team down); you are solo now";
			this.startClient(undefined);
		}

		// A tool call needs the daemon: start it if it is not running (the model called a piggery
		// tool, so the human asked), then wait for the client to connect and identify.
		async ensureConnected() {
			if (this.stale) throw new Error("this dsh session no longer drives a piggery participant");
			if (this.client?.ready) return;
			if (!existsSync(sockPath())) await startDaemon();
			this.client?.stop();
			this.startClient(this.envAuth ?? this.auth);
			for (let i = 0; i < 100 && !this.client?.ready && !this.stale; i++) await sleep(100);
			if (!this.client?.ready) throw new Error("could not reach the piggery daemon");
		}

		// The model or thinking level the agent's next request uses changed: tell the daemon, which
		// keeps both for workers this session spawns.
		observe(cfg) {
			const model = `${cfg.provider}/${cfg.model}`;
			const thinking = cfg.reasoningEffort ?? "";
			if (model === this.model && thinking === this.thinking) return;
			this.model = model;
			this.thinking = thinking;
			if (this.client?.ready) this.client.call("harness.event", { event: "model_changed", model, thinking }).catch(() => {});
		}

		// A worker's model or thinking level from the driver: checked with the llm service (it refuses a
		// route or level it does not run), then used by every request from the next one.
		async setModel(model, thinking) {
			const cur = this.want ?? { ...splitModel(this.model), reasoningEffort: this.thinking || undefined };
			const next = { ...cur };
			if (model !== undefined) Object.assign(next, splitModel(model), { reasoningEffort: undefined });
			if (thinking !== undefined) next.reasoningEffort = thinking || undefined;
			await ctx.get("llm").resolveCallConfig({ provider: next.provider, model: next.model, ...(next.reasoningEffort ? { reasoningEffort: next.reasoningEffort } : {}) });
			this.want = next;
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
			if (!part.inTeam) throw new Error("you are a solo session: only actions templates, found and reopen (when the user asks for a team)");
			const r = await cl().call("agent", p);
			if (p.action === "close") {
				// The team is closed and this participant left it (no retire push to the caller).
				part.becomeSolo(false);
				const failed = r.failed?.length ? `; could not stop: ${r.failed.join(", ")}` : "";
				return `closed team ${r.team_name}` + (r.stopped?.length ? `; stopped ${r.stopped.join(", ")}` : "") + `${failed}. You are solo now; your tools: ${TOOLS.map((t) => PREFIX + t).join(", ")}`;
			}
			if (p.action === "tail") return (r.records ?? []).map((x) => JSON.stringify(x)).join("\n") || "(no output)";
			if (r.exit) return `stopped (exit ${JSON.stringify(r.exit)})`;
			// Names and #N only, no ids.
			if (p.action === "spawn") return `spawned ${p.name}; its task is #${r.task_seq} (its reply comes to you as mail)`;
			if (p.action === "resume" && r.task_seq) return `resumed ${p.target}; its task is #${r.task_seq} (its reply comes to you as mail)`;
			if (p.action === "admit") return `admitted ${p.target} as ${p.role}`;
			return `${p.action} ok: ${p.target}`;
		},
	};

	const isRoot = (agent) => agent.session?.header?.origin !== "subagent" && ctx.agents.roots().includes(agent);

	// A root agent joins. Held until the first identify (a moment: a local socket) so its first
	// request already has the role card; a daemon that is not there does not hold it.
	const adopt = async (agent) => {
		const id = String(agent.id);
		if (!parts.has(id)) {
			if (!isRoot(agent) || (worker && id !== worker.session)) return;
			const part = new Part(agent, worker?.auth, worker?.run);
			parts.set(id, part);
			part.start();
		}
		if (existsSync(sockPath())) await Promise.race([parts.get(id).identified, sleep(1500)]);
	};

	// A failure here must never fail the agent's creation.
	ctx.on("agent/created", ({ agent }) => adopt(agent).then(() => undefined, (e) => log(`piggery: ${errorText(e)}`)));
	ctx.on("agent/disposed", ({ agent }) => {
		const part = parts.get(String(agent.id));
		if (!part) return;
		parts.delete(part.id);
		// A session gone from this process: its participant is gone (a later resume places it again).
		const cl = part.client;
		const done = cl?.ready && !part.stale ? cl.call("harness.event", { event: "session_end" }).catch(() => {}) : Promise.resolve();
		done.then(() => part.stop());
	});
	ctx.on("session/event", (session, ev) => {
		const part = parts.get(String(session.id));
		if (!part) return;
		try {
			part.records.event(ev);
			part.bridge.event(ev);
		} catch (e) {
			log(`piggery: ${errorText(e)}`);
		}
	});
	ctx.on("agent/turn-stopping", ({ agent }) => parts.get(String(agent.id))?.bridge.turnStopping());
	// Outermost: sees the request configuration as everything else left it, and has the last word on a switch.
	ctx.on(
		"agent/request",
		async ({ agent }, next) => {
			let cfg = await next();
			const part = parts.get(String(agent.id));
			if (!part) return cfg;
			if (part.want) {
				const { reasoningEffort: _dropped, ...rest } = cfg;
				cfg = { ...rest, provider: part.want.provider, model: part.want.model, ...(part.want.reasoningEffort ? { reasoningEffort: part.want.reasoningEffort } : {}) };
			}
			part.observe(cfg);
			return cfg;
		},
		{ prepend: true },
	);
	// A permission question waits for a person: piggery holds the agent's mail meanwhile.
	ctx.on("approval/request", async (request, next) => {
		const part = parts.get(String(request.agent?.id));
		if (!part || part.isWorker) return next();
		part.turns.uiPrompt(true);
		try {
			return await next();
		} finally {
			part.turns.uiPrompt(false);
		}
	});

	// Agents that exist already (this plugin was loaded, or reloaded, into a running dsh).
	for (const agent of ctx.agents.list()) adopt(agent).catch((e) => log(`piggery: ${errorText(e)}`));
	ctx.effect(() => () => {
		for (const part of parts.values()) part.stop();
		parts.clear();
	});

	if (worker) startWorker(ctx, worker, parts, rpc, log);
}

// "provider/model" as piggery names it (pi's form); the model id may itself hold slashes.
function modelOf(o) {
	return o?.provider && o?.model ? `${o.provider}/${o.model}` : "";
}
function splitModel(m) {
	const i = m.indexOf("/");
	return i < 0 ? { provider: "", model: m } : { provider: m.slice(0, i), model: m.slice(i + 1) };
}

// Every model dsh's llm service lists, as "provider/model" (what set_model takes); a provider that
// cannot list now (no key, offline) is left out, not an error for the rest.
async function listModels(ctx) {
	const llm = ctx.get("llm");
	const out = [];
	for (const p of llm.listProviders()) {
		try {
			for (const m of await llm.listModels(p.id)) out.push(`${p.id}/${m.id}`);
		} catch {}
	}
	return out;
}

// The worker: create the agent under piggery's session id (or resume it), and take the driver's
// commands. The sdk profile's own stdin reader ignores what it does not know.
function startWorker(ctx, w, parts, rpc, log) {
	(async () => {
		await ctx.get("loader")?.await();
		const agentOptions = { ...(w.model ? splitModel(w.model) : ctx.get("agentDefaultModel")?.currentSelection?.()), ...(w.thinking ? { reasoningEffort: w.thinking } : {}) };
		const cwd = process.cwd();
		// A model or level dsh does not run fails here, before the agent exists, so the worker does not start.
		if (agentOptions.provider && agentOptions.model)
			await ctx.get("llm").resolveCallConfig({ provider: agentOptions.provider, model: agentOptions.model, ...(agentOptions.reasoningEffort ? { reasoningEffort: agentOptions.reasoningEffort } : {}) });
		const handle = w.resume
			? await ctx.agents.resume({ resumeSessionId: w.session, agentOptions })
			: await ctx.agents.create({ sessionId: w.session, meta: { cwd }, agentOptions });
		ctx.effect(() => () => handle.dispose?.());
		rpc("piggery/ready", { session: w.session });
	})().catch((e) => {
		// No agent, no worker: say why in its log and end, so the driver sees the exit.
		rpc("piggery/record", { type: "message_end", message: { role: "assistant", stopReason: "error", errorMessage: `dsh worker did not start: ${errorText(e)}` } });
		log(`piggery: dsh worker did not start: ${e?.stack ?? e}`);
		process.exitCode = 1;
		setTimeout(() => process.exit(1), 200);
	});

	let buf = "";
	const decoder = new StringDecoder("utf8"); // the sdk's own reader gets the same chunks: leave the stream's encoding alone
	const onData = (chunk) => {
		buf += typeof chunk === "string" ? chunk : decoder.write(chunk);
		for (let i; (i = buf.indexOf("\n")) >= 0; ) {
			const line = buf.slice(0, i);
			buf = buf.slice(i + 1);
			let m;
			try {
				m = JSON.parse(line);
			} catch {
				continue;
			}
			if (typeof m?.method !== "string" || !m.method.startsWith("piggery/")) continue;
			const part = parts.get(w.session);
			const reply = (r) => rpc("piggery/result", { id: m.params?.id, ...r });
			if (!part) {
				reply({ ok: false, error: "the agent is not running" });
			} else if (m.method === "piggery/abort") {
				part.abort();
			} else if (m.method === "piggery/models") {
				listModels(ctx).then((models) => reply({ ok: true, models }), (e) => reply({ ok: false, error: errorText(e) }));
			} else if (m.method === "piggery/set_model") {
				part.setModel(m.params?.model, m.params?.thinking).then(() => reply({ ok: true }), (e) => reply({ ok: false, error: errorText(e) }));
			}
		}
	};
	process.stdin.on("data", onData);
	ctx.effect(() => () => process.stdin.off("data", onData));
}

async function startDaemon() {
	const dir = join(homedir(), ".piggery");
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
	for (let i = 0; i < 50 && !existsSync(daemonAddress()) && !exited; i++) await sleep(100);
	if (existsSync(daemonAddress())) return;
	// Its reason is the last line it wrote to the log (e.g. a bad config.yaml).
	const lines = readFileSync(logFile).subarray(from).toString("utf8").trim().split("\n");
	const why = (lines[lines.length - 1] ?? "").replace(/^piggery serve: /, "").trim() || "piggery serve exited";
	throw new Error(`the daemon did not start: ${why} (log: ${logFile})`);
}
