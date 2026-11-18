// pi's events (and omp's: extensions/omp maps its own onto these methods) as the standard adapter events. Pure: no pi, no socket.
//
// The daemon counts batches and applies the ack rule for every harness; this module only
// maps pi's events to `harness.event` calls and does what the answer says: mail text goes to the
// model natively (pi.sendUserMessage: a new turn when idle, a steer while running). A turn is one
// agent_start…agent_settled; its key is an id made here and sent with every event of the turn.
// Calls go out one at a time, in event order.

/** The text of a harness error: a JSON api error gives "<type>: <message>" (either alone if only one is there), anything else as it is. */
export function errorText(e) {
	const s = typeof e === "string" ? e : (e?.message ?? "");
	try {
		const j = JSON.parse(s);
		const e = j?.error ?? j;
		return [e?.type, e?.message].filter((x) => typeof x === "string" && x).join(": ") || s;
	} catch {
		return s;
	}
}

const OUTCOME = { completed: "ok", aborted: "interrupted", error: "failed" };

export class Turns {
	/**
	 * @param {object} io
	 * @param {(args: object) => Promise<{text?: string, block?: boolean, part?: number}>} io.event harness.event
	 * @param {(event: string) => Promise<any>} io.presence presence events with no standard event
	 * @param {(text: string, steer: boolean) => void | Promise<void>} io.deliver shows mail to the model;
	 *   a steer resolves once the harness has it queued
	 * @param {() => string} io.newKey a fresh turn key
	 * @param {() => boolean} [io.pending] input is queued in the harness (pi: a queued steer makes
	 *   the run go on past before_settle)
	 * @param {boolean} [io.reads] the harness calls modelTurn() when a model turn starts (omp, dsh), so
	 *   an ok stop does not end a turn whose steered mail no model turn has read yet. A harness with
	 *   neither (opencode) settles whatever is queued.
	 * @param {(err: Error) => void} io.onError
	 */
	constructor(io) {
		this.io = io;
		this.q = Promise.resolve();
		this.running = false; // pi is in a run (agent_start…settled)
		this.reset();
	}

	/**
	 * A new run (or a new participant): nothing of the old run can be ended any more. `running` stays:
	 * it says the harness is in a run, whoever the participant is; resumeRunning (after identify)
	 * opens that run's turn for the new one, and settled() is what ends it.
	 */
	reset() {
		this.key = null; // the turn in progress (or the one an idle wake opened)
		this.held = false; // a wake came after this run's turn ended, before pi settled
		this.lastError = ""; // the error text of the last model reply that failed, for a failed turn_end
		this.ended = []; // turn_ends the daemon did not get (it was down), oldest first
		this.unread = false; // mail was steered in and no model turn has started since
		this.parts = { key: null, given: [], inContext: 0, sent: 0 }; // gives of the turn `key`
	}

	/** The part bookkeeping of the turn in progress; a new key starts it again. */
	turnParts() {
		if (this.parts.key !== this.key) this.parts = { key: this.key, given: [], inContext: 0, sent: 0 };
		return this.parts;
	}

	enqueue(fn) {
		this.q = this.q.then(fn).catch((err) => this.io.onError(err));
		return this.q;
	}

	/** Resolves when every queued call has finished (tests, shutdown). */
	drain() {
		return this.q;
	}

	/** The turn_ends to send with identify of the same run; cleared once identify took them. */
	takeEnded() {
		const e = this.ended;
		this.ended = [];
		return e;
	}

	async give(args, steer) {
		const r = await this.io.event(args);
		if (r?.text) {
			if (r.part && args.prompt_id === this.key) this.turnParts().given.push({ part: r.part, text: r.text });
			await this.io.deliver(r.text, steer);
			if (steer) this.unread = true;
		}
		return r;
	}

	/**
	 * A user message entered the model's context (pi's message_end): when it is a give of this turn,
	 * the next model turn that ends well has read it (seen). Text no give matches changes nothing.
	 */
	messageIn(text) {
		const p = this.turnParts();
		const hit = p.given.find((g) => g.text === text);
		if (!hit) return;
		p.inContext = Math.max(p.inContext, hit.part);
		p.given = p.given.filter((g) => g.part > hit.part);
	}

	/** A model turn starts inside the run (pi's turn_start, omp's turn_start, dsh's step/start): mail steered before it is in it. */
	modelTurn() {
		this.unread = false;
	}

	agentStart() {
		// pi runs again before settling (auto-retry, compaction, or a steer that keeps the run
		// going): the same turn.
		if (this.running) return;
		this.running = true;
		const key = (this.key ??= this.io.newKey());
		this.enqueue(() => this.give({ event: "turn_start", prompt_id: key }, true));
	}

	/**
	 * The model's reply ended (pi's assistant message_end): ok (not error or aborted) means it read
	 * every give in its context, so the daemon acks them now, before the reply's tools run.
	 */
	answered(ok, error = "") {
		this.lastError = ok ? "" : errorText(error);
		const p = this.turnParts();
		if (this.key && ok && p.inContext > p.sent) this.toolBoundary(true);
	}

