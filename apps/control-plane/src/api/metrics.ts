// REST handlers: metrics. Owner: WS13.
import { json, type Handler } from "../http/handler";
import { compute } from "../metrics/collect";
import { factoryAccess, indexList, parseSince } from "./measure";

/** GET /api/v1/factories/{uid}/metrics: Dashboard metrics. Computed from the IndexDO read model. */
export const getMetrics: Handler = async (_req, env, ctx) => {
  const f = await factoryAccess(env, ctx);
  const now = Date.now();
  const since = parseSince(ctx.url.searchParams.get("since"), now) ?? now - 30 * 86_400_000;
  const metrics = await compute(indexList(env, ctx), f.factory_id, f.scorers, since, now);
  return json({ factory_id: f.factory_id, since: new Date(since).toISOString(), metrics });
};
