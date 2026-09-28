// What the plugin shows: piggery top's list, sidebar and events, read from `piggery ps --json` and
// `piggery tail`. The grouping (projects, teams and solos in display order, members as the
// reports_to tree) comes from piggery itself; nothing here groups or sorts. No imports, so the app,
// the daemon and `node --test` all load it as is.

/** The ps --json protocol_version this plugin was written against; another one is shown, not refused. */
export const PROTOCOL_VERSION = 1;

export interface Member {
  id: string;
  name: string;
  role?: string;
  state: string;
  headless?: boolean;
  gate?: boolean;
  harness?: string;
  model?: string;
  thinking?: string;
  unacked?: number;
  created_at?: number;
  state_since?: number;
  last_turn_end?: number;
  reports_to?: string;
  spawned_by?: string;
  cwd?: string;
}

export interface Team {
  id: string;
  name: string;
  root?: string;
  gate?: string;
  held?: number;
  members: Member[];
}

export type Solo = Omit<Member, "role" | "headless" | "gate" | "reports_to" | "spawned_by" | "last_turn_end" | "thinking">;

/** A team top lists as closed: the daemon's closed[] entry. */
export interface ClosedTeam extends Team {
  closed_at?: number;
  closed_by?: string;
}

export interface Unit {
  /** "closed" is a team closed within the last hour, listed like top's All tab. */
  kind: "team" | "solo" | "closed";
  id: string;
  cwd?: string;
  /** The members' tree; ctx (tokens) and turns only for a worker top shows them for. */
  members?: { id: string; depth: number; prefix: string; cwd: string; ctx?: number; turns?: number }[];
}

export interface Project {
  label: string;
  path: string;
  units: Unit[];
}

export interface Event {
  seq: number;
  ts: number;
  type: string;
  participant?: string | null;
  ref_id?: string | null;
  team_id?: string | null;
}

export interface Ps {
  protocol_version?: number;
  teams?: Team[] | null;
  closed?: ClosedTeam[] | null;
  solos?: Solo[] | null;
  projects?: Project[] | null;
  events?: Event[] | null;
  names?: Record<string, string> | null;
}

export type Status = "working" | "idle" | "waiting" | "gone";

/** A state's look and word, as piggery top shows them: the word it says and the kind of icon. */
export function stateLook(state: string): { status: Status; word: string } {
  switch (state) {
    case "working":
      return { status: "working", word: "working" };
    case "idle":
      return { status: "idle", word: "idle" };
    case "awaiting_permission":
      return { status: "waiting", word: "waiting" };
    case "gone":
      return { status: "gone", word: "gone" };
  }
  return { status: "waiting", word: state };
}

/** One row of top's list: a team member or a solo session, with every fact top's sidebar has. */
export interface Row {
  id: string;
  name: string;
  /** Tree depth under its team (0 for the gate or a solo). */
  depth: number;
  /** The role; "solo" for a session outside any team. */
  role: string;
  state: string;
  /** "pi", or "pi·worker" for a headless worker, as top. */
  harness: string;
  /** The harness as piggery names it ("pi", "claude", "codex", "cli"), "" when unknown: for its mark. */
  harnessId: string;
  model: string;
  /** The full model and thinking level, as top's Overview ("zai/glm-5.3 · high"). */
  modelFull: string;
  /** Context tokens ("12.3k") and turns over every run; "" where top shows "-". */
  ctx: string;
  turns: string;
  unacked: number;
  created?: number;
  since?: number;
  lastTurn?: number;
  /** Relative to the project, "" in the project directory itself. */
  cwd: string;
  /** Dim like top: gone, or under a gone lead. */
  dim: boolean;
  /** Its team's name, null for a solo. */
  team: string | null;
  /** "pi headless worker · gate", "pi session", "solo". */
  kind: string;
  /** "spawned 14:02 by boss", "joined 09:10". */
  came: string;
  reports: string;
  /** Where it runs: the team's root, or the solo's directory. */
  root: string;
  /** A headless worker, whose driver log piggery tails; a session has none. */
  logged: boolean;
}

