// REST handlers: inbox. Owner: WS5. Items of runs live in their WorkDO
// (partition token "w"); team-level items in IndexDO ("i"). Lists and read
// state come from IndexDO.
import { doName, OfError, partitionOf, tokenKind } from "@openfactory/core";
import { type Handler, type RouteContext } from "../http/handler";
import type { Env } from "../contracts/env";
import { factoryAccess } from "./measure";
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
  // {uid} may be an alias or a name: IndexDO filters and authorizes by factory ID.
  const { factory_id } = await factoryAccess(env, ctx);
  const q = { ...Object.fromEntries(ctx.url.searchParams), factory_id, scope: "team" };
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

/**
 * The caller's name or email for the follow-up text (actorLabel): the run
 * reads a name, never a raw user ID. "" when the caller is not a user.
 */
async function callerLabel(env: Env, ctx: RouteContext): Promise<string> {
  const p = ctx.principal;
  if (!p?.userId) return "";
  try {
    const meta = {
      requestId: ctx.requestId,
      principal: {
        kind: "service_account" as const,
        id: "inbox",
        teamId: p.teamId,
        role: "admin" as const,
        factoryRoles: {},
      },
    };
    const u = (await env.TEAM.getByName(doName.team(teamOf(ctx))).api(meta, "getUser", { id: p.userId }, null)) as {
      name?: string;
      email?: string;
      $error?: unknown;
    } | null;
    if (!u || u.$error) return "";
    return (u.name ?? "").trim() || (u.email ?? "").trim();
  } catch {
    return "";
  }
}

/** POST /api/v1/inbox/{id}/resolve */
export const resolveInboxItem: Handler = async (req, env, ctx) => {
  const id = ctx.params.id ?? "";
  if (!ownerIsWork(id))
    throw new OfError("feature_not_available", "team-level inbox items are resolved by their owner");
  const t = await req.text();
  const body = {
    ...((t.trim() ? JSON.parse(t) : {}) as Record<string, unknown>),
    actor_label: await callerLabel(env, ctx),
  };
  return json(200, itemView(await workFor(env, ctx, id).api(metaOf(ctx), "resolveInbox", { id }, body)));
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
  const body = { actor_label: await callerLabel(env, ctx) };
  return json(200, itemView(await workFor(env, ctx, id).api(metaOf(ctx), "unresolveInbox", { id }, body)));
};
