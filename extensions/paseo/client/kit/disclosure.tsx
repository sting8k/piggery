import type { PluginTheme } from "@getpaseo/plugin";
import { Icon } from "@getpaseo/plugin/client/react-native";
import type { ReactNode } from "react";
import { View } from "react-native";
import { ListRow } from "./list.tsx";
import { ICON_SIZE, SPACING } from "./theme.ts";

/**
 * A list row that folds what is under it: the host's chevron (providers-section.tsx:227-230), down
 * when open. The rows it folds are the caller's, drawn as siblings so the list's rules run on.
 */
export function Disclosure({ theme, open, onToggle, children }: { theme: PluginTheme; open: boolean; onToggle: () => void; children: ReactNode }) {
  return (
    <ListRow theme={theme} onPress={onToggle} expanded={open}>
      <View style={{ flex: 1, minWidth: 0, flexDirection: "row", alignItems: "center", gap: SPACING[2] }}>
        <Icon name={open ? "ChevronDown" : "ChevronRight"} size={ICON_SIZE.sm} color={theme.colors.foregroundMuted} />
        {children}
      </View>
    </ListRow>
  );
}