export interface UnitView {
  kind: "team" | "solo" | "closed";
  id: string;
  /** The team's name; null for a solo, whose one row carries its name. */
  title: string | null;
  /** The team's gate (its name), "" when it has none. */
  gate: string;
  held: number;
  /** A closed team's when and by whom (an empty `closedBy` is the admin). */
  closedAt?: number;
  closedBy?: string;
  rows: Row[];
}

export interface ProjectView {
  /** The directory as ps shortens it, without the leading "…/". */
  title: string;
  path: string;
  units: UnitView[];
}

/** A model as ps shows it: the id without its provider ("zai/glm-5.3" is "glm-5.3"). */
export function modelId(model: string | undefined): string {
  if (!model) return "";
  const slash = model.indexOf("/");
  return slash < 0 ? model : model.slice(slash + 1);
}

function harnessLabel(harness: string | undefined, headless: boolean | undefined): string {
  if (!harness) return "";
  return headless ? `${harness}·worker` : harness;
}

/** Tokens as top shows them: 950, 12.3k, 1.2M. */
export function tokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

function modelLabel(model: string | undefined, thinking: string | undefined): string {
  if (model && thinking) return `${model} · ${thinking}`;
  if (thinking) return `thinking ${thinking}`;
  return model ?? "";
}

function clock(ms: number | undefined): string {
  if (!ms) return "";
  const d = new Date(ms);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

/**
 * The projects in ps order, each unit with its rows. `dir` keeps only the projects that are that
 * directory, inside it or around it (the Farm panel of one workspace). A unit piggery lists but whose
 * team or solo is missing from the same snapshot is skipped.
 */
export function farm(ps: Ps, dir?: string): ProjectView[] {
  const teams = new Map((ps.teams ?? []).map((t) => [t.id, t]));
  const closed = new Map((ps.closed ?? []).map((t) => [t.id, t]));
  const solos = new Map((ps.solos ?? []).map((s) => [s.id, s]));
  const name = names(ps);
  const out: ProjectView[] = [];
  for (const project of ps.projects ?? []) {
    if (dir !== undefined && !related(project.path, dir)) continue;
    const units: UnitView[] = [];
    for (const unit of project.units) {
      if (unit.kind === "solo") {
        const s = solos.get(unit.id);
        if (!s) continue;
        const row: Row = {
          id: s.id,
          name: s.name,
          depth: 0,
          role: "solo",
          state: s.state,
          harness: harnessLabel(s.harness, false),
          harnessId: s.harness ?? "",
          model: modelId(s.model),
          modelFull: modelLabel(s.model, undefined),
          ctx: "",
          turns: "",
          unacked: s.unacked ?? 0,
          created: s.created_at,
          since: s.state_since,
          cwd: unit.cwd ?? "",
          dim: false,
          team: null,
          kind: "solo",
          came: s.created_at ? `joined ${clock(s.created_at)}` : "",
          reports: "",
          root: s.cwd ?? project.path,
          logged: false,
        };
        units.push({ kind: "solo", id: s.id, title: null, gate: "", held: 0, rows: [row] });
        continue;
      }
      const closedTeam = unit.kind === "closed" ? closed.get(unit.id) : undefined;
      const team = closedTeam ?? teams.get(unit.id);
      if (!team) continue;
      const members = new Map(team.members.map((m) => [m.id, m]));
      const gone = new Set(team.members.filter((m) => m.state === "gone").map((m) => m.id));
      const rows = (unit.members ?? []).flatMap((place): Row[] => {
        const m = members.get(place.id);
        if (!m) return [];
        const kind = [m.harness ? `${m.harness} ${m.headless ? "headless worker" : "session"}` : m.headless ? "headless worker" : "session", m.gate ? "gate" : ""].filter(Boolean).join(" · ");
        const came = m.headless && m.spawned_by ? `spawned ${clock(m.created_at)} by ${name.get(m.spawned_by) ?? "-"}` : `joined ${clock(m.created_at)}`;
        return [
          {
            id: m.id,
            name: m.name,
            depth: place.depth,
            role: m.role ?? "",
            state: m.state,
            harness: harnessLabel(m.harness, m.headless),
            harnessId: m.harness ?? "",
            model: modelId(m.model),
            modelFull: modelLabel(m.model, m.thinking),
            ctx: place.ctx !== undefined ? tokens(place.ctx) : "",
            turns: place.turns !== undefined ? String(place.turns) : "",
            unacked: m.unacked ?? 0,
            created: m.created_at,
            since: m.state_since,
            lastTurn: m.last_turn_end || undefined,
            cwd: place.cwd,
            dim: closedTeam !== undefined || m.state === "gone" || (m.reports_to !== undefined && gone.has(m.reports_to)),
            team: team.name,
            kind,
            came,
            reports: m.reports_to ? (name.get(m.reports_to) ?? "") : "",
            root: team.root ?? project.path,
            logged: m.headless === true,
          },
        ];
      });
      const gate = team.gate ? (name.get(team.gate) ?? team.gate) : "";
      units.push(
        closedTeam
          ? { kind: "closed", id: team.id, title: team.name, gate, held: 0, closedAt: closedTeam.closed_at, closedBy: closedTeam.closed_by ?? "", rows }
          : { kind: "team", id: team.id, title: team.name, gate, held: team.held ?? 0, rows },
      );
    }
    out.push({ title: project.label.replace(/^…\//, ""), path: project.path, units });
  }
  return out;
}

function related(path: string, dir: string): boolean {
  const a = path.replace(/\/+$/, "");
  const b = dir.replace(/\/+$/, "");
  return a === b || b.startsWith(a + "/") || a.startsWith(b + "/");
}

/** Names piggery knows for participants and teams. */
function names(ps: Ps): Map<string, string> {
  const out = new Map<string, string>(Object.entries(ps.names ?? {}));
  for (const team of ps.teams ?? []) {
    out.set(team.id, team.name);
    for (const m of team.members) out.set(m.id, m.name);
  }
  for (const team of ps.closed ?? []) {
    out.set(team.id, team.name);
    for (const m of team.members) if (!out.has(m.id)) out.set(m.id, m.name);
  }
  for (const s of ps.solos ?? []) out.set(s.id, s.name);
  return out;
}

export type Tone = "muted" | "warning" | "danger";

/**
 * The latest events, newest first, as piggery top lists them: when, who, the event, and its target
 * when that is someone else; denied and held in warning, exited and gone in danger.
 */
export function latestEvents(ps: Ps, count: number): { seq: number; ts: number; who: string; type: string; target: string; tone: Tone }[] {
  const name = names(ps);
  const short = (id: string | null | undefined) => (id ? (name.get(id) ?? (id.length <= 6 ? id : id.slice(-6))) : "");
  return (ps.events ?? [])
    .slice(-count)
    .reverse()
    .map((e) => ({
      seq: e.seq,
      ts: e.ts,
      who: short(e.participant),
      type: e.type,
      target: e.ref_id && e.ref_id !== e.participant && e.ref_id !== e.team_id ? short(e.ref_id) : "",
      tone: e.type === "denied" || e.type === "held" ? "warning" : e.type === "exited" || e.type === "gone" ? "danger" : "muted",
    }));
}

export type TailKind = "tool" | "result" | "error" | "warning" | "rule" | "user" | "text";

/**
 * `piggery tail` text, one entry per line, kind read from its prefix the way piggery top styles it:
 * "> " a tool call, "< " its result ("< name error: …" a failed one), "! " a warning, "-- " a
 * boundary, "user: " the prompt; anything else is the assistant's text.
 */
export function tailLines(text: string): { kind: TailKind; text: string }[] {
  return text
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => {
      if (line.startsWith("> ")) return { kind: "tool", text: "▸ " + line.slice(2) };
      if (line.startsWith("< ")) {
        const rest = line.slice(2);
        const colon = rest.indexOf(": ");
        const head = colon < 0 ? rest : rest.slice(0, colon);
        const body = colon < 0 ? "" : rest.slice(colon + 2);
        if (head.endsWith(" error")) return { kind: "error", text: `✗ ${head.slice(0, -" error".length)}  ${body}` };
        return { kind: "result", text: "✓ " + (colon < 0 ? rest : body) };
      }
      if (line.startsWith("! ")) return { kind: "warning", text: line };
      if (line.startsWith("-- ")) return { kind: "rule", text: line };
      if (line.startsWith("user: ")) return { kind: "user", text: line };
      return { kind: "text", text: line.startsWith("assistant: ") ? line.slice("assistant: ".length) : line };
    });
}
