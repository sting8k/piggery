import type { PluginTheme } from "@getpaseo/plugin";
import { Icon } from "@getpaseo/plugin/client/react-native";
import { View } from "react-native";
import { ICON_SIZE } from "./theme.ts";

/**
 * pi's mark, as Paseo draws it for its Pi provider (components/icons/pi-icon.tsx:10-16): a "P" and a
 * leg on a 4 x 4 grid inside a 600-unit box with a 65-unit margin. Plugins get no SVG, and the mark
 * is only squares, so it is drawn with views.
 */
const PI_CELLS: [col: number, row: number][] = [
  [0, 0], [1, 0], [2, 0],
  [0, 1], [2, 1],
  [0, 2], [1, 2], [3, 2],
  [0, 3], [3, 3],
];
const PI_MARGIN = 65.29 / 600;
const PI_CELL = 117.36 / 600;

function PiMark({ size, color }: { size: number; color: string }) {
  const cell = size * PI_CELL;
  const inset = size * PI_MARGIN;
  return (
    <View style={{ width: size, height: size }}>
      {PI_CELLS.map(([col, row]) => (
        <View key={`${col}-${row}`} style={{ position: "absolute", left: inset + col * cell, top: inset + row * cell, width: cell + 0.5, height: cell + 0.5, backgroundColor: color }} />
      ))}
    </View>
  );
}

/**
 * A row's harness, as Paseo's Providers list leads a provider's row with its logo
 * (providers-section.tsx:231): pi's own mark; a terminal for a CLI participant; a neutral bot for
 * any other harness, whose logos need curves a plugin cannot draw.
 */
export function HarnessMark({ theme, harness, muted }: { theme: PluginTheme; harness: string; muted?: boolean }) {
  const color = muted ? theme.colors.foregroundMuted : theme.colors.foreground;
  if (harness === "pi") return <PiMark size={ICON_SIZE.md} color={color} />;
  return <Icon name={harness === "cli" ? "Terminal" : "Bot"} size={ICON_SIZE.md} color={color} />;
}
