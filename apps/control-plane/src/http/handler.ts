// Handler and route types shared by src/http/routes.ts and the owner files in
// src/api/, plus the helpers that forward a request to an owner DO.
import { OfError, doName, notImplemented, problemResponse, toOfError, type ErrorCode } from "@openfactory/core";
import type { CallMeta, Principal, Role } from "../contracts/do-messages";
import type { Env } from "../contracts/env";

/** Minimum role of a route ("public": no authentication; "worker": worker access token). */
export type RouteRole = "public" | "worker" | Role;

export type HttpMethod = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

export interface RouteContext {
  /** Path parameters, by openapi name ({uid}, {id}, ...), URL-decoded. */
  params: Record<string, string>;
  url: URL;
  requestId: string;
  route: RouteDef;
  executionCtx: ExecutionContext;
  /** Set by the authentication step (WS4) for every non-public route. */
  principal?: Principal;
}

export type Handler = (request: Request, env: Env, ctx: RouteContext) => Promise<Response>;

export interface RouteDef {
  method: HttpMethod;
  /** openapi path under /api/v1, for example "/factories/{uid}/runs". */
  path: string;
  operationId: string;
  role: RouteRole;
  handler: Handler;
}

/** The C0 stub of every handler: 501 feature_not_available. */
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

/** decodeJSON: the JSON body, or undefined for an empty body when optional. */
export async function readJson(request: Request, optional = true, max = MAX_BODY_BYTES): Promise<unknown> {
  const len = Number(request.headers.get("content-length") ?? "0");
  if (len > max) throw new OfError("invalid_request", `request body exceeds ${max} bytes`);
  const text = await request.text();
  if (text.length > max) throw new OfError("invalid_request", `request body exceeds ${max} bytes`);
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

interface OpResponseLike {
  $status: number;
  body?: unknown;
  headers?: Record<string, string>;
}
interface OpErrorLike {
  $error: {
    code: string;
    detail: string;
    reason?: string;
    provider?: string;
    authUrl?: string;
    setupUrl?: string;
    extra?: Record<string, unknown>;
  };
}

/** Turns the result of a DO api() call into a Response (or throws its OfError). */
export function opResult(out: unknown): Response {
  if (typeof out === "object" && out !== null) {
    const e = (out as OpErrorLike).$error;
    if (e && typeof e === "object") {
      throw new OfError(e.code as ErrorCode, e.detail, {
        ...(e.reason ? { reason: e.reason } : {}),
        ...(e.provider ? { provider: e.provider } : {}),
        ...(e.authUrl ? { authUrl: e.authUrl } : {}),
        ...(e.setupUrl ? { setupUrl: e.setupUrl } : {}),
        ...(e.extra ? { extra: e.extra } : {}),
      });
    }
    const r = out as OpResponseLike;
    if (typeof r.$status === "number") return json(r.body ?? null, r.$status, r.headers);
  }
  return json(out ?? null);
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
