import type { PluginTheme } from "@getpaseo/plugin";
import { useRpc, useSettings, useWorkspace, type PluginSurfaceProps, type PluginWorkspacePanelProps } from "@getpaseo/plugin/client";
import { Icon, ScrollView } from "@getpaseo/plugin/client/react-native";
import { useEffect, useRef, useState } from "react";
import { Pressable, Text, View } from "react-native";
import { foldsOf, goneOpen as goneIsOpen, NO_FOLDS, prune, setGone, setTab, setTeam, teamOpen, type Folds } from "../shared/folds.ts";
import { snapshot, viewSettings } from "../shared/rpc.ts";
import { readView, related, sameDir, type Block, type Column, type Dir, type EventRow, type Head, type Row, type View as PigView } from "../shared/view.ts";
import { DetailDialog, type Index } from "./dialog.tsx";
import { Header } from "./header.tsx";
import { Chip, statusColour } from "./kit/badge.tsx";
import { Disclosure } from "./kit/disclosure.tsx";
import { PressRow } from "./kit/press.tsx";
import { Notice } from "./kit/notice.tsx";
import { AgentRow, columnsFor, Tail, type Columns } from "./kit/row.tsx";
import { GroupLabel, Rows } from "./kit/rows.tsx";
import { Select } from "./kit/select.tsx";
import { TabBar } from "./kit/tab-bar.tsx";
import { ASIDE, BAR, BOARD_COLUMNS_MIN, CHEVRON_SLOT, COLUMN_MIN, ICON_SIZE, INDENT, FRAME_MAX, FOLD_MARK, ROW_BAND, ROW_EVENT, ROW_MIN, SPACING, text } from "./kit/theme.ts";
import { pollEvery, useNow, usePoll } from "./poll.ts";

/** ps --view, and with `dir` that directory as piggery writes paths (resolved on the daemon side). */
function useView(dir: string | undefined) {
  const call = useRpc(snapshot);
  return usePoll(
    async () => {
      const r = await call({ dir });
      if (!r.ok) return r;
      const v = readView(r.view);
      return v.ok ? { ok: true as const, value: { view: v.view, dir: r.dir ?? dir } } : v;
    },
    `view:${dir ?? ""}`,
    (v) => pollEvery((v?.view.summary.working ?? 0) + (v?.view.summary.waiting ?? 0) > 0),
  );
}

/** Every participant row of the snapshot (the All and the Closed lists), by id. */
function indexOf(view: PigView): Index {
  const out: Index = new Map();
  for (const list of [view.all, view.closed]) for (const d of list.dirs) for (const b of d.blocks) for (const r of b.rows) out.set(r.id, r);
  return out;
}

/** The directories of a workspace: the ones at, inside or around it, its own first. */
function dirsOf(view: PigView, dir: string | undefined): Dir[] {
  if (dir === undefined) return view.all.dirs;
  const mine = view.all.dirs.filter((d) => related(d.path, dir));
  const own = (d: Dir) => sameDir(d.path, dir);
  return [...mine.filter(own), ...mine.filter((d) => !own(d))];
}

/** A small button that opens a line's Overview, beside the line's own fold. */
function InfoButton({ theme, label, onPress }: { theme: PluginTheme; label: string; onPress: () => void }) {
  return (
    <Pressable onPress={onPress} accessibilityRole="button" accessibilityLabel={label} hitSlop={SPACING[2]}>
      <Icon name="Info" size={ICON_SIZE.md} color={theme.colors.foregroundMuted} />
    </Pressable>
  );
}

