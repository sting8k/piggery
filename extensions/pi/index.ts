// piggery adapter for pi: native tools, role card, and
// pi's events as the standard adapter events (adapter.mjs); the daemon counts batches and acks,
// the extension only shows the mail it hands back, natively (a new turn or a steer).
// Identity: PIGGERY_ID/PIGGERY_TOKEN (plus
// PIGGERY_RUN_ID for a spawned worker); without them, join.auto places the pi session: its old
// participant if it was in a still-open team (resume), else a new solo.
// PIGGERY_DISABLED=1 makes it inert. A solo session is silent (no warnings).
import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { closeSync, existsSync, fstatSync, mkdirSync, openSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { Type } from "@earendil-works/pi-ai";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Turns } from "./adapter.mjs";
import { afterRetire, render, renderWho, sentText } from "./render.mjs";
import { Client } from "./client.mjs";

// The built-in tools, defined once for every adapter (tools.json, next to this file); {tool:X}
// in a text is X's name here.
const BUILTINS: { name: string; description: string; parameters: object }[] = JSON.parse(
	readFileSync(new URL("./tools.json", import.meta.url), "utf8"),
).tools;
const TOOLS = BUILTINS.map((t) => t.name);
// Model-facing names carry this prefix; verbs, manifests and identify.tools use the
// short names.
const PREFIX = "piggery_";
// The version of the socket lines this extension relies on (core.ProtocolVersion when it was built).
const PROTOCOL_VERSION = 1;

// "provider/id", the form `pi --model` takes; "" when unknown.
const modelName = (m?: { provider: string; id: string }) => (m ? `${m.provider}/${m.id}` : "");

// Per-process state that must survive /reload (a reload re-runs this factory in the same process):
// the run this process identified as, and the turn state of that run.
type ProcState = {
	runId?: string;
	auth?: { id: string; token: string }; // from auto-join (env credentials are never stored here)
	envAuth?: { id: string; token: string }; // PIGGERY_ID/TOKEN, taken out of process.env at first load
	envRun?: string; // PIGGERY_RUN_ID, same
	disabled?: boolean; // PIGGERY_DISABLED=1 at first load (later loads see the value set for children)
	authRef?: string; // pi session id that auth was joined for
	stale?: boolean;
	turns?: Turns;
};
const proc: ProcState = ((globalThis as any).__piggeryPi ??= {});

