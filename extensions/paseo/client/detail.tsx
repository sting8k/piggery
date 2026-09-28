import type { PluginTheme } from "@getpaseo/plugin";
import { useRpc } from "@getpaseo/plugin/client";
import { Icon, ScrollView } from "@getpaseo/plugin/client/react-native";
import { useRef, useState } from "react";
import { Pressable, Text, View, type ScrollView as NativeScrollView } from "react-native";
import { tail } from "../shared/rpc.ts";
import { stateLook, tailLines, type Row as PigRow, type TailKind } from "../shared/view.ts";
import { ListRow, Rows } from "./kit/list.tsx";
import { StateDot } from "./kit/mark.tsx";
import { TabBar } from "./kit/tab-bar.tsx";
import { ICON_SIZE, RADIUS, SPACING, text } from "./kit/theme.ts";
import { ago, usePoll } from "./poll.ts";

const TAIL_COLOUR: Record<TailKind, keyof PluginTheme["colors"]> = {
  tool: "accent",
  result: "foregroundMuted",
  error: "statusDanger",
  warning: "statusWarning",
  rule: "foregroundMuted",
  user: "foregroundMuted",
  text: "foreground",
};

/** The Overview's key column. */
const KEY_WIDTH = 96;

/**
 * The selected row, as piggery top's sidebar: Overview (every fact top has of it) and Tail. `onClose`
 * is a back chevron when the detail replaces the list (narrow), a close mark beside it (wide).
 */
export function Detail({ theme, row, beside, onClose }: { theme: PluginTheme; row: PigRow; beside: boolean; onClose: () => void }) {
  const [tab, setTab] = useState<"overview" | "tail">("overview");
  const look = stateLook(row.state);
  const c = theme.colors;
  return (
    <View style={{ flex: 1, backgroundColor: c.surface0, gap: SPACING[3] }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: SPACING[2], paddingHorizontal: SPACING[4], paddingTop: SPACING[4] }}>
        {beside ? null : (
          <Pressable onPress={onClose} accessibilityRole="button" accessibilityLabel="Back to the list" hitSlop={SPACING[2]}>
            <Icon name="ChevronLeft" size={ICON_SIZE.md} color={c.foregroundMuted} />
          </Pressable>
        )}
        <Text style={[text(theme, "detailTitle"), { flexShrink: 1 }]} numberOfLines={1}>
          {row.name}
        </Text>
        <Text style={text(theme, "meta")}>·</Text>
        <StateDot theme={theme} status={look.status} word={look.word} />
        <Text style={[text(theme, "meta"), { flexShrink: 1 }]} numberOfLines={1}>
          {`· ${row.role}`}
        </Text>
        <View style={{ flex: 1 }} />
        {beside ? (
          <Pressable onPress={onClose} accessibilityRole="button" accessibilityLabel="Close" hitSlop={SPACING[2]}>
            <Icon name="X" size={ICON_SIZE.md} color={c.foregroundMuted} />
          </Pressable>
        ) : null}
      </View>
      <View style={{ paddingHorizontal: SPACING[4] }}>
      <TabBar
        theme={theme}
        tabs={[
          { id: "overview", label: "Overview" },
          { id: "tail", label: "Tail" },
        ]}
        active={tab}
        onPick={setTab}
      />
      </View>
      {tab === "overview" ? <Overview theme={theme} row={row} /> : <Tail theme={theme} row={row} />}
    </View>
  );
}

/** Every fact top's sidebar has of the row, one card row each; empty ones left out. */
function Overview({ theme, row }: { theme: PluginTheme; row: PigRow }) {
  const facts: [string, string][] = [
    ["team", row.team ?? ""],
    ["kind", row.kind],
    ["model", row.modelFull],
    ["ctx", row.logged ? row.ctx || "-" : ""],
    ["turns", row.logged ? row.turns || "-" : ""],
    ["started", row.came],
    ["since", ago(row.since)],
    ["unacked", String(row.unacked)],
    ["reports", row.reports],
    ["last turn", row.lastTurn ? `${ago(row.lastTurn)} ago` : ""],
    [row.team ? "root" : "cwd", row.root],
    ["id", row.id],
  ];
  return (
    <ScrollView style={{ flex: 1 }}>
      <Rows theme={theme}>
        {facts
          .filter(([, v]) => v !== "")
          .map(([k, v]) => (
            <ListRow key={k} theme={theme}>
              <Text style={[text(theme, "label"), { width: KEY_WIDTH }]}>{k}</Text>
              <Text style={[text(theme, k === "id" ? "meta" : "rowTitle", k === "unacked" && row.unacked > 0 ? "statusWarning" : undefined), { flex: 1 }]} selectable numberOfLines={1}>
                {v}
              </Text>
            </ListRow>
          ))}
      </Rows>
    </ScrollView>
  );
}

function Tail({ theme, row }: { theme: PluginTheme; row: PigRow }) {
  if (!row.logged) {
    return <Text style={[text(theme, "meta"), { paddingHorizontal: SPACING[4] }]}>No tail: its harness keeps no driver log here.</Text>;
  }
  return <TailLog theme={theme} id={row.id} title={row.name} />;
}

/** A worker's last lines as piggery tail prints them, in a code block, newest at the bottom, refreshed while open. */
function TailLog({ theme, id, title }: { theme: PluginTheme; id: string; title: string }) {
  const call = useRpc(tail);
  const { value, error } = usePoll(async () => {
    const r = await call({ id, lines: 20 });
    return r.ok ? { ok: true, value: r.text } : r;
  }, id);
  const scroll = useRef<NativeScrollView>(null);
  const lines = value === null ? [] : tailLines(value);
  return (
    <View style={{ flex: 1, marginHorizontal: SPACING[4], marginBottom: SPACING[4], backgroundColor: theme.colors.surface1, borderRadius: RADIUS.lg, borderWidth: 1, borderColor: theme.colors.border, overflow: "hidden" }}>
        <ScrollView ref={scroll} onContentSizeChange={() => scroll.current?.scrollToEnd({ animated: false })} contentContainerStyle={{ padding: SPACING[4], gap: SPACING[1] }} accessibilityLabel={`Tail of ${title}`}>
          {error ? <Text style={text(theme, "meta", "statusDanger")}>{error}</Text> : null}
          {value === null && !error ? <Text style={text(theme, "meta")}>Loading…</Text> : null}
          {value !== null && lines.length === 0 ? <Text style={text(theme, "meta")}>No output yet.</Text> : null}
          {lines.map((line, i) => (
            <Text key={i} style={text(theme, "code", TAIL_COLOUR[line.kind])} numberOfLines={3} selectable>
              {line.text}
            </Text>
          ))}
        </ScrollView>
    </View>
  );
}
