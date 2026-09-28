// A dense list in Paseo's finish: small muted labels (styles/settings.ts:14-18), rows split by a rule
// in the border colour (settings.ts:42-45), tinted on hover, press and selection like the host's
// provider rows (providers-section.tsx:496-501). Full width: no card, no centred column.
import type { PluginTheme } from "@getpaseo/plugin";
import { Icon } from "@getpaseo/plugin/client/react-native";
import { Children, isValidElement, type ReactNode } from "react";
import { Pressable, Text, View, type PressableStateCallbackType } from "react-native";
import { ICON_SIZE, ROW_MIN, SPACING, text } from "./theme.ts";

/** A small muted label over the rows it names (a project directory, Events), led by its icon. */
export function GroupLabel({ theme, label, icon }: { theme: PluginTheme; label: string; icon?: string }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: SPACING[1.5], paddingHorizontal: SPACING[4], paddingTop: SPACING[4], paddingBottom: SPACING[1.5] }}>
      {icon ? <Icon name={icon} size={ICON_SIZE.sm} color={theme.colors.foregroundMuted} /> : null}
      <Text style={[text(theme, "label"), { flexShrink: 1 }]} numberOfLines={1} ellipsizeMode="head">
        {label}
      </Text>
    </View>
  );
}

/** Children stacked with a rule above each (the first too: it closes the label's group); nulls skipped. */
export function Rows({ theme, children }: { theme: PluginTheme; children: ReactNode }) {
  const shown = Children.toArray(children).filter(isValidElement);
  return (
    <View style={{ borderBottomWidth: shown.length > 0 ? 1 : 0, borderBottomColor: theme.colors.border }}>
      {shown.map((child, i) => (
        <View key={child.key ?? i} style={{ borderTopWidth: 1, borderTopColor: theme.colors.border }}>
          {child}
        </View>
      ))}
    </View>
  );
}

/** One list row: its cells side by side; tinted on hover and press when pressable, and while selected. */
export function ListRow({ theme, onPress, selected, expanded, children }: { theme: PluginTheme; onPress?: () => void; selected?: boolean; expanded?: boolean; children: ReactNode }) {
  const style = (state: PressableStateCallbackType) => {
    const hovered = (state as PressableStateCallbackType & { hovered?: boolean }).hovered;
    const tint = selected || (onPress !== undefined && (hovered || state.pressed));
    return {
      flexDirection: "row" as const,
      alignItems: "center" as const,
      minHeight: ROW_MIN,
      paddingHorizontal: SPACING[4],
      backgroundColor: tint ? theme.colors.surface2 : "transparent",
    };
  };
  return (
    <Pressable onPress={onPress} disabled={!onPress} accessibilityRole={onPress ? "button" : undefined} accessibilityState={expanded === undefined ? undefined : { expanded }} style={style}>
      {children}
    </Pressable>
  );
}

/** A cell of fixed width in the meta role; `right` for numbers. */
export function Cell({ theme, width, right, colour, head, children }: { theme: PluginTheme; width: number; right?: boolean; colour?: keyof PluginTheme["colors"]; head?: boolean; children: ReactNode }) {
  return (
    <Text style={[text(theme, head ? "label" : "meta", colour), { width, textAlign: right ? "right" : "left", paddingLeft: SPACING[2] }]} numberOfLines={1}>
      {children}
    </Text>
  );
}
