// REST handlers: inbox. Owner: WS5. Items of runs live in their WorkDO
// (partition token "w"); team-level items in IndexDO ("i"). Lists and read
// state come from IndexDO.
import { doName, OfError, partitionOf, tokenKind } from "@openfactory/core";
import { type Handler, type RouteContext } from "../http/handler";
import type { Env } from "../contracts/env";
import { json, metaOf, teamOf, workFor } from "./runs";

function ownerIsWork(id: string): boolean {
  try {
    return tokenKind(partitionOf(id)) === "w";
  } catch {
    return false;
  }
}

function index(env: Env, ctx: RouteContext) {
  return env.INDEX.getByName(doName.index(teamOf(ctx)));
}

/** GET /api/v1/inbox: the caller's inbox across the team's factories. */
export const listTeamInbox: Handler = async (_req, env, ctx) => {
  const q = { ...Object.fromEntries(ctx.url.searchParams), user_id: ctx.principal?.userId ?? ctx.principal?.id ?? "" };
  return json(200, await index(env, ctx).list(metaOf(ctx), "inbox", q));
};

/** GET /api/v1/factories/{uid}/inbox */
export const listInbox: Handler = async (_req, env, ctx) => {
  const q = { ...Object.fromEntries(ctx.url.searchParams), factory_id: ctx.params.uid ?? "", scope: "team" };
  return json(200, await index(env, ctx).list(metaOf(ctx), "inbox", q));
};

/** InboxItemView defaults: recipients and is_read are always present. */
function itemView(it: unknown): unknown {
  if (typeof it !== "object" || it === null || "$error" in it) return it;
  const o = it as Record<string, unknown>;
  return { ...o, recipients: Array.isArray(o.recipients) ? o.recipients : [], is_read: o.is_read === true };
}

/** GET /api/v1/inbox/{id} */
export const getInboxItem: Handler = async (_req, env, ctx) => {
  const id = ctx.params.id ?? "";
  if (ownerIsWork(id))
    return json(200, itemView(await workFor(env, ctx, id).api(metaOf(ctx), "getInbox", { id }, null)));
  return json(200, itemView(await index(env, ctx).getInboxItem(metaOf(ctx), id)));
};

/** POST /api/v1/inbox/{id}/resolve */
export const resolveInboxItem: Handler = async (req, env, ctx) => {
  const id = ctx.params.id ?? "";
  if (!ownerIsWork(id))
    throw new OfError("feature_not_available", "team-level inbox items are resolved by their owner");
  const t = await req.text();
  return json(
    200,
    itemView(await workFor(env, ctx, id).api(metaOf(ctx), "resolveInbox", { id }, t.trim() ? JSON.parse(t) : {})),
  );
};

/** POST /api/v1/inbox/{id}/read */
export const readInboxItem: Handler = async (_req, env, ctx) => {
  await index(env, ctx).markInboxRead(metaOf(ctx), ctx.params.id ?? "");
  return json(200, { read: true });
};

/** POST /api/v1/inbox/{id}/unresolve (Engine.UnresolveInbox). */
export const unresolveInboxItem: Handler = async (_req, env, ctx) => {
  const id = ctx.params.id ?? "";
  if (!ownerIsWork(id)) throw new OfError("feature_not_available", "team-level inbox items cannot be unresolved");
  return json(200, itemView(await workFor(env, ctx, id).api(metaOf(ctx), "unresolveInbox", { id }, null)));
};
