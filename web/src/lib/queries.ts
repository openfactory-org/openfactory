import { useQuery } from "@tanstack/react-query";
import { api, unwrap, type Factory, type Schemas } from "../api/client";
import { useMutate } from "./mutate";

/** Queries and mutations that the shell and several pages share. */

export const FACTORIES_KEY = ["factories"] as const;

export function useFactories() {
  return useQuery({
    queryKey: FACTORIES_KEY,
    queryFn: () => unwrap(api.GET("/factories", { params: { query: { limit: 200 } } })).then((r) => (r.items ?? []) as Factory[]),
  });
}

/**
 * The key of the sidebar inbox badge. It starts with "inbox", so the live
 * stream's inbox events (lib/live.ts) and the inbox mutations refresh it at once;
 * the interval is only a fallback.
 */
export const INBOX_COUNT_KEY = ["inbox", "count"] as const;

export function useInboxCount() {
  return useQuery({
    queryKey: INBOX_COUNT_KEY,
    queryFn: () => unwrap(api.GET("/inbox", { params: { query: { limit: 200 } } })).then((r) => r.items?.length ?? 0),
    refetchInterval: 60_000,
  });
}

/** Dispatches a prompt to a factory's foreman (POST /factories/{uid}/runs). */
export function useDispatch(onDone: (r: Schemas["Run"], fid: string) => void) {
  return useMutate(
    ({ fid, prompt, title }: { fid: string; prompt: string; title?: string }) =>
      // The work item records that it came from the dashboard, like its run.
      unwrap(api.POST("/factories/{uid}/runs", { params: { path: { uid: fid } }, body: { prompt, title, source: { provider: "dashboard", kind: "task" } } })),
    { invalidate: [["workitems"], ["runs"]], success: "Sent to the foreman.", onSuccess: (r, v) => onDone(r, v.fid) },
  );
}

/** The path of a factory page. */
export function fpath(f: Factory | string, sub = ""): string {
  const id = typeof f === "string" ? f : f.id;
  return `/f/${encodeURIComponent(id)}${sub ? `/${sub}` : ""}`;
}

/** True when the event is the "send" chord: ⌘↵ on macOS, Ctrl+↵ elsewhere. */
export function isSubmitChord(e: { key: string; metaKey: boolean; ctrlKey: boolean }): boolean {
  return e.key === "Enter" && (e.metaKey || e.ctrlKey);
}

const IS_MAC = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform ?? "");

/** The modifier key of the platform, as shown in keyboard hints. */
export const MOD = IS_MAC ? "⌘" : "Ctrl";
