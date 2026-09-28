import type { PluginTheme } from "@getpaseo/plugin";
import { Text, View } from "react-native";
import type { Status } from "../../shared/view.ts";
import { DOT, SPACING, text } from "./theme.ts";

/**
 * A state as Paseo's Providers screen shows a provider's: a dot, then its word
 * (providers-section.tsx:275-320, 517-525). Colours as piggery top's: working success, waiting
 * warning, gone danger, idle muted. Filled while it runs or has stopped for good, hollow while it
 * waits (idle, starting, permission), so the two read apart without the words.
 */
export function StateDot({ theme, status, word }: { theme: PluginTheme; status: Status; word?: string }) {
  const c = theme.colors;
  const colour = { working: c.statusSuccess, idle: c.foregroundMuted, waiting: c.statusWarning, gone: c.statusDanger }[status];
  const hollow = status === "idle" || status === "waiting";
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: SPACING[1.5] }}>
      <View style={{ width: DOT, height: DOT, borderRadius: DOT / 2, borderWidth: hollow ? 1.5 : 0, borderColor: colour, backgroundColor: hollow ? "transparent" : colour }} />
      {word ? <Text style={text(theme, "meta")}>{word}</Text> : null}
    </View>
  );
}
