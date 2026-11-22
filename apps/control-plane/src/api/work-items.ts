// REST handlers: work items. Lists from IndexDO; the WorkDO of
// the work item (by its partition token) owns reads and writes.
import { doName } from "@openfactory/core";
import { runView } from "../engine/view";
import type { Run, WorkItem } from "../engine/types";
import { callMeta, json, readJson, teamOf, type Handler } from "../http/handler";
import { factoryAccess } from "./measure";
import { workFor } from "./runs";

/** The JSON body of a request; {} for an empty body. */
async function body(req: Request): Promise<unknown> {
  return (await readJson(req)) ?? {};
}

/** GET /api/v1/factories/{uid}/work-items */
export const listWorkItems: Handler = async (_req, env, ctx) => {
  // {uid} may be an alias or a name: IndexDO filters and authorizes by factory ID.
  const { factory_id } = await factoryAccess(env, ctx);
  const q = { ...Object.fromEntries(ctx.url.searchParams), factory_id };
  return json(await env.INDEX.getByName(doName.index(teamOf(ctx))).list(callMeta(ctx), "workItems", q));
};

function base(env: { OF_PUBLIC_URL?: string }): string {
  return (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
}

/** workItemView: the work item with its dashboard URL (links.WorkItem). */
function wiView(env: { OF_PUBLIC_URL?: string }, wi: WorkItem): Record<string, unknown> {
  return { ...wi, url: `${base(env)}/f/${encodeURIComponent(wi.factory_id)}/work/${encodeURIComponent(wi.id)}` };
}

/** GET /api/v1/work-items/{id}: the work item with its timeline, runs and open inbox items. */
export const getWorkItem: Handler = async (_req, env, ctx) => {
  const id = ctx.params.id ?? "";
  const stub = workFor(env, ctx, id);
  const wi = (await stub.api(callMeta(ctx), "getWorkItem", { id }, null)) as WorkItem;
  const tl = (await stub.api(callMeta(ctx), "getWorkItemTimeline", { id }, null)) as { events: unknown[] };
  const runs = (await stub.api(callMeta(ctx), "listWorkItemRuns", { id }, null)) as { runs: Run[] };
  const inbox = (await stub.api(callMeta(ctx), "listWorkItemInbox", { id }, null)) as { items: unknown[] };
  const pub = base(env);
  return json({
    ...wiView(env, wi),
    timeline: tl.events ?? [],
    runs: runs.runs.map((r) => runView(r, pub ? { publicUrl: pub } : {})),
    inbox: inbox.items ?? [],
  });
};

const op =
  (name: string, status = 200): Handler =>
  async (req, env, ctx) =>
    json(
      await workFor(env, ctx, ctx.params.id ?? "").api(
        callMeta(ctx),
        name,
        { id: ctx.params.id ?? "" },
        await body(req),
      ),
      status,
    );

const wiOp =
  (name: string): Handler =>
  async (req, env, ctx) => {
    const out = (await workFor(env, ctx, ctx.params.id ?? "").api(
      callMeta(ctx),
      name,
      { id: ctx.params.id ?? "" },
      await body(req),
    )) as WorkItem | { work_item: WorkItem };
    return json(wiView(env, "work_item" in out ? out.work_item : out));
  };

/** POST /api/v1/work-items/{id}/messages */
export const messageWorkItem: Handler = op("workItemMessage", 202);
/** POST /api/v1/work-items/{id}/cancel */
export const cancelWorkItem: Handler = wiOp("workItemCancel");
/** POST /api/v1/work-items/{id}/complete */
export const completeWorkItem: Handler = wiOp("workItemComplete");
/** POST /api/v1/work-items/{id}/stage */
export const setWorkItemStage: Handler = wiOp("workItemStage");
