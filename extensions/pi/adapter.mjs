// pi's events as the standard adapter events. Pure: no pi, no socket.
//
// The daemon counts batches and applies the ack rule for every harness; this module only
// maps pi's events to `harness.event` calls and does what the answer says: mail text goes to the
// model natively (pi.sendUserMessage: a new turn when idle, a steer while running). A turn is one
// agent_start…agent_settled; its key is an id made here and sent with every event of the turn.
// Calls go out one at a time, in event order.

const OUTCOME = { completed: "ok", aborted: "interrupted", error: "failed" };

export class Turns {
	/**
	 * @param {object} io
	 * @param {(args: object) => Promise<{text?: string, block?: boolean}>} io.event harness.event
	 * @param {(event: string) => Promise<any>} io.presence presence events with no standard event
	 * @param {(text: string, steer: boolean) => void} io.deliver shows mail to the model
	 * @param {() => string} io.newKey a fresh turn key
	 * @param {(err: Error) => void} io.onError
	 */
	constructor(io) {
		this.io = io;
		this.q = Promise.resolve();
		this.reset();
	}

	/** A new run (or a new participant): nothing of the old run can be ended any more. */
	reset() {
		this.key = null; // the turn in progress (or the one an idle wake opened)
		this.running = false;
		this.held = false; // a wake came after this run's turn ended, before pi settled
		this.ended = []; // turn_ends the daemon did not get (it was down), oldest first
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
		if (r?.text) this.io.deliver(r.text, steer);
		return r;
	}

	agentStart() {
		// pi runs again before settling (auto-retry, compaction, or a steer that keeps the run
		// going): the same turn.
		if (this.running) return;
		this.running = true;
		const key = (this.key ??= this.io.newKey());
		this.enqueue(() => this.give({ event: "turn_start", prompt_id: key }, true));
	}

	/** Tool results are in (pi's turn_end): mail can be steered into the running turn. */
	toolBoundary() {
		const key = this.key;
		if (key) this.enqueue(() => this.give({ event: "tool_boundary", prompt_id: key }, true));
	}

	/**
	 * pi's last actionable boundary. Awaited by pi: mail the daemon hands back (block) is steered
	 * in and pi keeps running the same turn; its next before_settle ends it again.
	 */
	beforeSettle(outcome) {
		const key = this.key;
		const o = OUTCOME[outcome];
		if (!key || !o) return this.q;
		return this.enqueue(async () => {
			try {
				const r = await this.io.event({ event: "turn_end", prompt_id: key, outcome: o });
				if (r?.block && r.text) {
					this.io.deliver(r.text, true);
					return;
				}
			} catch (err) {
				if (!err?.notConnected) throw err;
				this.ended.push({ key, outcome: o }); // told to the daemon at the next identify
			}
			if (this.key === key) this.key = null;
		});
	}

	settled() {
		this.running = false;
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
	 * Idle: open a turn for it; its text starts a new pi turn, which keeps this key. No mail left
	 * (read elsewhere): end that turn, nothing acked.
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
			const key = (this.key ??= this.io.newKey());
			const r = await this.give({ event: "turn_start", prompt_id: key }, false);
			if (!r?.text && !this.running && this.key === key) {
				this.key = null;
				await this.io.event({ event: "turn_end", prompt_id: key, outcome: "interrupted" });
			}
		});
	}

	/** After a reconnect mid-run: the daemon closed the old turn; go on under a new key. */
	resumeRunning() {
		if (!this.running) return;
		this.key = this.io.newKey();
		const key = this.key;
		this.enqueue(() => this.give({ event: "turn_start", prompt_id: key }, true));
	}
}