/** A live team's line: chevron, name, what is wrong with it (amber), and, folded, its counts. */
function TeamHead({ theme, head, open, onToggle, onInfo, narrow }: { theme: PluginTheme; head: Head; open: boolean; onToggle: () => void; onInfo: () => void; narrow: boolean }) {
  return (
    <Disclosure theme={theme} open={open} onToggle={onToggle} onBody={narrow ? onInfo : undefined} label={head.name} left={0} aside={narrow ? undefined : <InfoButton theme={theme} label={`About ${head.name}`} onPress={onInfo} />}>
      <Icon name="Users" size={ICON_SIZE.md} color={theme.colors.foreground} />
      <Text style={[text(theme, "rowTitle"), { flexShrink: 1 }]} numberOfLines={1}>
        {head.name}
      </Text>
      {head.flags?.length ? (
        <Text style={[text(theme, "meta", "statusWarning"), { flexShrink: 1 }]} numberOfLines={1}>
          {head.flags.join(" · ")}
        </Text>
      ) : null}
      {!open && head.counts?.length ? (
        <Text style={[text(theme, "meta"), { flexShrink: 1 }]} numberOfLines={1}>
          {head.counts.join(" · ")}
        </Text>
      ) : null}
    </Disclosure>
  );
}

/** A team listed as one line (closed, or open with every member gone), and a live team's gone members as one line: dim, with its state and when, in the participants' columns. */
function QuietLine({ theme, row, icon, open, onToggle, onInfo, left, columns }: { theme: PluginTheme; row: Row; icon?: string; open: boolean; onToggle: () => void; onInfo: () => void; left: number; columns: Columns }) {
  const narrow = columns === "none";
  return (
    <Disclosure theme={theme} open={open} onToggle={onToggle} onBody={narrow ? onInfo : undefined} label={row.name} left={left} tail={<Tail theme={theme} row={row} columns={columns} dim />} aside={narrow ? undefined : <InfoButton theme={theme} label={`About ${row.name}`} onPress={onInfo} />}>
      {icon ? <Icon name={icon} size={ICON_SIZE.md} color={theme.colors.foregroundMuted} /> : null}
      <Text style={[text(theme, "rowTitle", "foregroundMuted"), { flexShrink: 1, minWidth: 0 }]} numberOfLines={1}>
        {row.name}
      </Text>
      {row.closed ? <Text style={[text(theme, "label"), { flexShrink: 0 }]}>{row.closed}</Text> : null}
    </Disclosure>
  );
}

/** Where a depth-0 participant's name starts, px from the list's left edge: what an event's text lines up with. */
const NAME_X = CHEVRON_SLOT + BAR + SPACING[2];

/** A participant's indent: under its team's chevron, and one level more for a member below the gate. */
const indentOf = (row: Row) => CHEVRON_SLOT + row.depth * INDENT;

interface Handlers {
  folds: Folds;
  change: (next: Folds) => void;
  open: (id: string) => void;
  columns: Columns;
}

/** One unit of a directory, folded as ps --view says (head.open, row.open, row.folded) unless the user chose. */
function renderBlock(theme: PluginTheme, block: Block, key: string, h: Handlers) {
  const { folds, change, open } = h;
  // The width where a participant's line gives its info slot back: every kind of line then does the same.
  const narrow = h.columns === "none";
  const agent = (row: Row) => <AgentRow key={row.id} theme={theme} row={row} left={indentOf(row)} slot={narrow ? 0 : ASIDE} columns={h.columns} onPress={() => open(row.id)} />;
  const member = agent;
  const gone = (row: Row, teamId: string) => {
    const isOpen = goneIsOpen(folds, teamId);
    return <QuietLine key={row.id} theme={theme} row={row} open={isOpen} left={CHEVRON_SLOT} onToggle={() => change(setGone(folds, teamId, !isOpen))} onInfo={() => open(row.id)} columns={h.columns} />;
  };
  const head = block.head;
  if (head) {
    const isOpen = teamOpen(folds, head.id, head.open);
    const goneOpen = goneIsOpen(folds, head.id);
    return [
      <TeamHead key={`${key}-head`} theme={theme} head={head} open={isOpen} onToggle={() => change(setTeam(folds, head.id, !isOpen, head.open))} onInfo={() => open(head.detail)} narrow={narrow} />,
      ...(isOpen ? block.rows.filter((r) => !r.folded || goneOpen).map((r) => (r.kind === "gone" ? gone(r, head.id) : member(r))) : []),
      isOpen && block.no_members ? (
        <View key={`${key}-none`} style={{ minHeight: ROW_MIN, justifyContent: "center", paddingLeft: CHEVRON_SLOT }}>
          <Text style={text(theme, "meta")}>No members yet. Open an agent session in its directory and ask the gate to admit it.</Text>
        </View>
      ) : null,
    ];
  }
  const [first, ...rest] = block.rows;
  if (first?.kind === "team") {
    const teamId = first.team ?? first.id;
    const isOpen = teamOpen(folds, teamId, first.open === true);
    return [
      <QuietLine key={first.id} theme={theme} row={first} icon={first.closed ? "Archive" : "Users"} open={isOpen} left={0} onToggle={() => change(setTeam(folds, teamId, !isOpen, first.open === true))} onInfo={() => open(first.id)} columns={h.columns} />,
      ...(isOpen ? rest.map(member) : []),
    ];
  }
  return block.rows.map(agent);
}

