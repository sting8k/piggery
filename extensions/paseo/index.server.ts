import type { PluginServerContext } from "@getpaseo/plugin/server";
import { piggeryPath } from "./server/installed.ts";
import { pickBin, realDir, runPiggery } from "./server/piggery.ts";
import { settings, snapshot, tail } from "./shared/rpc.ts";

export default function contribute(server: PluginServerContext) {
  const stored = server.registerSettings(settings);
  const bin = async () => {
    const state = await stored.read();
    return pickBin(state.status === "ready" ? state.values.path : "", piggeryPath);
  };

  server.handle(snapshot, async ({ dir }) => {
    const r = await runPiggery(await bin(), ["ps", "--json"]);
    if (!r.ok) return r;
    try {
      return { ok: true as const, ps: JSON.parse(r.out) as Record<string, unknown>, dir: dir === undefined ? undefined : await realDir(dir) };
    } catch {
      return { ok: false as const, code: "failed" as const, error: "piggery ps --json did not print JSON." };
    }
  });

  server.handle(tail, async ({ id, lines }) => {
    const r = await runPiggery(await bin(), ["tail", id, "-n", String(lines)]);
    return r.ok ? { ok: true as const, text: r.out } : r;
  });

  return () => {};
}
