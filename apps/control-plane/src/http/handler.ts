// Handler and route types shared by src/http/routes.ts and the owner files in
// src/api/, plus the helpers that forward a request to an owner DO.
import { OfError, doName, isOfError, notImplemented, problemResponse, toOfError } from "@openfactory/core";
import type { CallMeta, Principal } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import type { RouteMeta } from "../contracts/routes";
import { readTextCapped } from "../lib/body";
import { isOpResponse, unwrapOp } from "../lib/rpc";

export type { HttpMethod, RouteRole } from "../contracts/routes";

export interface RouteContext {
  /** Path parameters, by openapi name ({uid}, {id}, ...), URL-decoded. */
  params: Record<string, string>;
  url: URL;
  requestId: string;
  route: RouteDef;
  executionCtx: ExecutionContext;
  /** Set by the authentication step for every non-public route. */
  principal?: Principal;
}

export type Handler = (request: Request, env: Env, ctx: RouteContext) => Promise<Response>;

/** A route of src/contracts/routes.ts with its handler. */
export interface RouteDef extends RouteMeta {
  handler: Handler;
}

/** The handler of an operation without an implementation: 501 feature_not_available. */
export const notImplementedHandler: Handler = async (_request, _env, ctx) =>
  problemResponse(notImplemented(`${ctx.route.operationId} is not implemented yet`), {
    instance: ctx.url.pathname,
    requestId: ctx.requestId,
  });

/** Problem response for any error; rebuilds OfError codes that crossed RPC. */
export function errorResponse(e: unknown, ctx: { instance?: string; requestId?: string }): Response {
  return problemResponse(toOfError(e).error, ctx);
}

/** JSON response (Go writeJSON: no HTML escaping, trailing newline). */
export function json(body: unknown, status = 200, headers?: Record<string, string>): Response {
  const h = new Headers({ "content-type": "application/json" });
  for (const [k, v] of Object.entries(headers ?? {})) h.set(k, v);
  if (status === 204) return new Response(null, { status, headers: h });
  return new Response(JSON.stringify(body) + "\n", { status, headers: h });
}

/** maxBodyBytes of internal/api (definitions can be large). */
export const MAX_BODY_BYTES = 8 << 20;

/**
 * decodeJSON: the JSON body, or undefined for an empty body when optional.
 * The limit counts UTF-8 bytes; the read stops after max bytes.
 */
export async function readJson(request: Request, optional = true, max = MAX_BODY_BYTES): Promise<unknown> {
  let text: string;
  try {
    text = await readTextCapped(request, max, "request body");
  } catch (e) {
    if (isOfError(e, "invalid_request")) throw new OfError("invalid_request", `request body exceeds ${max} bytes`);
    throw new OfError("invalid_request", `reading body: ${(e as Error).message}`);
  }
  if (text.trim() === "") {
    if (optional) return undefined;
    throw new OfError("invalid_request", "request body is required");
  }
  try {
    return JSON.parse(text) as unknown;
  } catch (e) {
    throw new OfError("invalid_request", `malformed JSON body: ${(e as Error).message}`);
  }
}

export function callMeta(ctx: RouteContext): CallMeta {
  return ctx.principal ? { requestId: ctx.requestId, principal: ctx.principal } : { requestId: ctx.requestId };
}

/** Path params plus query params prefixed with "?" (TeamDO.api convention). */
export function opParams(ctx: RouteContext): Record<string, string> {
  const out: Record<string, string> = { ...ctx.params };
  for (const [k, v] of ctx.url.searchParams) if (!("?" + k in out)) out["?" + k] = v;
  return out;
}

/** Turns the result of a DO api() call into a Response (or throws its OfError). */
export function opResult(out: unknown): Response {
  if (isOpResponse(out)) return json(out.body ?? null, out.$status, out.headers);
  return json(unwrapOp(out) ?? null);
}

/** The team of the request: the principal's team, else the default team. */
export function teamOf(ctx: RouteContext): string {
  return ctx.principal?.teamId || "default";
}

/** Calls TeamDO.api(meta, op, params, body) and returns the result as a Response. */
export async function callTeam(
  env: Env,
  ctx: RouteContext,
  op: string,
  body?: unknown,
  extraParams?: Record<string, string>,
): Promise<Response> {
  const stub = env.TEAM.getByName(doName.team(teamOf(ctx)));
  return opResult(await stub.api(callMeta(ctx), op, { ...opParams(ctx), ...extraParams }, body));
}

/** A handler that forwards to TeamDO.api with the route's operationId and the JSON body. */
export function teamApi(op?: string): Handler {
  return async (request, env, ctx) => {
    const body = request.method === "GET" || request.method === "DELETE" ? undefined : await readJson(request);
    return callTeam(env, ctx, op ?? ctx.route.operationId, body);
  };
}
