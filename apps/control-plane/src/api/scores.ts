// REST handlers: scores. Owner: WS13.
import { OfError, workDOName, type Score } from "@openfactory/core";
import { requireRole } from "../auth/roles";
import { callTeam, errorResponse, json, readJson, teamApi, teamOf, type Handler } from "../http/handler";
import { bodyObject, str } from "../lib/body";
import { scoringService } from "../scoring/jobs";
import { factoryAccess, indexList, parseSince } from "./measure";

/** GET /api/v1/factories/{uid}/scorers: List scorers. */
export const listScorers: Handler = teamApi();

/** GET /api/v1/factories/{uid}/scores: List scores (IndexDO read model). */
export const listScores: Handler = async (_req, env, ctx) => {
  const f = await factoryAccess(env, ctx);
  const q = ctx.url.searchParams;
  let scorerId = q.get("scorer_id") ?? "";
  const slug = q.get("scorer") ?? "";
  if (slug && !scorerId) {
    const sc = f.scorers.find((s) => s.slug === slug || s.id === slug);
    if (!sc) throw new OfError("resource_not_found", `scorer ${slug} not found`);
    scorerId = sc.id;
  }
  const since = parseSince(q.get("since"));
  const params: Record<string, string> = { factory_id: f.factory_id };
  for (const k of ["limit", "cursor", "sort_order", "sort_by"]) if (q.get(k)) params[k] = q.get(k)!;
  if (scorerId) params["scorer_id"] = scorerId;
  const page = (await indexList(env, ctx)("scores", params)) as { items: Score[]; page_info: unknown };
  const passed = q.get("passed");
  const agent = q.get("agent");
  const items = page.items.filter(
    (s) => (passed === null || passed === "" || s.passed === (passed === "true")) && (!agent || s.agent_name === agent) && (since === undefined || Date.parse(s.created_at) >= since),
  );
  return json({ ...page, items });
};

/** POST /api/v1/runs/{runId}/score: Score a run with a scorer now. 202 while the judge runs. */
export const scoreRun: Handler = async (req, env, ctx) => {
  try {
    const body = bodyObject(await readJson(req, false));
    const scorer = str(body, "scorer_id");
    const teamId = teamOf(ctx);
    const run = (await env.WORK.getByName(workDOName(teamId, ctx.params["runId"] ?? "")).getRun(ctx.params["runId"] ?? "")) as { id: string; factory_id: string };
    requireRole(ctx.principal, run.factory_id, "editor");
    const score = await scoringService(env, teamId).scoreRun(scorer, run.id);
    return json(score, score.id === "" ? 202 : 200);
  } catch (e) {
    return errorResponse(e, { instance: ctx.url.pathname, requestId: ctx.requestId });
  }
};

export { callTeam };
