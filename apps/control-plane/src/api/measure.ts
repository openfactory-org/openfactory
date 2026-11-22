// Shared helpers of the WS13 handlers (scores, benchmarks, metrics, proposals,
// self-improvement, spend): factory access through TeamDO, IndexDO reads, since.
import { OfError, doName } from "@openfactory/core";
import type { Scorer } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { callMeta, teamOf, type RouteContext } from "../http/handler";
import { unwrapOp } from "../scoring/clients";
import { requireRole } from "../auth/roles";

export interface FactoryAccess {
  factory_id: string;
  team_id: string;
  scorers: Scorer[];
}

/**
 * Resolves the factory of the path ({uid}: an ID, alias or name) and returns
 * its ID, team and scorers. TeamDO checks that the caller is of the team;
 * this checks the route role on the resolved factory (the scoringFactory op
 * has no route, so TeamDO cannot know the route role).
 */
export async function factoryAccess(env: Env, ctx: RouteContext): Promise<FactoryAccess> {
  const stub = env.TEAM.getByName(doName.team(teamOf(ctx)));
  const params: Record<string, string> = { ...ctx.params };
  for (const [k, v] of ctx.url.searchParams) params["?" + k] = v;
  const f = unwrapOp<FactoryAccess>(await stub.api(callMeta(ctx), "scoringFactory", params, undefined));
  requireRole(ctx.principal, f.factory_id, ctx.route.role);
  return f;
}

/** IndexDO.list with the call meta of the request. */
export function indexList(env: Env, ctx: RouteContext) {
  const stub = env.INDEX.getByName(doName.index(teamOf(ctx)));
  const meta = callMeta(ctx);
  return (op: string, params: Record<string, string>): Promise<unknown> => stub.list(meta, op, params);
}

const REL = /^(\d+)([hdw])$/;

/** since: RFC 3339, YYYY-MM-DD, or a relative duration (24h, 7d, 2w). Returns ms or undefined. */
export function parseSince(v: string | null, nowMs = Date.now()): number | undefined {
  const s = (v ?? "").trim();
  if (s === "") return undefined;
  const m = REL.exec(s);
  if (m) return nowMs - Number(m[1]) * { h: 3_600_000, d: 86_400_000, w: 604_800_000 }[m[2] as "h"];
  const t = Date.parse(s);
  if (Number.isNaN(t))
    throw new OfError(
      "invalid_request",
      `query parameter "since" must be an RFC 3339 time, YYYY-MM-DD or a duration such as 7d`,
    );
  return t;
}
