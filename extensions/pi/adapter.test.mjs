// What the pi adapter still decides: pi events -> standard events, and how the daemon's
// mail text is shown (steer vs a new turn). Batches and acks are the daemon's (Go contract test).
import assert from "node:assert/strict";
import test from "node:test";
import { Turns } from "./adapter.mjs";

// A fake io: records harness.event calls and deliveries; answer(args) is the daemon's reply.
function setup(answer = () => ({})) {
	const calls = [];
	const shown = [];
	let n = 0;
	const turns = new Turns({
		event: async (a) => {
			calls.push(a);
			return answer(a);
		},
		presence: async (e) => calls.push({ presence: e }),
		deliver: (text, steer) => shown.push({ text, steer }),
		newKey: () => `k${++n}`,
		onError: (err) => {
			throw err;
		},
	});
	return { turns, calls, shown };
}

test("a pi run maps to one keyed turn: start, tool boundaries, end by outcome; abort ends interrupted at settle", async () => {
	const { turns, calls } = setup();
	turns.agentStart();
	turns.toolBoundary();
	turns.agentStart(); // auto-retry before settle: same turn
	await turns.beforeSettle("completed");
	turns.settled();
	for (const [outcome, want] of [["aborted", "interrupted"], ["error", "failed"]]) {
		turns.agentStart();
		await turns.beforeSettle(outcome);
		turns.settled();
		await turns.drain();
		assert.deepEqual(calls.at(-2), { event: "turn_end", prompt_id: calls.at(-3).prompt_id, outcome: want });
	}
	turns.agentStart(); // Esc/abort: pi settles with no before_settle
	turns.settled();
	await turns.drain();
	assert.deepEqual(calls.slice(0, 4), [
		{ event: "turn_start", prompt_id: "k1" },
		{ event: "tool_boundary", prompt_id: "k1" },
		{ event: "turn_end", prompt_id: "k1", outcome: "ok" },
		{ presence: "agent_settled" },
	]);
	assert.deepEqual(calls.slice(-3), [
		{ event: "turn_start", prompt_id: "k4" },
		{ event: "turn_end", prompt_id: "k4", outcome: "interrupted" },
		{ presence: "agent_settled" },
	]);
});

test("mail text: steered while running; an idle wake starts a new turn that keeps the wake's key", async () => {
	const { turns, calls, shown } = setup((a) => (a.event === "turn_end" ? {} : { text: `mail for ${a.event}` }));
	await turns.wake(); // idle
	assert.deepEqual(shown, [{ text: "mail for turn_start", steer: false }]);
	turns.agentStart(); // the turn that mail started
	await turns.wake(); // running: into this turn
	await turns.drain();
	assert.deepEqual(calls.map((c) => c.prompt_id), ["k1", "k1", "k1"]);
	assert.deepEqual(calls.at(-1), { event: "tool_boundary", prompt_id: "k1" });
	assert.deepEqual(shown.slice(1), [
		{ text: "mail for turn_start", steer: true },
		{ text: "mail for tool_boundary", steer: true },
	]);
});

test("a wake after the turn ended but before pi settled opens the next turn after the settle", async () => {
	const { turns, calls } = setup();
	turns.agentStart();
	await turns.beforeSettle("completed");
	await turns.wake(); // the daemon woke: mail left after the ok end
	assert.equal(calls.length, 2);
	turns.settled();
	await turns.drain();
	assert.deepEqual(calls.slice(2), [
		{ presence: "agent_settled" },
		{ event: "turn_start", prompt_id: "k2" },
		{ event: "turn_end", prompt_id: "k2", outcome: "interrupted" },
	]);
});

test("an idle wake with no mail left ends its turn unacked", async () => {
	const { turns, calls, shown } = setup();
	await turns.wake();
	assert.deepEqual(calls, [
		{ event: "turn_start", prompt_id: "k1" },
		{ event: "turn_end", prompt_id: "k1", outcome: "interrupted" },
	]);
	assert.equal(shown.length, 0);
});

test("a blocked end steers the mail and the turn goes on; an end the daemon missed waits for identify", async () => {
	let block = true;
	const { turns, calls, shown } = setup((a) => {
		if (a.event !== "turn_end") return {};
		if (block) return (block = false), { block: true, text: "more mail" };
		const err = new Error("daemon not connected");
		err.notConnected = true;
		throw err;
	});
	turns.agentStart();
	await turns.beforeSettle("completed"); // blocked: pi runs on
	assert.deepEqual(shown, [{ text: "more mail", steer: true }]);
	await turns.beforeSettle("completed"); // the daemon is down
	turns.settled();
	await turns.drain();
	assert.deepEqual(calls.filter((c) => c.event === "turn_end").map((c) => c.prompt_id), ["k1", "k1"]);
	assert.deepEqual(turns.takeEnded(), [{ key: "k1", outcome: "ok" }]);
	assert.deepEqual(turns.ended, []);
});
