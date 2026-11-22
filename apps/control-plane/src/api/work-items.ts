// REST handlers: work items. Owner: WS5. Lists from IndexDO; the WorkDO of
// the work item (by its partition token) owns reads and writes.
import { doName } from "@openfactory/core";
import { runView } from "../engine/view";
import type { Run, WorkItem } from "../engine/types";
import type { Handler } from "../http/handler";
import { json, metaOf, teamOf, workFor } from "./runs";

async function body(req: Request): Promise<unknown> {
  const t = await req.text();
  return t.trim() ? (JSON.parse(t) as unknown) : {};
}

/** GET /api/v1/factories/{uid}/work-items */
export const listWorkItems: Handler = async (_req, env, ctx) => {
  const q = { ...Object.fromEntries(ctx.url.searchParams), factory_id: ctx.params.uid ?? "" };
  return json(200, await env.INDEX.getByName(doName.index(teamOf(ctx))).list(metaOf(ctx), "workItems", q));
};

/** GET /api/v1/work-items/{id}: the work item with its timeline and runs. */
export const getWorkItem: Handler = async (_req, env, ctx) => {
  const id = ctx.params.id ?? "";
  const stub = workFor(env, ctx, id);
  const wi = (await stub.api(metaOf(ctx), "getWorkItem", { id }, null)) as WorkItem;
  const tl = (await stub.api(metaOf(ctx), "getWorkItemTimeline", { id }, null)) as { events: unknown[] };
  const runs = (await stub.api(metaOf(ctx), "listWorkItemRuns", { id }, null)) as { runs: Run[] };
  return json(200, { ...wi, timeline: tl.events, runs: runs.runs.map((r) => runView(r)) });
};

const op =
  (name: string, status = 200): Handler =>
  async (req, env, ctx) =>
    json(status, await workFor(env, ctx, ctx.params.id ?? "").api(metaOf(ctx), name, { id: ctx.params.id ?? "" }, await body(req)));

/** POST /api/v1/work-items/{id}/messages */
export const messageWorkItem: Handler = op("workItemMessage", 202);
/** POST /api/v1/work-items/{id}/cancel */
export const cancelWorkItem: Handler = op("workItemCancel");
/** POST /api/v1/work-items/{id}/complete */
export const completeWorkItem: Handler = op("workItemComplete");
/** POST /api/v1/work-items/{id}/stage */
export const setWorkItemStage: Handler = op("workItemStage");
