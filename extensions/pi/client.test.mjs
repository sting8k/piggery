import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fakeDaemonPath } from "../testutil/daemon.mjs";
import { Client } from "./client.mjs";

test("a run.stale answer to any call stops the client for good", async () => {
	const dir = mkdtempSync(join(tmpdir(), "pgc"));
	const path = fakeDaemonPath(dir);
	let connections = 0;
	// Identify succeeds; every later call is answered as from a superseded run.
	const server = net.createServer((c) => {
		c.on("error", () => {}); // a client that has gone makes a write fail (EPIPE on a Windows pipe): not the fake's concern
		connections++;
		let buf = "";
		c.on("data", (d) => {
			buf += d;
			let i;
			while ((i = buf.indexOf("\n")) >= 0) {
				const req = JSON.parse(buf.slice(0, i));
				buf = buf.slice(i + 1);
				const res =
					req.verb === "identify"
						? { id: req.id, ok: true, result: {} }
						: { id: req.id, error: { code: "unauthorized", message: "stale", rule_id: "run.stale", layer: "token" } };
				c.write(JSON.stringify(res) + "\n");
			}
		});
	});
	await new Promise((r) => server.listen(path, r));

	const fatal = [];
	let ready;
	const isReady = new Promise((r) => (ready = r));
	const client = new Client({
		path,
		auth: { id: "p", token: "t" },
		onPush: () => {},
		onConnect: () => client.early("identify", { new_run: true }),
		onReady: ready,
		onFatal: (err) => fatal.push(err.rule_id),
		log: () => {},
	});
	client.start();
	await isReady;
	await assert.rejects(client.call("presence", { event: "agent_start" }), (err) => err.fatal === true);
	await new Promise((r) => setTimeout(r, 1500)); // past the first reconnect delay
	assert.deepEqual(fatal, ["run.stale"]);
	assert.equal(client.stopped, true);
	assert.equal(connections, 1);
	server.close();
	rmSync(dir, { recursive: true, force: true });
});
