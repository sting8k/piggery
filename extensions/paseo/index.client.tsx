import type { PluginClientContext } from "@getpaseo/plugin/client";
import { FarmPanel, PiggerySurface } from "./client/farm.tsx";
import { PiggerySettings } from "./client/settings.tsx";

export default function contribute(client: PluginClientContext) {
  const cleanups = [
    client.addSurface("piggery", PiggerySurface),
    client.addSidebarItem({ id: "piggery", title: "Piggery", icon: "PiggyBank", surface: "piggery" }),
    client.addWorkspacePanel({ id: "farm", title: "Farm", icon: "PiggyBank", context: "workspace", locations: ["explorer", "workspace"], Component: FarmPanel }),
    client.addSettingsScreen({ id: "piggery", title: "Piggery", icon: "PiggyBank", Component: PiggerySettings }),
  ];
  return () => cleanups.forEach((cleanup) => cleanup());
}