export default function piggery(pi: ExtensionAPI) {
	// Decided once per process: this extension sets PIGGERY_DISABLED=1 for its children below, so
	// a /reload must not read it back as "disabled".
	proc.disabled ??= process.env.PIGGERY_DISABLED === "1";
	// Every child process (bash, a child pi) stays out of the team: a helper process of an agent is
	// not a member, a worker comes from the agent tool (the daemon strips PIGGERY_* for workers).
	process.env.PIGGERY_DISABLED = "1";
	if (proc.disabled || proc.stale) return;
	// Take the identity out of the environment so no subprocess (bash, a child pi) inherits it:
	// a child pi with these would identify as a new run and take over this session's participant.
	const { PIGGERY_ID: id, PIGGERY_TOKEN: token, PIGGERY_RUN_ID: run } = process.env;
	if (id && token) proc.envAuth ??= { id, token };
	proc.envRun ??= run;
	delete process.env.PIGGERY_ID;
	delete process.env.PIGGERY_TOKEN;
	delete process.env.PIGGERY_RUN_ID;
	let envAuth = proc.envAuth; // cleared if this session founds a team (new participant)
	const workerRun = proc.envRun;
	// A spawned worker identifies as the run the daemon created for its process (processes row,
	// tail log), not as a new run.
	if (envAuth && workerRun) proc.runId ??= workerRun;
	const sockPath = join(homedir(), ".piggery", "piggery.sock");

	let ctx: ExtensionContext | undefined;
	let roleCard = "";
	let client: Client | undefined;
	// Auto-join sessions say nothing until they are in a team: pi outside a team is normal.
	// In a team (identify team_id != ""). A solo session stays silent: no warnings.
	let inTeam = false;
	// Told to the model after the next identify (the session became solo after team down).
	let notice: string | undefined;

	const log = (msg: string) => {
		if (!inTeam) return;
		if (ctx?.mode === "tui" && ctx.hasUI) ctx.ui.notify(msg, "warning");
		else console.error(msg);
	};

	const deliver = (text: string, steer: boolean) => {
		// The adapter's view can trail pi by one event; never send a plain message into a busy agent.
		if (steer || (ctx && !ctx.isIdle())) pi.sendUserMessage(text, { deliverAs: "steer" });
		else pi.sendUserMessage(text);
	};
	const io = {
		event: (args: object) => client!.call("harness.event", args),
		presence: (event: string) => client!.call("presence", { event }),
		deliver,
		newKey: () => randomUUID(),
		pending: () => ctx?.hasPendingMessages() ?? false,
		onError: (err: any) => {
			// Outages are logged once by the client; a fatal error is reported by retire().
			if (!err?.notConnected && !err?.fatal) log(`piggery: ${err?.message ?? err}`);
		},
	};
	const turns = (proc.turns ??= new Turns(io));
	turns.io = io;

	const setTools = (allowed: string[]) => {
		const active = new Set(pi.getActiveTools());
		for (const t of TOOLS) {
			if (allowed.includes(t)) active.add(PREFIX + t);
			else active.delete(PREFIX + t);
		}
		pi.setActiveTools([...active]);
	};

	const harness = () => ({
		harness: "pi",
		mode: ctx?.mode === "tui" ? "interactive" : (ctx?.mode ?? "rpc"),
		harness_ref: ctx?.sessionManager.getSessionId(),
		tool_prefix: PREFIX,
		model: modelName(ctx?.model),
		thinking: pi.getThinkingLevel() ?? "", // pi's own level, as is: core only stores it
		// What piggery may do to this session: the abort push cancels the turn, a wake push
		// starts one, mail goes in as a steer, the role card goes in the system prompt (so a role
		// change is not repeated in mail text). A worker's are declared by its runtime driver.
		capabilities: ["abort", "wake", "steer", "system_prompt"],
	});

	// Tools and role card of the current identity (solo or a role).
	let protocolWarned = false;
	const apply = (res: any) => {
		roleCard = res.role_card ?? "";
		setTools(res.tools ?? []);
		inTeam = !!res.team_id;
		// An old daemon sends no version (0): a field this extension added is dropped until it restarts.
		// A solo is silent; the warning comes when it joins a team (found, admit, reopen).
		const daemon = res.protocol_version ?? 0;
		if (daemon !== PROTOCOL_VERSION && !protocolWarned && inTeam) {
			protocolWarned = true;
			log(
				`piggery: the daemon speaks protocol ${daemon}, this extension ${PROTOCOL_VERSION}: ` +
					(daemon < PROTOCOL_VERSION ? "run `piggery restart`" : "run `piggery setup pi`") +
					", then restart this session",
			);
		}
	};

	// cl is the client whose socket just connected (never whatever `client` points to now).
	const identify = async (cl: Client) => {
		const h = harness();
		if (!cl.auth) {
			// No PIGGERY_*: the daemon places this pi session: its old participant if it was in a
			// still-open team (resume), else a new solo participant.
			let res: any;
			try {
				// The session's own file, for top's ctx, turns and tail (none when pi does not save
				// the session): read by the CLI, never by the daemon.
				const file = ctx?.sessionManager.getSessionFile?.();
				const transcript = file ? { path: file, format: "pi" } : undefined;
				res = await cl.early("join.auto", { cwd: ctx?.cwd, ...h, transcript });
			} catch (err: any) {
				if (err?.code) {
					// Live elsewhere (invalid) or a headless worker's session (not_found): inert for
					// this process, silently. Transport errors have no code and are retried.
					proc.stale = true;
					cl.stop();
					setTools([]);
					return;
				}
				throw err;
			}
			cl.auth = proc.auth = { id: res.id, token: res.token };
			proc.authRef = h.harness_ref;
			proc.runId = res.run_id;
		}
		const newRun = !proc.runId;
		const args: Record<string, unknown> = { new_run: newRun, protocol_version: PROTOCOL_VERSION, ...h };
		// Same run: the turn ends the daemon missed while it was down go with identify; a new
		// run drops them (its old turns can never ack).
		if (newRun) turns.reset();
		else {
			args.run_id = proc.runId;
			if (turns.ended.length) args.ended = turns.ended;
		}
		const res: any = await cl.early("identify", args);
		turns.takeEnded();
		if (newRun) proc.runId = res.run_id;
		apply(res);
		if (notice) {
			deliver(notice, false);
			notice = undefined;
		}
	};

	// Same run, new identity (admitted, or founded a team): identify again for tools and role card.
	const reidentify = async () => {
		const res: any = await client!.call("identify", {
			new_run: false,
			run_id: proc.runId,
			protocol_version: PROTOCOL_VERSION,
			...harness(),
		});
		apply(res);
		return res;
	};

	// This process may no longer act for the participant (another process took it over, or the
	// token is bad): stop driving for the rest of the process, never re-identify as a new run.
	const retire = (err: any) => {
		if (afterRetire(err, !!workerRun) === "solo") return becomeSolo();
		proc.stale = true;
		setTools([]);
		log(
			err.rule_id === "run.stale"
				? "piggery: another process now owns this participant; this session stops driving it"
				: err.rule_id === "team.closed"
					? "piggery: left the team: team closed (team down)"
					: `piggery: ${err.message}; this session stops driving it`,
		);
	};

	// Team down (or this session's own close): drop the closed team's participant and let join.auto
	// place this same pi session again; the team is closed, so the daemon gives it a new solo
	// participant. After its own close the tool result says it, so no notice.
	const becomeSolo = (tell = true) => {
		client?.stop();
		client = undefined;
		envAuth = proc.envAuth = undefined;
		proc.auth = proc.authRef = proc.runId = undefined;
		turns.reset();
		inTeam = false;
		setTools(TOOLS);
		if (tell) notice = "[piggery] your team was closed (team down); you are solo now";
		startClient(undefined);
	};

	// The session's model or thinking level changed (/model, cycling): tell the daemon both, which
	// keeps them for workers this session spawns. Not identified yet: the next
	// identify carries them.
	const reportModel = (model?: { provider: string; id: string }, level?: string) => {
		if (client && proc.auth)
			client.call("harness.event", { event: "model_changed", model: modelName(model), thinking: level ?? "" }).catch(() => {});
	};
	pi.on("model_select", (e) => reportModel(e.model, pi.getThinkingLevel()));
	pi.on("thinking_level_select", (e, c) => reportModel(c?.model ?? ctx?.model, e.level));

	let clientRef: string | undefined; // pi session id the current client serves
	pi.on("session_start", (_e, c) => {
		const ref = c.sessionManager.getSessionId();
		ctx = c;
		// pi can fire session_start more than once for one session (seen after new_session):
		// keep the client already serving it. A different session replaces the client.
		if (client && clientRef === ref) return;
		client?.stop();
		client = undefined;
		clientRef = ref;
		setTools([]);
		if (!envAuth && proc.auth && proc.authRef !== ref) {
			// /new or /resume in the same process: the auto-joined participant belongs to the old
			// pi session. Never identify it with the new session; join again for this one.
			proc.auth = proc.authRef = proc.runId = undefined;
			turns.reset();
			inTeam = false;
		}
		// Until the daemon places this session it has the solo tools, so the model can call one
		// (which starts the daemon). A running daemon is joined now; a missing one is retried by the
		// client's reconnect loop, silently, and never started here.
		setTools(TOOLS);
		startClient(envAuth ?? proc.auth);
	});

	const startClient = (auth: { id: string; token: string } | undefined) => {
		const cl: Client = new Client({
			path: sockPath,
			auth,
			onPush: (f: any) => {
				if (f.event === "wake") turns.wake();
				// piggery -a abort: cancel the current turn, as Esc does. It ends with no "completed"
				// outcome, so the daemon acks nothing and its mail comes again.
				else if (f.event === "abort") ctx?.abort();
				else if (f.event === "role") {
					// Admitted into a team: same participant and run, new role.
					reidentify()
						.then((res) =>
							deliver(
								`[piggery] you were admitted to a team as ${res.name} (role ${res.role}); ` +
									"your piggery tools and role card are updated (see piggery_who)",
								false,
							),
						)
						.catch((err) => log(`piggery: ${err.message}`));
				}
				else if (f.event === "retire" && !proc.stale) {
					// team down: leave the team now, not at the next verb (which would be refused anyway).
					cl.stop();
					retire({ rule_id: f.reason, message: f.reason });
				}
			},
			onConnect: () => identify(cl),
			onReady: () => {
				// identify closed any turn left open: a run still going continues under a new key.
				// Idle: mail that may have arrived while disconnected or before this session started.
				if (turns.running) turns.resumeRunning();
				else turns.wake();
			},
			onFatal: retire,
			log,
		});
		client = cl;
		cl.start();
	};

	// A tool call needs the daemon: start it if it is not running (the model called a piggery tool,
	// so the human asked), then wait for the client to connect and identify.
	const ensureConnected = async () => {
		if (proc.stale) throw new Error("this pi session no longer drives a piggery participant");
		if (client?.ready) return;
		if (!existsSync(sockPath)) await startDaemon();
		client?.stop();
		startClient(envAuth ?? proc.auth);
		for (let i = 0; i < 100 && !client?.ready && !proc.stale; i++) await new Promise((r) => setTimeout(r, 100));
		if (!client?.ready) throw new Error("could not reach the piggery daemon");
	};

	const startDaemon = async () => {
		const dir = join(homedir(), ".piggery");
		mkdirSync(dir, { recursive: true, mode: 0o700 });
		// Like the CLI autostart: append to serve.log (0600), own session (detached = setsid), released.
		const log = join(dir, "serve.log");
		const out = openSync(log, "a", 0o600);
		const from = fstatSync(out).size;
		let exited = false; // serve ended before its socket appeared: it could not start
		await new Promise<void>((resolve, reject) => {
			const d = spawn("piggery", ["serve"], { detached: true, stdio: ["ignore", out, out] });
			d.on("error", (e: any) =>
				reject(new Error(e.code === "ENOENT" ? "the piggery binary is not on PATH (see https://github.com/sting8k/piggery/blob/main/docs/guide.md)" : e.message)),
			);
			d.on("exit", () => (exited = true));
			d.on("spawn", () => {
				d.unref();
				resolve();
			});
		}).finally(() => closeSync(out));
		for (let i = 0; i < 50 && !existsSync(sockPath) && !exited; i++) await new Promise((r) => setTimeout(r, 100));
		if (existsSync(sockPath)) return;
		// Its reason is the last line it wrote to the log (e.g. a bad config.yaml).
		const lines = readFileSync(log).subarray(from).toString("utf8").trim().split("\n");
		const why = (lines[lines.length - 1] ?? "").replace(/^piggery serve: /, "").trim() || "piggery serve exited";
		throw new Error(`the daemon did not start: ${why} (log: ${log})`);
	};

	pi.on("session_shutdown", async (e) => {
		if (!client) return;
		await turns.drain();
		if (e.reason === "quit") await client.call("harness.event", { event: "session_end" }).catch(() => {});
		client.stop();
		client = clientRef = undefined;
	});

	pi.on("before_agent_start", (e) => {
		if (roleCard) e.systemPromptOptions.sections = { ...e.systemPromptOptions.sections, piggery_role: roleCard };
	});
	// pi's events -> standard adapter events. pi awaits agent_before_settle, so a
	// blocked end (more mail) is steered in before pi decides whether to run on.
	pi.on("agent_start", () => turns.agentStart());
	pi.on("turn_start", () => turns.modelTurn());
	pi.on("turn_end", () => turns.toolBoundary());
	pi.on("agent_before_settle", async (e) => {
		await turns.beforeSettle(e.outcome);
	});
	pi.on("agent_settled", () => turns.settled());
	pi.on("ui_prompt_start", () => turns.uiPrompt(true));
	pi.on("ui_prompt_end", () => turns.uiPrompt(false));

	const text = (s: string) => ({ content: [{ type: "text" as const, text: s }], details: undefined });

	// builtin registers the tools.json tool name with what it does here.
	const builtin = (name: string, execute: (...a: any[]) => Promise<any>) => {
		const t = BUILTINS.find((b) => b.name === name)!;
		pi.registerTool({
			name: PREFIX + name,
			label: `piggery ${name}`,
			description: t.description.replace(/\{tool:([^{}]*)\}/g, (_, n) => PREFIX + n),
			parameters: Type.Unsafe(t.parameters),
			execute,
		});
	};

	builtin("send", async (_id, p) => {
		await ensureConnected();
		return text(sentText(await client!.call("send", p)));
	});

	builtin("inbox", async (_id, p) => {
		await ensureConnected();
		// No batch: in a turn the daemon records the delivery in that turn (like piggery mcp); a
		// view is read-only (no delivery).
		const msgs: any[] = await client!.call("inbox", p.view ? { view: p.view } : {});
		if (!msgs.length) return text(p.view ? "Nothing in this view." : "No new messages.");
		return text(p.view ? render(msgs, `view ${p.view}, ${msgs.length} message(s), nothing marked read`) : render(msgs));
	});

	builtin("who", async () => {
		await ensureConnected();
		const ps: any[] = await client!.call("who", undefined);
		return text(renderWho(ps, (envAuth ?? proc.auth)?.id));
	});

	builtin("agent", async (_id, p) => {
		await ensureConnected();
		if (p.action === "found" || p.action === "reopen") {
			const r: any = await client!.call(
				"agent",
				p.action === "found"
					? { action: "found", template: p.template || undefined }
					: { action: "reopen", team: p.team },
			);
			if (r.token) {
				// Another participant for this session: founded from inside a team (the old one
				// left), or reopened by the old gate's own session (its old participant is back).
				client!.auth = proc.auth = { id: r.participant_id, token: r.token };
				envAuth = proc.envAuth = undefined;
				proc.runId = r.run_id;
				turns.reset();
			}
			const res = await reidentify();
			const tools = "your tools: " + (res.tools ?? []).map((t: string) => PREFIX + t).join(", ");
			if (p.action === "reopen") return text(`${r.text}\n${tools}`);
			return text(`founded team ${r.team_name}; you are ${res.name} (${res.role}), its gate; ${tools}`);
		}
		if (p.action === "templates") return text((await client!.call("agent", { action: "templates" }) as any).text);
		if (!inTeam && !(p.action === "spawn" && p.template) && !(p.action === "close" && p.team) && !p.action.startsWith("gate_")) throw new Error("you are a solo session: only actions templates, found, reopen, gate_close, gate_open, and spawn with template or close with team for a taskforce");
		const r: any = await client!.call("agent", p);
		if (p.action === "close" && !p.team) {
			// The team is closed and this participant left it (no retire push to the caller).
			becomeSolo(false);
			const failed = r.failed?.length ? `; could not stop: ${r.failed.join(", ")}` : "";
			return text(
				`closed team ${r.team_name}` +
					(r.stopped?.length ? `; stopped ${r.stopped.join(", ")}` : "") +
					`${failed}. You are solo now; your tools: ${TOOLS.map((t) => PREFIX + t).join(", ")}`,
			);
		}
		if (p.action === "close") return text(`closed taskforce ${r.team_name}` + (r.stopped?.length ? `; stopped ${r.stopped.join(", ")}` : ""));
		if (p.action === "tail") return text((r.records ?? []).map((x: unknown) => JSON.stringify(x)).join("\n") || "(no output)");
		if (r.exit) return text(`stopped (exit ${JSON.stringify(r.exit)})`);
		// Names and #N only, no ids.
		if (p.action === "spawn") return text(p.template ? `called up taskforce ${r.team_name}; its task is #${r.task_seq}; write to it as ${r.team_name}, its result comes to you as mail` : `spawned ${p.name}; its task is #${r.task_seq} (its reply comes to you as mail)`);
		if (p.action === "resume" && r.task_seq) return text(`resumed ${p.target}; its task is #${r.task_seq} (its reply comes to you as mail)`);
		if (p.action === "gate_close" || p.action === "gate_open") return text(r.text);
		if (p.action === "admit") return text(`admitted ${p.target} as ${p.role}`);
		return text(`${p.action} ok: ${p.target}`);
	});
}
