import { useEffect, useState } from "react";

/** ps --json reads the workers' logs for ctx and turns, so ask no more often than this. */
const REFRESH_MS = 5000;

/** The web app's page; a native app has none. */
type Page = { visibilityState: string; addEventListener(type: string, fn: () => void): void; removeEventListener(type: string, fn: () => void): void };
const page = (globalThis as { document?: Page }).document;

/** The app page is in the background. */
function hidden(): boolean {
  return page?.visibilityState === "hidden";
}

export type Loaded<T> = { value: T | null; error: string | null };

/**
 * Calls `load` now and every REFRESH_MS while mounted and the page is shown (a hidden page skips,
 * and asks at once when shown again); keeps the last good value across a failure.
 */
export function usePoll<T>(load: () => Promise<{ ok: true; value: T } | { ok: false; error: string }>, key: string): Loaded<T> {
  const [state, setState] = useState<Loaded<T>>({ value: null, error: null });
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async () => {
      if (hidden()) {
        timer = setTimeout(tick, REFRESH_MS);
        return;
      }
      try {
        const r = await load();
        if (live) setState((s) => (r.ok ? { value: r.value, error: null } : { value: s.value, error: r.error }));
      } catch (error) {
        if (live) setState((s) => ({ value: s.value, error: String(error) }));
      }
      if (live) timer = setTimeout(tick, REFRESH_MS);
    };
    const shown = () => {
      if (hidden() || !live) return;
      clearTimeout(timer);
      void tick();
    };
    setState({ value: null, error: null });
    void tick();
    page?.addEventListener("visibilitychange", shown);
    return () => {
      live = false;
      clearTimeout(timer);
      page?.removeEventListener("visibilitychange", shown);
    };
    // `load` is a fresh closure each render; `key` says when it asks for something else.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  return state;
}

/** Time since `ms` in one unit: 12s, 4m, 3h, 2d; "" when unknown. */
export function ago(ms: number | undefined): string {
  if (!ms) return "";
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}
