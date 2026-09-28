import type { PluginTheme } from "@getpaseo/plugin";
import { Pressable, Text, View, type PressableStateCallbackType } from "react-native";
import { RADIUS, SEGMENT, SPACING, text } from "./theme.ts";

/**
 * Paseo's segmented control at its small size (control-geometry.ts:225-253, segmented-control.tsx
 * :167-230): segments side by side, the chosen one filled. The SDK theme has no surface3, so the
 * chosen and pressed segment take surface2 and a hovered one surface1.
 */
export function TabBar<T extends string>({ theme, tabs, active, onPick }: { theme: PluginTheme; tabs: { id: T; label: string }[]; active: T; onPick: (id: T) => void }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: SPACING[1], minHeight: SEGMENT.height }}>
      {tabs.map((tab) => {
        const on = tab.id === active;
        const style = (state: PressableStateCallbackType) => {
          const hovered = (state as PressableStateCallbackType & { hovered?: boolean }).hovered;
          return {
            minHeight: SEGMENT.height - SEGMENT.inset * 2,
            justifyContent: "center" as const,
            paddingHorizontal: SEGMENT.paddingX,
            borderRadius: RADIUS.md,
            backgroundColor: on || state.pressed ? theme.colors.surface2 : hovered ? theme.colors.surface1 : "transparent",
          };
        };
        return (
          <Pressable key={tab.id} accessibilityRole="tab" accessibilityState={{ selected: on }} onPress={() => onPick(tab.id)} style={style}>
            <Text style={text(theme, "meta", on ? "foreground" : "foregroundMuted")}>{tab.label}</Text>
          </Pressable>
        );
      })}
    </View>
  );
}
