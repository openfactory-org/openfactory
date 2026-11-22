// Handler and route types shared by src/http/routes.ts and the owner files in src/api/.
import { OfError, notImplemented, problemResponse } from "@openfactory/core";
import type { Principal, Role } from "../contracts/do-messages";
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
  /** Set by the authentication step (WS4). */
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

export function errorResponse(e: unknown, ctx: { instance?: string; requestId?: string }): Response {
  const err = e instanceof OfError ? e : new OfError("internal_error");
  return problemResponse(err, ctx);
}
