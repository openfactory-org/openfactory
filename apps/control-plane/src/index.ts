// The control-plane Worker: fetch router, cron and queue handlers, and the
// Durable Object classes (exported for wrangler.jsonc "exports").
import { DEFAULT_TEAM, OfError, doName, newId, notImplemented, problemResponse } from "@openfactory/core";
import type { JobMessage } from "./contracts/do-messages";
import type { Env } from "./contracts/env";
import * as workersApi from "./api/workers";
import { authenticate, hasSessionCookie } from "./auth/authenticate";
import { isFactoryScoped, requireRole } from "./auth/roles";
import { handleAuthRoute, loginProviders } from "./auth/routes";
import { clearSessionCookie } from "./auth/session";
import { cronHooksList, matchServerRoutes } from "./builder";
import { handleCloudIdRoute } from "./cloudid/routes";
import { errorResponse, type Handler, type RouteContext } from "./http/handler";
import { API_PREFIX, Router } from "./http/router";
import { ROUTES } from "./http/routes";

export { BenchmarkDO } from "./do/benchmark/index";
export { DispatchDO } from "./do/dispatch/index";
export { IndexDO } from "./do/index/index";
export { IngressDO } from "./do/ingress/index";
export { RouterDO } from "./do/router/index";
export { TeamDO } from "./do/team/index";
export { WorkDO } from "./do/work/index";
export { WorkerHubDO } from "./do/workerhub/index";
export { registerServerRoute, registerCronHook, type ServerRouteHandler, type CronHook } from "./builder";
export type { Env } from "./contracts/env";

const router = new Router(ROUTES);

/** Server paths outside /api/v1 (wrangler.jsonc run_worker_first). */
const SERVER_PREFIXES = [
  "/runner/",
  "/worker/",
  "/mcp",
  "/oauth/",
  "/.well-known/",
  "/auth/",
  "/webhooks/",
  "/integrations/",
  "/plugins/",
  "/metrics",
];

function requestIdOf(request: Request): string {
  const given = request.headers.get("x-request-id");
  return given && given.length <= 128 ? given : newId("req").slice(4);
}

function withRequestId(res: Response, requestId: string, extraCookie?: string): Response {
  if (res.headers.has("x-request-id") && !extraCookie) return res;
  if (res.status === 101) return res;
  const out = new Response(res.body, res);
  if (!out.headers.has("x-request-id")) out.headers.set("x-request-id", requestId);
  if (extraCookie) out.headers.append("set-cookie", extraCookie);
  return out;
}

async function handleApi(request: Request, env: Env, ctx: ExecutionContext, url: URL, requestId: string): Promise<Response> {
  const path = url.pathname.slice(API_PREFIX.length) || "/";
  const problemCtx = { instance: url.pathname, requestId };
  // Worker protocol v2 socket: the worker proves itself inside the socket (WS6).
  const connect = (workersApi as unknown as { connectWorker?: Handler }).connectWorker;
  if (path === "/workers/connect" && request.method === "GET" && connect) {
    const route = ROUTES.find((r) => r.operationId === "getWorker")!;
    try {
      return await connect(request, env, { params: {}, url, requestId, route, executionCtx: ctx });
    } catch (e) {
      return errorResponse(e, problemCtx);
    }
  }
  const m = router.match(request.method, path);
  if (m.kind === "not_found") return problemResponse(new OfError("resource_not_found", `no route for ${url.pathname}`), problemCtx);
  if (m.kind === "method_not_allowed") {
    return new Response("Method Not Allowed\n", { status: 405, headers: { allow: m.allow.join(", "), "x-request-id": requestId } });
  }
  const rctx: RouteContext = { params: m.params, url, requestId, route: m.route, executionCtx: ctx };
  let clearCookie: string | undefined;
  try {
    if (m.route.role !== "public") {
      const p = await authenticate(request, env);
      if (!p) {
        if (hasSessionCookie(request)) clearCookie = clearSessionCookie(env);
        throw new OfError("authentication_required", "missing credentials: sign in or send an Authorization: Bearer header", {
          extra: { login_providers: loginProviders(env) },
        });
      }
      // Team-wide routes are checked here and again in the owner DO; factory-scoped routes in the owner DO.
      if (!isFactoryScoped(m.route)) requireRole(p, "", m.route.role);
      rctx.principal = p;
    }
    const res = await m.route.handler(request, env, rctx);
    return withRequestId(res, requestId, clearCookie);
  } catch (e) {
    if (!(e instanceof OfError) && !(e instanceof Error && /^[a-z_]+(: |$)/.test(e.message))) {
      console.error("api: internal error", request.method, url.pathname, e);
    }
    return withRequestId(errorResponse(e, problemCtx), requestId, clearCookie);
  }
}

async function handleServer(request: Request, env: Env, ctx: ExecutionContext, url: URL, requestId: string): Promise<Response> {
  for (const h of matchServerRoutes(url.pathname)) {
    const res = await h(request, env, ctx, url, requestId);
    if (res) return withRequestId(res, requestId);
  }
  if (url.pathname.startsWith("/auth/")) {
    const res = await handleAuthRoute(request, env, url, requestId);
    if (res) return withRequestId(res, requestId);
  }
  if (url.pathname.startsWith("/.well-known/")) {
    const res = await handleCloudIdRoute(request, env, url);
    if (res) return withRequestId(res, requestId);
  }
  return problemResponse(notImplemented(`${url.pathname} is not implemented yet`), { instance: url.pathname, requestId });
}

export default {
  async fetch(request, env, ctx): Promise<Response> {
    const url = new URL(request.url);
    const requestId = requestIdOf(request);
    if (url.pathname === "/healthz") return new Response("ok\n");
    if (url.pathname === "/readyz") return new Response("ok\n");
    if (url.pathname === API_PREFIX || url.pathname.startsWith(API_PREFIX + "/")) {
      return handleApi(request, env, ctx, url, requestId);
    }
    if (SERVER_PREFIXES.some((p) => url.pathname.startsWith(p))) return handleServer(request, env, ctx, url, requestId);
    return env.ASSETS.fetch(request);
  },

  async scheduled(_controller, env, ctx): Promise<void> {
    // Safety net (1/min): re-arm the alarms of the team's TeamDO and DispatchDO, then owner hooks.
    const jobs: Promise<unknown>[] = [
      env.TEAM.getByName(doName.team(DEFAULT_TEAM)).ensureAlarm(),
      (env.DISPATCH.getByName(doName.dispatch(DEFAULT_TEAM)) as unknown as { ensureAlarm?: () => Promise<void> }).ensureAlarm?.() ??
        Promise.resolve(),
      env.INDEX.getByName(doName.index(DEFAULT_TEAM)).ensureAlarm(),
      ...cronHooksList().map((h) => h(env, ctx)),
    ];
    for (const r of await Promise.allSettled(jobs)) {
      if (r.status === "rejected") console.error("cron: safety net step failed", r.reason);
    }
  },

  async queue(batch, _env, _ctx): Promise<void> {
    // Unordered fan-out jobs (judge results, scoring, retention, compaction). Owner: WS13.
    for (const msg of batch.messages) msg.ack();
  },
} satisfies ExportedHandler<Env, JobMessage>;
