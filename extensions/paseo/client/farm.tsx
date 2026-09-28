import type { PluginTheme } from "@getpaseo/plugin";
import { useRpc, useWorkspace, type PluginSurfaceProps, type PluginWorkspacePanelProps } from "@getpaseo/plugin/client";
import { Icon, ScrollView } from "@getpaseo/plugin/client/react-native";
import { useState } from "react";
import { Text, View } from "react-native";
import { snapshot } from "../shared/rpc.ts";
import { farm, latestEvents, PROTOCOL_VERSION, stateLook, type Ps, type Row as PigRow, type UnitView } from "../shared/view.ts";
import { Detail } from "./detail.tsx";
import { Disclosure } from "./kit/disclosure.tsx";
import { HarnessMark } from "./kit/harness-mark.tsx";
import { Cell, GroupLabel, ListRow, Rows } from "./kit/list.tsx";
import { StateDot } from "./kit/mark.tsx";
import { COLUMNS, HIDE_BELOW, ICON_SIZE, INDENT, NAME_MIN, SPACING, SPLIT_MIN, text, type ColumnKey } from "./kit/theme.ts";
import { ago, usePoll } from "./poll.ts";

const EVENTS = 8;

/** ps --json, and with `dir` that directory as piggery writes paths (resolved on the daemon side). */
function useSnapshot(dir: string | undefined) {
  const call = useRpc(snapshot);
  return usePoll(async () => {
    const r = await call({ dir });
    return r.ok ? { ok: true, value: { ps: r.ps as Ps, dir: r.dir ?? dir } } : r;
  }, `snapshot:${dir ?? ""}`);
}

/** The columns that fit a list this wide, in top's order. */
function shown(width: number) {
  return COLUMNS.filter((c) => width >= HIDE_BELOW[c.key]);
}

const numeric = (key: ColumnKey) => key === "ctx" || key === "turns" || key === "unacked" || key === "age" || key === "since";

function value(row: PigRow, key: Exclude<ColumnKey, "state">): string {
  switch (key) {
    case "role":
      return row.role;
    case "harness":
      return row.harness;
    case "model":
      return row.model;
    case "ctx":
      return row.ctx;
    case "turns":
      return row.turns;
    case "unacked":
      return String(row.unacked);
    case "age":
      return ago(row.created);
    case "since":
      return ago(row.since);
    case "cwd":
      return row.cwd && `./${row.cwd}`;
  }
}

/** The trailing slot: a chevron that says the row opens its detail (accent while selected). */
function Opens({ theme, selected }: { theme: PluginTheme; selected?: boolean }) {
  return (
    <View style={{ width: SPACING[6], alignItems: "flex-end" }}>
      {selected !== undefined ? <Icon name="ChevronRight" size={ICON_SIZE.sm} color={selected ? theme.colors.accent : theme.colors.foregroundMuted} /> : null}
    </View>
  );
}

/** The column labels, once above every group (top's header), as the host's small muted labels. */
function Header({ theme, width }: { theme: PluginTheme; width: number }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", paddingHorizontal: SPACING[4], paddingTop: SPACING[3], paddingBottom: SPACING[1.5] }}>
      <Text style={[text(theme, "label"), { flex: 1, minWidth: NAME_MIN }]}>Name</Text>
      {shown(width).map((c) => (
        <Cell key={c.key} theme={theme} width={c.width} right={numeric(c.key)} head>
          {c.label}
        </Cell>
      ))}
      <Opens theme={theme} />
    </View>
  );
}

function MemberRow({ theme, row, width, indent, selected, onSelect }: { theme: PluginTheme; row: PigRow; width: number; indent: number; selected: boolean; onSelect: () => void }) {
  const look = stateLook(row.state);
  return (
    <ListRow theme={theme} onPress={onSelect} selected={selected}>
      <View style={{ flex: 1, minWidth: NAME_MIN, flexDirection: "row", alignItems: "center", gap: SPACING[2], paddingLeft: indent }}>
        <HarnessMark theme={theme} harness={row.harnessId} muted={row.dim} />
        <Text style={[text(theme, "rowTitle", row.dim ? "foregroundMuted" : "foreground"), { flexShrink: 1 }]} numberOfLines={1}>
          {row.name}
        </Text>
      </View>
      {shown(width).map((c) =>
        c.key === "state" ? (
          <View key={c.key} style={{ width: c.width, paddingLeft: SPACING[2] }}>
            <StateDot theme={theme} status={look.status} word={look.word} />
          </View>
        ) : (
          <Cell key={c.key} theme={theme} width={c.width} right={numeric(c.key)} colour={c.key === "unacked" && row.unacked > 0 ? "statusWarning" : undefined}>
            {value(row, c.key) || "-"}
          </Cell>
        ),
      )}
      <Opens theme={theme} selected={selected} />
    </ListRow>
  );
}