/** The directories and what is in them, in top's order. */
function Directories({ theme, dirs, h }: { theme: PluginTheme; dirs: Dir[]; h: Handlers }) {
  return (
    <>
      {dirs.map((d) => (
        <View key={d.path}>
          <GroupLabel theme={theme} label={d.label.replace(/^…\//, "")} icon="Folder" />
          <Rows theme={theme}>{d.blocks.flatMap((b, i) => renderBlock(theme, b, `${d.path}:${i}`, h))}</Rows>
        </View>
      ))}
    </>
  );
}

/**
 * The latest events, newest first, as many as ps --view gives (top shows them all too), in the list's
 * own column: each a dense muted line whose text starts at the name column's x and whose time ends at
 * the "since" column's, so it reads as part of the list above. `slot` is what the lines above keep free at the right.
 */
function Events({ theme, events, slot }: { theme: PluginTheme; events: EventRow[]; slot: number }) {
  if (events.length === 0) return null;
  const colourOf = (tone: string) => (tone === "danger" ? "statusDanger" : tone === "warning" ? "statusWarning" : "foregroundMuted");
  return (
    <>
      <GroupLabel theme={theme} label={`Events · ${events.length}`} icon="History" />
      <View style={{ borderTopWidth: 1, borderTopColor: theme.colors.border }}>
        {events.map((e, i) => (
          <View key={i} style={{ flexDirection: "row", alignItems: "center", gap: SPACING[2], minHeight: ROW_EVENT, paddingVertical: SPACING[0.5], paddingLeft: NAME_X, paddingRight: slot }}>
            <Text style={[text(theme, "label"), { flex: 1, minWidth: 0 }]} numberOfLines={1}>
              {e.who || "-"}
              {"  "}
              <Text style={text(theme, "label", colourOf(e.tone))}>{e.type}</Text>
              {e.target ? `  ${e.target}` : ""}
            </Text>
            <Text style={[text(theme, "label"), { fontVariant: ["tabular-nums"] }]}>{e.time}</Text>
          </View>
        ))}
      </View>
    </>
  );
}

/** Every participant of the snapshot with the directory it is listed under; closed teams' only when their tab is chosen. */
interface Item {
  row: Row;
  dir: string;
}

function itemsOf(view: PigView): Item[] {
  const out: Item[] = [];
  for (const d of view.board.dirs) {
    for (const b of d.blocks) {
      for (const row of b.rows) {
        if (row.kind === "member" || row.kind === "solo") out.push({ row, dir: d.path });
      }
    }
  }
  return out;
}

/** The one frame every part of the panel shares: centred and no wider than FRAME_MAX, so a tab switch moves no edge. */
const frame = { width: "100%" as const, maxWidth: FRAME_MAX, alignSelf: "center" as const };

/** A team's filter is chips up to this many (All included); more teams than that are a select. */
const TEAM_CHIPS_MAX = 5;

/** Items in groups by `key`, in the order the keys first appear. */
function groupsOf<T>(items: T[], key: (item: T) => string): [string, T[]][] {
  const out = new Map<string, T[]>();
  for (const item of items) out.set(key(item), [...(out.get(key(item)) ?? []), item]);
  return [...out];
}

/**
 * What names a Board band: its directory, from the cards' left edge, then a thin rule (and, folded,
 * what the fold hides). More room above than below, so it reads as the cards' heading. A dead band is only this
 * line; its fold mark hangs in the panel's gutter, so it takes no room in the line, and a press on the
 * line folds or unfolds it.
 */
function BandDivider({ theme, label, hidden, fold }: { theme: PluginTheme; label: string; hidden?: string; fold?: { open: boolean; onToggle: () => void } }) {
  return (
    <PressRow theme={theme} onPress={fold?.onToggle} expanded={fold?.open} label={fold ? `${fold.open ? "Fold" : "Unfold"} ${label}` : label} style={{ flexDirection: "row", alignItems: "center", gap: SPACING[2], minHeight: ROW_BAND, marginTop: SPACING[4], paddingHorizontal: SPACING[4] }}>
      {fold ? (
        <View style={{ position: "absolute", left: SPACING[0.5], top: 0, bottom: 0, justifyContent: "center" }}>
          <Icon name={fold.open ? "ChevronDown" : "ChevronRight"} size={FOLD_MARK} color={theme.colors.foregroundMuted} />
        </View>
      ) : null}
      <Icon name="Folder" size={ICON_SIZE.sm} color={theme.colors.foregroundMuted} />
      <Text style={[text(theme, "label"), { flexShrink: 1 }]} numberOfLines={1} ellipsizeMode="head">
        {label}
      </Text>
      <View style={{ flex: 1, height: 1, backgroundColor: theme.colors.border }} />
      {hidden ? (
        <Text style={text(theme, "label")} numberOfLines={1}>
          {hidden}
        </Text>
      ) : null}
    </PressRow>
  );
}

/**
 * The Board: one band per directory (ps --view's dirs, in its order), told apart by a labelled divider,
 * and in each band a column per status, the same columns at the same x in every band, so a project
 * reads on its own and the statuses read down the page. A cell is the band's cards of that status, one line each, under a
 * small muted team label when the band holds several teams (solos under none); an empty cell stays
 * empty. A band ps --view says starts folded (gone and closed directories) is one line that opens on press. Filtered by team (the tabs ps --view
 * lists); gone cards only when asked for. Narrow, a band's statuses stack.
 */
function Board({ theme, view, compact, wide, open }: { theme: PluginTheme; view: PigView; compact: boolean; wide: number; open: (id: string) => void }) {
  const [team, setTeam] = useState("");
  const [gone, setGone] = useState(false);
  const [opened, setOpened] = useState<Record<string, boolean>>({});
  const all = itemsOf(view);
  const teamName = (id: string | undefined) => view.tabs.find((t) => t.key === id)?.label ?? "";
  const shown = all.filter((i) => (i.row.tabs ?? []).includes(team));
  const bands = view.board.dirs.map((d) => ({ dir: d, items: shown.filter((i) => i.dir === d.path) })).filter((b) => b.items.length > 0);
  const unfolded = (d: Dir) => opened[d.path] ?? !d.folded;
  const goneShown = gone || bands.some((b) => b.dir.folded && unfolded(b.dir));
  const columns = view.board.columns.filter((c) => (c.status === "gone" ? goneShown : shown.some((i) => i.row.status === c.status)));
  const goneCount = shown.filter((i) => i.row.status === "gone").length;
  const row = wide >= BOARD_COLUMNS_MIN && !compact;
  const cell = (items: Item[], c: Column, labels: boolean) => {
    const cards = items.filter((i) => i.row.status === c.status);
    return groupsOf(cards, (i) => (labels && i.row.team ? teamName(i.row.team) : "")).map(([name, group]) => (
      <View key={name}>
        {name ? (
          <Text style={[text(theme, "label"), { paddingTop: SPACING[3], paddingBottom: SPACING[0.5] }]} numberOfLines={1}>
            {name}
          </Text>
        ) : null}
        <Rows theme={theme} bare>
          {group.map((i) => (
            <AgentRow key={i.row.id} theme={theme} row={i.row} left={0} slot={0} columns="none" noState={i.row.state_text === c.word} onPress={() => open(i.row.id)} />
          ))}
        </Rows>
      </View>
    ));
  };
  return (
    <View>
      <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", gap: SPACING[2], paddingHorizontal: SPACING[4], paddingTop: SPACING[3] }}>
        {view.tabs.length <= TEAM_CHIPS_MAX ? (
          <>
            <Text style={text(theme, "label")}>Team</Text>
            {view.tabs.map((t) => (
              <Chip key={t.key} theme={theme} label={t.label} count={t.count} on={team === t.key} onPress={() => setTeam(t.key)} />
            ))}
          </>
        ) : (
          <Select theme={theme} label="Team" value={team} options={view.tabs.map((t) => ({ value: t.key, label: t.label, count: t.count }))} onPick={setTeam} />
        )}
        <View style={{ flex: 1 }} />
        <Chip theme={theme} label={gone ? "Hide gone" : "Show gone"} count={goneCount} on={gone} onPress={() => setGone(!gone)} />
      </View>
      {row ? (
        <View style={{ flexDirection: "row", gap: SPACING[4], paddingHorizontal: SPACING[4], paddingTop: SPACING[4] }}>
          {columns.map((c) => (
            <View key={c.status} style={{ flex: 1, minWidth: COLUMN_MIN, flexDirection: "row", alignItems: "center", gap: SPACING[2], paddingBottom: SPACING[1.5], borderBottomWidth: 1, borderBottomColor: theme.colors.border }}>
              <Text accessibilityRole="header" style={[text(theme, "section"), { color: statusColour(theme, c.status) }]}>
                {c.word}
              </Text>
              <Text style={[text(theme, "label"), { fontVariant: ["tabular-nums"] }]}>{shown.filter((i) => i.row.status === c.status).length}</Text>
            </View>
          ))}
        </View>
      ) : null}
      {bands.map(({ dir, items }) => {
        const label = dir.label.replace(/^…\//, "");
        const isOpen = unfolded(dir);
        const labels = new Set(items.map((i) => i.row.team).filter(Boolean)).size > 1;
        return (
          <View key={dir.path}>
            <BandDivider theme={theme} label={label} hidden={dir.folded && !isOpen ? `${items.length} gone` : undefined} fold={dir.folded ? { open: isOpen, onToggle: () => setOpened({ ...opened, [dir.path]: !isOpen }) } : undefined} />
            {isOpen ? (
              row ? (
                <View style={{ flexDirection: "row", alignItems: "flex-start", gap: SPACING[4], paddingHorizontal: SPACING[4], paddingBottom: SPACING[1] }}>
                  {columns.map((c) => (
                    <View key={c.status} style={{ flex: 1, minWidth: COLUMN_MIN }}>
                      {cell(items, c, labels)}
                    </View>
                  ))}
                </View>
              ) : (
                <View style={{ paddingHorizontal: SPACING[4], paddingBottom: SPACING[1] }}>
                  {columns
                    .filter((c) => items.some((i) => i.row.status === c.status))
                    .map((c) => (
                      <View key={c.status}>
                        <Text style={[text(theme, "section"), { color: statusColour(theme, c.status), paddingTop: SPACING[2] }]}>{c.word}</Text>
                        {cell(items, c, labels)}
                      </View>
                    ))}
                </View>
              )
            ) : null}
          </View>
        );
      })}
    </View>
  );
}

/**
 * What the user chose is kept in the host's "piggery-view" setting: read once when it arrives, saved a
 * moment after the last change (a burst of clicks is one save), only teams piggery still lists.
 * `change` shows a new state at once and saves it.
 */
function useFolds(set: (f: Folds) => void, view: PigView | null) {
  const stored = useSettings(viewSettings);
  const latest = useRef({ stored, view });
  latest.current = { stored, view };
  const restored = useRef(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => {
    if (stored.status === "ready" && !restored.current) {
      restored.current = true;
      set(foldsOf(stored.values));
    }
  }, [stored.status]);
  useEffect(() => () => clearTimeout(timer.current), []);
  const change = (next: Folds) => {
    set(next);
    clearTimeout(timer.current);
    timer.current = setTimeout(async () => {
      const { stored: s, view: v } = latest.current;
      if (s.status !== "ready") return;
      if (!(await s.save(prune(next, (v?.tabs ?? []).map((t) => t.key)), s.revision))) await s.reload();
    }, 300);
  };
  return { change };
}

/**
 * The Piggery surface: piggery top's rows in Paseo's finish. With a `dir` it is one workspace's: one
 * column, that directory first, no tabs. Otherwise the whole machine, with Overview (the directories
 * and their teams, then the latest events) and Board (one column per status).
 */
function Shell({ theme, compact, dir, empty }: { theme: PluginTheme; compact: boolean; dir?: string; empty: string }) {
  const { value: snap, error, at } = useView(dir);
  const view = snap?.view ?? null;
  const now = useNow(1000);
  const [width, setWidth] = useState(0);
  const [folds, setFolds] = useState<Folds>(NO_FOLDS);
  const { change } = useFolds(setFolds, view);
  const [opened, setOpened] = useState<string | null>(null);
  const index = view ? indexOf(view) : new Map<string, Row>();
  const dirs = view ? dirsOf(view, dir === undefined ? undefined : snap?.dir) : [];
  const h: Handlers = { folds, change, open: setOpened, columns: columnsFor(Math.min(width, FRAME_MAX)) };
  const whole = dir === undefined;
  const board = whole && folds.tab === "board";
  const all = view?.tabs.find((t) => t.key === "")?.count;
  return (
    <View style={{ flex: 1, backgroundColor: theme.colors.surface0 }} onLayout={(e) => setWidth(e.nativeEvent.layout.width)}>
      <ScrollView style={{ flex: 1 }} contentContainerStyle={{ paddingBottom: SPACING[6] }}>
        <View style={frame}>
        <View style={{ gap: SPACING[2], paddingHorizontal: SPACING[4], paddingTop: SPACING[4] }}>
          {view ? <Header theme={theme} summary={view.summary} daemon={view.daemon} at={at} now={now} /> : null}
          {error && view ? <Text style={text(theme, "label", "statusWarning")}>{`Showing the last read: ${error.message}`}</Text> : null}
          {whole && view ? <TabBar theme={theme} tabs={[{ id: "overview", label: "Overview" }, { id: "board", label: "Board", count: all }]} active={folds.tab} onPick={(t) => change(setTab(folds, t))} /> : null}
        </View>
        {error && !view ? <Notice theme={theme} kind={error.code} detail={error.message} /> : null}
        {!view && !error ? <Notice theme={theme} kind="loading" /> : null}
        {view && dirs.length === 0 ? <Notice theme={theme} kind="empty" detail={empty} /> : null}
        {view && !board ? (
          <View style={{ paddingHorizontal: SPACING[4] }}>
            <Directories theme={theme} dirs={dirs} h={h} />
            {whole ? <Events theme={theme} events={view.events} slot={h.columns === "none" ? 0 : ASIDE} /> : null}
          </View>
        ) : null}
        {view && board ? <Board theme={theme} view={view} compact={compact} wide={Math.min(width, FRAME_MAX)} open={setOpened} /> : null}
        </View>
      </ScrollView>
      {view ? <DetailDialog theme={theme} view={view} index={index} root={opened} onClose={() => setOpened(null)} /> : null}
    </View>
  );
}

/** Sidebar surface: every directory, like piggery top. */
export function PiggerySurface({ theme, layout }: PluginSurfaceProps) {
  return <Shell theme={theme} compact={layout.compact} empty="Open an agent session, or ask one to make a team." />;
}

/** Workspace panel: the teams and sessions of this workspace's directory. */
export function FarmPanel({ theme, layout, workspaceId }: PluginWorkspacePanelProps) {
  const dir = useWorkspace(workspaceId, (w) => w.directory);
  if (!dir) return <View style={{ flex: 1, backgroundColor: theme.colors.surface0 }} />;
  return <Shell theme={theme} compact={layout.compact} dir={dir} empty="No team works here yet. Ask your agent: “make a supervisor-executor team for …”" />;
}
