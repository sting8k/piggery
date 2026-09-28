// Runs the piggery CLI: execFile (no shell), a timeout, and always --no-start so a read never starts
// a piggery daemon.
import { execFile } from "node:child_process";
import { realpath } from "node:fs/promises";

export type Result = { ok: true; out: string } | { ok: false; code: "missing" | "down" | "failed"; error: string };

const TIMEOUT_MS = 5000;

/** The binary to run: the user's setting, else the piggery that installed the plugin, else PATH. */
export function pickBin(setting: string, installed: string): string {
  return setting.trim() || installed.trim() || "piggery";
}

export function runPiggery(bin: string, args: string[], timeoutMs = TIMEOUT_MS): Promise<Result> {
  return new Promise((resolve) => {
    execFile(bin, [...args, "--no-start"], { timeout: timeoutMs, maxBuffer: 16 << 20, env: { ...process.env, NO_COLOR: "1" } }, (error, stdout, stderr) => {
      if (!error) return resolve({ ok: true, out: stdout });
      const err = error as NodeJS.ErrnoException & { killed?: boolean };
      const said = String(stderr).trim();
      if (err.code === "ENOENT") {
        return resolve({ ok: false, code: "missing", error: `piggery was not found at "${bin}". Set its full path in the Piggery settings.` });
      }
      if (said.includes("the piggery daemon is not running")) {
        return resolve({ ok: false, code: "down", error: "The piggery daemon is not running. Any piggery command starts it, e.g. `piggery ps`." });
      }
      if (said.includes("flag provided but not defined: -no-start")) {
        return resolve({ ok: false, code: "failed", error: `The piggery at "${bin}" is older than this plugin (it has no --no-start). Point the Piggery setting at a newer piggery.` });
      }
      if (err.killed) return resolve({ ok: false, code: "failed", error: `piggery did not answer within ${timeoutMs / 1000}s.` });
      resolve({ ok: false, code: "failed", error: said || err.message });
    });
  });
}

/** A directory as piggery records it, symlinks resolved; as given when it cannot be resolved. */
export async function realDir(dir: string): Promise<string> {
  try {
    return await realpath(dir);
  } catch {
    return dir;
  }
}