/** A team: chevron · name, then its gate (or none) and held letters as top's title; a closed one, dim. */
function TeamRow({ theme, unit, open, onToggle }: { theme: PluginTheme; unit: UnitView; open: boolean; onToggle: () => void }) {
  const closed = unit.kind === "closed";
  return (
    <Disclosure theme={theme} open={open} onToggle={onToggle}>
      <Icon name={closed ? "Archive" : "Users"} size={ICON_SIZE.md} color={closed ? theme.colors.foregroundMuted : theme.colors.foreground} />
      <Text style={text(theme, "rowTitle", closed ? "foregroundMuted" : "foreground")} numberOfLines={1}>
        {unit.title}
      </Text>
      <Text style={[text(theme, "meta"), { flexShrink: 1 }]} numberOfLines={1}>
        {closed ? (
          `closed ${ago(unit.closedAt)} ago by ${unit.closedBy || "admin"} · ${unit.rows.length} member${unit.rows.length === 1 ? "" : "s"}`
        ) : (
          <>
            {unit.gate ? `gate ${unit.gate}` : <Text style={text(theme, "meta", "statusWarning")}>no gate</Text>}
            {unit.held > 0 ? <Text style={text(theme, "meta", "statusWarning")}>{` · ${unit.held} held`}</Text> : null}
          </>
        )}
      </Text>
    </Disclosure>
  );
}

/** An event's small mark by its kind, muted; one per row. */
const EVENT_ICON: Record<string, string> = { spawned: "Plus", team_up: "ArrowUp", team_down: "ArrowDown", gc: "Trash2" };

/** The latest events as top lists them: when, who, what, its target. */
function Events({ theme, ps }: { theme: PluginTheme; ps: Ps }) {
  const events = latestEvents(ps, EVENTS);
  if (events.length === 0) return null;
  return (
    <>
      <GroupLabel theme={theme} label="Events" icon="Activity" />
      <Rows theme={theme}>
        {events.map((e) => {
          const colour = e.tone === "danger" ? "statusDanger" : e.tone === "warning" ? "statusWarning" : "foreground";
          return (
            <ListRow key={e.seq} theme={theme}>
              <View style={{ width: NAME_MIN, flexDirection: "row", alignItems: "center", gap: SPACING[2] }}>
                <Icon name={EVENT_ICON[e.type] ?? "Dot"} size={ICON_SIZE.md} color={theme.colors.foregroundMuted} />
                <Text style={[text(theme, "rowTitle"), { flexShrink: 1 }]} numberOfLines={1}>
                  {e.who || "-"}
                </Text>
              </View>
              <Cell theme={theme} width={COLUMNS[0].width + COLUMNS[1].width} colour={colour}>
                {e.type}
              </Cell>
              <Text style={[text(theme, "meta"), { flex: 1, paddingLeft: SPACING[2] }]} numberOfLines={1}>
                {e.target}
              </Text>
              <Cell theme={theme} width={COLUMNS[8].width} right>
                {ago(e.ts)}
              </Cell>
              <Opens theme={theme} />
            </ListRow>
          );
        })}
      </Rows>
    </>
  );
}

function Message({ theme, message, colour }: { theme: PluginTheme; message: string; colour?: "statusDanger" | "statusWarning" }) {
  return <Text style={[text(theme, "meta", colour), { padding: SPACING[4] }]}>{message}</Text>;
}

/**
 * piggery top's table in Paseo's finish: per project directory a small label, then its teams (rows
 * that fold their member tree; closed teams folded) and solos; then the latest events. `dir` limits
 * it to one workspace. A selected row opens beside the list when there is room, otherwise in its place.
 */
