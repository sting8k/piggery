// Model-facing text and tool helpers of the pi adapter: mail, who, a send
// result, declarative tools. The same rendering as core/render.go (RenderMail, RenderWho, SentText),
// which the daemon uses for the mail it hands back; this copy renders the inbox tool's answer.

/**
 * Renders messages for the model. The header is stamped by the daemon (from_label). `heading`
 * replaces the default "N new messages" line (read-only views are not new mail).
 */
export function render(msgs, heading, now = Date.now()) {
	// No ULIDs for the model: messages are #seq, people are names.
	const parts = msgs.map((m) => {
		let attrs = `id="#${m.seq}" from="${m.from_label}" sender="${m.from_name}"`;
		if (m.kind) attrs += ` kind="${m.kind}"`;
		if (m.reply_to) attrs += ` reply_to="#${m.reply_to_seq}"`;
		if (m.cc_of) attrs += ` cc_of="#${m.cc_of_seq}" cc_of_mail_to="${m.cc_to}"`; // a routing cc copy
		attrs += ` at="${sentAt(m.created_at, now)}"`;
		if (now - m.created_at > 60_000) attrs += ` age="${age(now - m.created_at)}"`; // held, or shown again
		if (m.redelivered) attrs += ` redelivered="true"`; // given before and not acked: whatever its age
		return `<message ${attrs}>\n${m.body}\n</message>`;
	});
	return (
		`[piggery] ${heading ?? `${msgs.length} new message${msgs.length === 1 ? "" : "s"}`}:\n\n${parts.join("\n\n")}\n\n` +
		`To reply, use the piggery_send tool with to=<sender> and reply_to=<id> (the #N).`
	);
}

const pad = (n) => String(n).padStart(2, "0");

/** A send time in local time: `14:02:11` today, `2026-09-26 23:58` another day. */
function sentAt(ms, now) {
	const d = new Date(ms);
	const time = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
	if (d.toDateString() === new Date(now).toDateString()) return `${time}:${pad(d.getSeconds())}`;
	return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}`;
}

/** A duration, short: `4m`, `2h5m`, `3d2h`. */
function age(ms) {
	const m = Math.floor(ms / 60_000);
	if (m < 60) return `${m}m`;
	if (m < 24 * 60) return `${Math.floor(m / 60)}h${m % 60 ? `${m % 60}m` : ""}`;
	const h = Math.floor(m / 60);
	return `${Math.floor(h / 24)}d${h % 24 ? `${h % 24}h` : ""}`;
}

/** The model-facing result of a send (send tool and declarative tools alike). */
export function sentText(r) {
	const ref = `#${r.seq}`;
	if (r.duplicate) return `duplicate of ${ref}`;
	if (r.held) return `stored ${ref} but HELD by ${r.rule_id}: not delivered until an admin releases it`;
	return `sent ${ref}`;
}

/**
 * Renders `who` for the model: the caller's team in full, then one line per other
 * team and per solo session. Teams and solos talk gate to gate; a team name addresses its gate.
 */
export function renderWho(ps, selfId) {
	// Names are addresses; an id only tells apart two listed people with the same name.
	const seen = new Map();
	for (const p of ps) if (p.kind !== "team") seen.set(p.name, (seen.get(p.name) ?? 0) + 1);
	const id = (p) => (seen.get(p.name) > 1 ? ` id=${p.id}` : "");
	const members = ps.filter((p) => p.kind === "member");
	const teams = ps.filter((p) => p.kind === "team");
	const solos = ps.filter((p) => p.kind === "solo");
	const out = [];
	if (members.length) {
		out.push(`Your team ${members[0].team}:`);
		for (const p of members)
			out.push(`  ${p.name} (${p.role}) ${p.state}${p.gate ? " [gate]" : ""}${p.id === selfId ? " (you)" : ""}${id(p)}`);
	}
	if (teams.length) {
		out.push("Other teams (write to the team name; it reaches the gate):");
		for (const t of teams) out.push(`  ${t.name} (root ${t.cwd}) gate ${t.gate_name || "none live"}`);
	}
	if (solos.length) {
		out.push("Solo sessions (each is its own gate):");
		for (const p of solos)
			out.push(`  ${p.name} (cwd ${p.cwd}) ${p.state}${p.admittable ? " [admittable]" : ""}${p.id === selfId ? " (you)" : ""}${id(p)}`);
	}
	return out.join("\n") || "Nobody else is on piggery.";
}

/**
 * What a refusal that ends this participant means for the session: after team
 * down an interactive session becomes solo in the same pi session; a headless worker, a
 * superseded run (run.stale) or a bad token stops driving for good.
 */
export function afterRetire(err, isWorker) {
	return err?.rule_id === "team.closed" && !isWorker ? "solo" : "stale";
}

