// The plugin's two RPCs and its one setting, shared by the daemon side and the app. Both RPCs only
// read: they run the piggery CLI with --no-start, so looking never starts a piggery daemon.
import { defineRpc, defineSettings } from "@getpaseo/plugin";
import { z } from "zod";

/** Why piggery gave nothing: not installed at that path, daemon not running, or another failure. */
const Failure = z.object({ ok: z.literal(false), code: z.enum(["missing", "down", "failed"]), error: z.string() });

/**
 * `piggery ps --json`, passed through as piggery wrote it (shared/view.ts reads it). With a workspace
 * `dir`, also that directory as piggery records paths (symlinks resolved, e.g. macOS's /tmp is a
 * symlink), which only the daemon side can work out.
 */
export const snapshot = defineRpc({
  name: "piggery.snapshot",
  input: z.object({ dir: z.string().optional() }),
  output: z.union([z.object({ ok: z.literal(true), ps: z.record(z.string(), z.unknown()), dir: z.string().optional() }), Failure]),
});

/** The last lines of one participant's session as `piggery tail` prints them. */
export const tail = defineRpc({
  name: "piggery.tail",
  input: z.object({ id: z.string().min(1), lines: z.number().int().min(1).max(200).default(20) }),
  output: z.union([z.object({ ok: z.literal(true), text: z.string() }), Failure]),
});

export const RPC_NAMES = [snapshot.name, tail.name];

/** A piggery binary the user chose; empty uses the one that installed the plugin, else PATH. */
export const settings = defineSettings({
  id: "piggery",
  scope: "host",
  version: 1,
  schema: z.object({ path: z.string().default("") }),
});