function Farm({ theme, compact, dir, empty }: { theme: PluginTheme; compact: boolean; dir?: string; empty: string }) {
  const { value: snap, error } = useSnapshot(dir);
  const ps = snap?.ps ?? null;
  const [width, setWidth] = useState(0);
  const [selected, setSelected] = useState<string | null>(null);
  const [folds, setFolds] = useState<Record<string, boolean>>({});
  const projects = ps ? farm(ps, dir === undefined ? undefined : snap?.dir) : [];
  const rows = new Map(projects.flatMap((p) => p.units.flatMap((u) => u.rows.map((r) => [r.id, r] as const))));
  const pick = rows.get(selected ?? "") ?? null;
  const beside = !compact && width >= SPLIT_MIN;
  const listWidth = pick && beside ? width / 2 : width;
  const select = (id: string) => setSelected(id === selected ? null : id);

  const list = (
    <ScrollView style={{ flex: 1 }} contentContainerStyle={{ paddingBottom: SPACING[6] }}>
      {error ? <Message theme={theme} message={error} colour="statusDanger" /> : null}
      {ps && ps.protocol_version !== PROTOCOL_VERSION ? (
        <Message theme={theme} colour="statusWarning" message={`piggery speaks protocol ${ps.protocol_version ?? "?"}; this plugin was written for ${PROTOCOL_VERSION}. Some fields may be missing.`} />
      ) : null}
      {!ps && !error ? <Message theme={theme} message="Loading…" /> : null}
      {ps && projects.length === 0 ? <Message theme={theme} message={empty} /> : null}
      {projects.length > 0 ? <Header theme={theme} width={listWidth} /> : null}
      {projects.map((project) => (
        <View key={project.path}>
          <GroupLabel theme={theme} label={project.title} icon="Folder" />
          <Rows theme={theme}>
            {project.units.map((unit) => {
              if (unit.kind === "solo") {
                const row = unit.rows[0];
                return <MemberRow key={unit.id} theme={theme} row={row} width={listWidth} indent={0} selected={row.id === selected} onSelect={() => select(row.id)} />;
              }
              const open = folds[unit.id] ?? unit.kind === "team";
              return [
                <TeamRow key={unit.id} theme={theme} unit={unit} open={open} onToggle={() => setFolds({ ...folds, [unit.id]: !open })} />,
                open && unit.rows.length === 0 ? (
                  <ListRow key={`${unit.id}-none`} theme={theme}>
                    <Text style={[text(theme, "meta"), { paddingLeft: INDENT }]}>No members. Open an agent session in its directory and ask the gate to admit it.</Text>
                  </ListRow>
                ) : null,
                ...(open ? unit.rows.map((row) => <MemberRow key={row.id} theme={theme} row={row} width={listWidth} indent={(row.depth + 1) * INDENT} selected={row.id === selected} onSelect={() => select(row.id)} />) : []),
              ];
            })}
          </Rows>
        </View>
      ))}
      {ps && dir === undefined ? <Events theme={theme} ps={ps} /> : null}
    </ScrollView>
  );

  return (
    <View style={{ flex: 1, flexDirection: "row", backgroundColor: theme.colors.surface0 }} onLayout={(e) => setWidth(e.nativeEvent.layout.width)}>
      {pick && !beside ? null : list}
      {pick ? (
        <View style={{ flex: 1, borderLeftWidth: beside ? 1 : 0, borderLeftColor: theme.colors.border }}>
          <Detail key={pick.id} theme={theme} row={pick} beside={beside} onClose={() => setSelected(null)} />
        </View>
      ) : null}
    </View>
  );
}

/** Sidebar surface: every project, like piggery top. */
export function PiggerySurface({ theme, layout }: PluginSurfaceProps) {
  return <Farm theme={theme} compact={layout.compact} empty="No piggery team or session is open." />;
}

/** Workspace panel: the teams and sessions of this workspace's directory. */
export function FarmPanel({ theme, layout, workspaceId }: PluginWorkspacePanelProps) {
  const dir = useWorkspace(workspaceId, (w) => w.directory);
  if (!dir) return <View style={{ flex: 1, backgroundColor: theme.colors.surface0 }} />;
  return <Farm theme={theme} compact={layout.compact} dir={dir} empty="No team works here yet. Ask your agent: “make a supervisor-executor team for …”" />;
}
