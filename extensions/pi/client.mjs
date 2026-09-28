// JSON-lines client for the piggery daemon socket. No pi imports.
// Responses carry the request `id`; server pushes carry `event` and no `id`.
import net from "node:net";

export class Client {
	/**
	 * @param {object} o
	 * @param {string} o.path unix socket path
	 * @param {{id: string, token: string}} o.auth
	 * @param {(frame: object) => void} o.onPush
	 * @param {() => Promise<void>} o.onConnect runs first on every connection (identify); calls made
	 *   through `early` inside it go out before the connection is marked ready
	 * @param {() => void} o.onReady runs after onConnect succeeded and the connection is ready
	 * @param {(err: Error) => void} o.onFatal a response said this process may no longer act for the
	 *   participant (rule run.stale, or unauthorized); the client has stopped and will not reconnect
	 * @param {(msg: string) => void} o.log
	 */
	constructor(o) {
		Object.assign(this, o);
		this.sock = null;
		this.ready = false;
		this.stopped = true;
		this.next = 0;
		this.pending = new Map();
		this.delay = 500;
		this.warned = false;
		this.timer = null;
	}

	start() {
		this.stopped = false;
		this.connect();
	}

	stop() {
		this.stopped = true;
		clearTimeout(this.timer);
		this.sock?.destroy();
	}

	connect() {
		const s = net.createConnection(this.path);
		let buf = Buffer.alloc(0);
		s.on("connect", async () => {
			this.sock = s;
			this.delay = 500;
			this.warned = false;
			try {
				await this.onConnect();
				if (this.stopped || this.sock !== s) return;
				this.ready = true;
				this.onReady();
			} catch (err) {
				if (!err.fatal && !err.notConnected) this.log(`piggery: ${err.message}`);
				s.destroy();
			}
		});
		s.on("data", (chunk) => {
			buf = Buffer.concat([buf, chunk]);
			let i;
			while ((i = buf.indexOf(10)) >= 0) {
				const line = buf.subarray(0, i).toString("utf8");
				buf = buf.subarray(i + 1);
				if (line.trim()) this.frame(line);
			}
		});
		s.on("error", (err) => {
			if (!this.warned) {
				this.warned = true;
				this.log(`piggery: daemon unavailable (${err.code ?? err.message}); pi keeps working, retrying`);
			}
		});
		s.on("close", () => {
			if (this.sock === s) {
				this.sock = null;
				this.ready = false;
			}
			for (const p of this.pending.values()) p.reject(notConnected("disconnected"));
			this.pending.clear();
			if (this.stopped) return;
			this.timer = setTimeout(() => this.connect(), this.delay);
			this.delay = Math.min(this.delay * 2, 10_000);
		});
	}

	frame(line) {
		let f;
		try {
			f = JSON.parse(line);
		} catch {
			return;
		}
		if (f.id === undefined || f.id === "") {
			if (f.event) this.onPush(f);
			return;
		}
		const p = this.pending.get(f.id);
		if (!p) return;
		this.pending.delete(f.id);
		if (!f.error) return p.resolve(f.result);
		const err = new Error(`${f.error.code}: ${f.error.message}`);
		Object.assign(err, { code: f.error.code, rule_id: f.error.rule_id, layer: f.error.layer });
		err.fatal = f.error.rule_id === "run.stale" || f.error.code === "unauthorized";
		p.reject(err);
		if (err.fatal && !this.stopped) {
			this.stop();
			this.onFatal(err);
		}
	}

	/** Calls a verb on a ready (identified) connection. */
	call(verb, args) {
		if (!this.ready) return Promise.reject(notConnected("daemon not connected"));
		return this.early(verb, args);
	}

	/** Calls a verb as soon as the socket is open (only for onConnect). */
	early(verb, args) {
		const s = this.sock;
		if (!s) return Promise.reject(notConnected("daemon not connected"));
		const id = String(++this.next);
		return new Promise((resolve, reject) => {
			this.pending.set(id, { resolve, reject });
			s.write(JSON.stringify({ id, verb, auth: this.auth, args }) + "\n");
		});
	}
}

/** A call failed only because there is no connection; the outage itself is logged once. */
function notConnected(msg) {
	return Object.assign(new Error(msg), { notConnected: true });
}
