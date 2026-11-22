// The IndexDO side of router lookups (05 §5.4 `router.resolveOrReserve`).
// The main plan overrides 05: RouterDO owns the routes and makes
// resolveOrReserve atomic. This module is the one entry point that the
// Worker and IndexDO use, so callers of the 05 name reach RouterDO, and it
// rebuilds RouterDO routes from the IndexDO work-item read model after a
// RouterDO reset.
import { doName } from "@openfactory/core";
import type { Env } from "../../contracts/env";

export interface ResolveOrReserveInput {
  factoryId: string;
  sourceKeys: string[];
  actionKey: string;
  target: string;
}

function router(env: Env, teamId: string) {
  return env.ROUTER.getByName(doName.router(teamId));
}

/** router.resolveOrReserve: the active or reserved work item of any key, or a new reservation (atomic in RouterDO). */
export function resolveOrReserve(env: Env, teamId: string, input: ResolveOrReserveInput): Promise<{ wiId: string; token: string; created: boolean }> {
  return router(env, teamId).resolveOrReserve(input);
}

/** The work item of the first key with an active or reserved route, else null. */
export async function resolveAny(env: Env, teamId: string, factoryId: string, keys: string[]): Promise<{ wiId: string; token: string; status: string } | null> {
  for (const k of keys) {
    if (!k) continue;
    const r = await router(env, teamId).resolve(factoryId, k);
    if (r && (r.status === "active" || r.status === "reserved")) return r;
  }
  return null;
}

/** A work-item summary of the IndexDO read model (index.project entity "work_item"). */
export interface WorkItemSummary {
  id: string;
  factory_id: string;
  stage: string;
  source_key?: string;
  references?: { key: string }[];
  completed_at?: string;
  version: number;
}

/**
 * The router.status messages that restore RouterDO routes from summaries
 * (RouterDO ignores lower versions, so a replay is safe).
 */
export function routeRebuildMessages(items: WorkItemSummary[]): { wiId: string; factoryId: string; stage: string; completedMs: number | null; version: number; keys: string[] }[] {
  return items.map((wi) => ({
    wiId: wi.id,
    factoryId: wi.factory_id,
    stage: wi.stage,
    completedMs: wi.completed_at ? Date.parse(wi.completed_at) : null,
    version: wi.version,
    keys: [wi.source_key ?? "", ...(wi.references ?? []).map((r) => r.key)].filter((k) => k !== ""),
  }));
}

/** Sends the rebuild messages to RouterDO (chunked by the caller's alarm, rule 10). */
export async function rebuildRoutes(env: Env, teamId: string, items: WorkItemSummary[]): Promise<number> {
  const r = router(env, teamId);
  let n = 0;
  for (const p of routeRebuildMessages(items)) {
    await r.receive({ msgId: `rebuild:${p.wiId}:${p.version}`, kind: "router.status", lane: "router", origin: doName.index(teamId), payload: p });
    n++;
  }
  return n;
}
