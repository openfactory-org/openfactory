// REST handlers: metrics.
import { doName } from "@openfactory/core";
import { callMeta, json, teamOf, type Handler } from "../http/handler";
import type { MSession } from "../metrics/build";
import { compute, partitionSessionLoader } from "../metrics/collect";
import { unwrapOp } from "../lib/rpc";
import { factoryAccess, indexList, parseSince } from "./measure";

/**
 * GET /api/v1/factories/{uid}/metrics: Dashboard metrics. Computed from the
 * IndexDO read model, with the run sessions (compute time) from WorkDO.
 */
export const getMetrics: Handler = async (_req, env, ctx) => {
  const f = await factoryAccess(env, ctx);
  const now = Date.now();
  const since = parseSince(ctx.url.searchParams.get("since"), now) ?? now - 30 * 86_400_000;
  const meta = callMeta(ctx);
  const sessions = partitionSessionLoader(async (partition, runIds) => {
    const work = env.WORK.getByName(doName.work(teamOf(ctx), partition));
    const out = await work.api(meta, "metricsSessions", {}, { factoryId: f.factory_id, runIds });
    return unwrapOp<{ sessions: Record<string, MSession[]> }>(out, "metricsSessions").sessions;
  });
  const metrics = await compute(indexList(env, ctx), f.factory_id, f.scorers, since, now, sessions);
  return json({ factory_id: f.factory_id, since: new Date(since).toISOString(), metrics });
};