	/**
	 * Tool results are in (pi's turn_end): mail can be steered into the running turn. modelOk: the model
	 * turn that just ended finished without error or abort, so the gives in its context are seen and
	 * the daemon acks them; a harness that does not say keeps acking at the turn's end. Resolves once
	 * the mail is delivered: pi awaits it, so the steer is in its queue when it looks next.
	 */
	toolBoundary(modelOk = false) {
		const key = this.key;
		if (!key) return;
		const args = { event: "tool_boundary", prompt_id: key };
		const p = this.turnParts();
		if (modelOk && p.inContext > p.sent) args.seen = p.sent = p.inContext;
		return this.enqueue(() => this.give(args, true));
	}

	/**
	 * pi's last actionable boundary. Awaited by pi: mail the daemon hands back (block) is steered
	 * in and pi keeps running the same turn; its next before_settle ends it again.
	 */
	beforeSettle(outcome, final = false, error = "") {
		const key = this.key;
		const o = OUTCOME[outcome];
		if (!key || !o) return this.q;
		return this.enqueue(async () => {
			// Input still queued (mail steered at the last boundary, or the Human typed): pi runs on
			// with it in this same turn, so this is not its end; the next before_settle is. Should
			// pi settle instead, settled() ends the turn interrupted (no ack, the mail comes again).
			// With io.reads (omp, dsh) and no io.pending an ok stop does the same for mail no model turn has read:
			// the step or run after the stop hook shows it, and its own stop acks the turn. Any other
			// outcome, and a run that ended for good (final), ends the turn now.
			const unseen = this.io.pending ? this.unread || this.io.pending() : this.io.reads && o === "ok" && this.unread;
			if (!final && unseen) return;
			try {
				const text = o === "failed" ? errorText(error) || this.lastError : "";
				const r = await this.io.event({ event: "turn_end", prompt_id: key, outcome: o, ...(text && { error: text }) });
				if (r?.block && r.text) {
					await this.io.deliver(r.text, true);
					return;
				}
			} catch (err) {
				if (!err?.notConnected) throw err;
				this.ended.push({ key, outcome: o }); // told to the daemon at the next identify
			}
			if (this.key === key) this.key = null;
		});
	}

	/**
	 * A run ended for good with no awaited hook before it (omp: a terminal agent_end that no
	 * session_stop preceded, as after an abort or a compaction dead end). The turn ends as
	 * beforeSettle ends it, with outcome "completed" | "aborted" | "error"; mail the turn had not
	 * seen blocks that end (the text is shown, a run with it follows and it is the same turn, so
	 * nothing settles); else the run settles. pi does not call this. Steered mail no model turn
	 * read ends the turn interrupted (the ack waits for a turn that shows it).
	 */
	async endRun(outcome, error = "") {
		await this.beforeSettle(this.io.reads && this.unread ? "aborted" : outcome, true, error);
		if (this.key) return;
		this.settled();
	}

	settled() {
		this.running = false;
		this.unread = false;
		// An abort (Esc, piggery abort, rpc compact) has no before_settle: the turn is over all the
		// same. End it interrupted: nothing is acked and nothing wakes, but its batch closes, so the
		// next mail wakes this session again.
		const key = this.key;
		this.key = null;
		if (key) this.enqueue(() => this.io.event({ event: "turn_end", prompt_id: key, outcome: "interrupted" }));
		this.enqueue(() => this.io.presence("agent_settled"));
		if (this.held) {
			this.held = false;
			this.wake();
		}
	}

	uiPrompt(start) {
		this.enqueue(() => (start ? this.io.event({ event: "permission_wait" }) : this.io.presence("ui_prompt_end")));
	}

	/**
	 * The daemon's wake push (or a reconnect): mail for this session. Running: steer it in at once.
	 * Idle: ask for a turn for it; its text starts a new pi turn, which keeps this key. No mail left
	 * (read elsewhere): no turn was opened, nothing is ended.
	 */
	wake() {
		return this.enqueue(async () => {
			if (this.running) {
				// The turn already ended (the daemon wakes when mail is left after an ok end): the
				// mail needs a turn of its own, once pi has settled.
				if (this.key) await this.give({ event: "tool_boundary", prompt_id: this.key }, true);
				else this.held = true;
				return;
			}
			// A mail check: the daemon opens a turn only when it has something to give, so no mail is
			// no turn (a reconnect must not look like one). A key from an earlier wake is a turn it
			// opened: that one is ended.
			const fresh = !this.key;
			const key = (this.key ??= this.io.newKey());
			const r = await this.give({ event: "turn_start", prompt_id: key, wake: true }, false);
			if (!r?.text && !this.running && this.key === key) {
				this.key = null;
				if (!fresh) await this.io.event({ event: "turn_end", prompt_id: key, outcome: "interrupted" });
			}
		});
	}

	/** After a reconnect or a change of participant mid-run: go on as the current one under a new key. */
	resumeRunning() {
		if (!this.running) return;
		this.key = this.io.newKey();
		const key = this.key;
		this.enqueue(() => this.give({ event: "turn_start", prompt_id: key }, true));
	}
}
