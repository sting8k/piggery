import { useSettings, type PluginSurfaceProps } from "@getpaseo/plugin/client";
import { useState } from "react";
import { Pressable, Text, TextInput, View } from "react-native";
import { settings } from "../shared/rpc.ts";
import { RADIUS, SPACING, text } from "./kit/theme.ts";

/** Settings: which piggery binary to run, for a Paseo daemon that should not use the default. */
export function PiggerySettings({ theme }: PluginSurfaceProps) {
  const state = useSettings(settings);
  const [draft, setDraft] = useState<string | null>(null);
  if (state.status !== "ready") {
    return <Text style={text(theme, "meta", state.status === "loading" ? "foregroundMuted" : "statusDanger")}>{state.status === "loading" ? "Loading…" : state.error}</Text>;
  }
  const value = draft ?? state.values.path;
  const changed = value.trim() !== state.values.path;
  const c = theme.colors;
  return (
    <View style={{ padding: SPACING[3], gap: SPACING[2] }}>
      <Text style={text(theme, "rowTitle")}>piggery binary</Text>
      <Text style={text(theme, "meta")}>A full path. Leave it empty to use the piggery that installed this plugin, or else the one in the Paseo daemon's PATH.</Text>
      <TextInput
        value={value}
        onChangeText={setDraft}
        accessibilityLabel="piggery binary"
        placeholder="piggery"
        autoCapitalize="none"
        autoCorrect={false}
        style={[text(theme, "rowTitle"), { borderWidth: 1, borderColor: c.border, borderRadius: RADIUS.md, paddingHorizontal: SPACING[2], paddingVertical: SPACING[1.5] }]}
      />
      {state.saveError ? <Text style={text(theme, "meta", "statusDanger")}>{state.saveError}</Text> : null}
      <Pressable
        disabled={!changed || state.saving}
        onPress={async () => {
          if (await state.save({ path: value.trim() }, state.revision)) setDraft(null);
        }}
        style={{ alignSelf: "flex-start", paddingHorizontal: SPACING[3], paddingVertical: SPACING[1.5], borderRadius: RADIUS.md, borderWidth: 1, borderColor: c.border, backgroundColor: changed ? c.surface2 : "transparent" }}
      >
        <Text style={text(theme, "meta", changed ? "foreground" : "foregroundMuted")}>{state.saving ? "Saving…" : "Save"}</Text>
      </Pressable>
    </View>
  );
}
