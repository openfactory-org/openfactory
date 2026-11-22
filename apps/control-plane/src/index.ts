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
import { errorResponse, type RouteContext, type RouteDef } from "./http/handler";
import { API_PREFIX, Router } from "./http/router";
import { ROUTES } from "./http/routes";
import { handleMcpRequest } from "./mcp/index";
import { handleCompactionJob } from "./compaction/consumer";
import { expire as expireEventContext } from "./eventctx";
import { handleRetentionJob } from "./retention/consumer";
import { SCORING_JOB_KINDS, handleScoringJob } from "./scoring/jobs";
import { teamOfDoName } from "./lib/outbox/dest";
import { bootstrapControlPlane } from "./wiring";

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

// The composition root: every isolate loads this module before it serves.
bootstrapControlPlane();

/**
 * Routes under /api/v1 that are not REST operations of api/openapi.json.
 * GET /workers/connect is the worker protocol v2 WebSocket. It is public:
 * the worker proves itself inside the socket. Its two literal segments win
 * over GET /workers/{id}.
 */
const SOCKET_ROUTES: readonly RouteDef[] = [
  {
    method: "GET",
    path: "/workers/connect",
    operationId: "connectWorker",
    role: "public",
    handler: workersApi.connectWorker,
  },
];

const router = new Router([...ROUTES, ...SOCKET_ROUTES]);

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

async function handleApi(
  request: Request,
  env: Env,
  ctx: ExecutionContext,
  url: URL,
  requestId: string,
): Promise<Response> {
  const path = url.pathname.slice(API_PREFIX.length) || "/";
  const problemCtx = { instance: url.pathname, requestId };
  const m = router.match(request.method, path);
  if (m.kind === "not_found")
    return problemResponse(new OfError("resource_not_found", `no route for ${url.pathname}`), problemCtx);
  if (m.kind === "method_not_allowed") {
    return new Response("Method Not Allowed\n", {
      status: 405,
      headers: { allow: m.allow.join(", "), "x-request-id": requestId },
    });
  }
  const rctx: RouteContext = { params: m.params, url, requestId, route: m.route, executionCtx: ctx };
  let clearCookie: string | undefined;
  try {
    if (m.route.role !== "public") {
      const p = await authenticate(request, env);
      if (!p) {
        if (hasSessionCookie(request)) clearCookie = clearSessionCookie(env);
        throw new OfError(
          "authentication_required",
          "missing credentials: sign in or send an Authorization: Bearer header",
          {
            extra: { login_providers: loginProviders(env) },
          },
        );
      }
      // Team-wide routes are checked here and again in the owner DO. Factory-scoped routes are checked
      // with the role for the resource's factory: in the owner DO (TeamDO, WorkDO, IndexDO, BenchmarkDO),
      // or in the handler when the DO cannot know the route role (api/measure.ts, api/webhooks.ts).
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

async function handleServer(
  request: Request,
  env: Env,
  ctx: ExecutionContext,
  url: URL,
  requestId: string,
): Promise<Response> {
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
  return problemResponse(notImplemented(`${url.pathname} is not implemented yet`), {
    instance: url.pathname,
    requestId,
  });
}

/** The MCP paths: /mcp, /api/v1/mcp/factory, /mcp/proxy/*, /mcp/integrations/*, /.well-known/oauth-*, /oauth/*. */
function isMcpPath(path: string): boolean {
  return (
    path === "/mcp" ||
    path.startsWith("/mcp/") ||
    path === API_PREFIX + "/mcp/factory" ||
    path.startsWith("/.well-known/oauth-") ||
    path.startsWith("/oauth/")
  );
}

const SCORING_KINDS = new Set<string>(SCORING_JOB_KINDS);

/** The team of a job: from the DO name it names, else the default team. */
function teamOfJob(msg: JobMessage): string {
  const name = "workDO" in msg ? msg.workDO : "doName" in msg ? msg.doName : "";
  return teamOfDoName(name) || DEFAULT_TEAM;
}

async function handleJob(env: Env, msg: JobMessage): Promise<void> {
  if (SCORING_KINDS.has(msg.kind)) return handleScoringJob(env, msg, teamOfJob(msg));
  switch (msg.kind) {
    case "retention.sweep":
      return handleRetentionJob(env, msg);
    case "compaction.run":
      return handleCompactionJob(env, msg);
  }
  console.warn("queue: unknown job kind", (msg as { kind?: string }).kind);
}

/**
 * Hourly at minute 0: one retention step for the team's TeamDO and IndexDO
 * (each step sends the next), and removal of expired event context bundles.
 */
async function scheduleRetention(env: Env, scheduledTime: number): Promise<void> {
  const d = new Date(scheduledTime);
  if (d.getUTCMinutes() !== 0) return;
  await expireEventContext(env.DATA, scheduledTime);
  const hour = Math.floor(scheduledTime / 3_600_000);
  const names = [doName.team(DEFAULT_TEAM), doName.index(DEFAULT_TEAM)];
  await env.JOBS.sendBatch(
    names.map((n) => ({
      body: { kind: "retention.sweep", msgId: `retention:${n}:${hour}`, doName: n } satisfies JobMessage,
    })),
  );
}

export default {
  async fetch(request, env, ctx): Promise<Response> {
    const url = new URL(request.url);
    const requestId = requestIdOf(request);
    if (url.pathname === "/healthz") return new Response("ok\n");
    if (url.pathname === "/readyz") return new Response("ok\n");
    // MCP before the REST API: /api/v1/mcp/factory is the Factory MCP alias.
    if (isMcpPath(url.pathname)) {
      const res = await handleMcpRequest(request, env, ctx);
      if (res) return withRequestId(res, requestId);
    }
    if (url.pathname === API_PREFIX || url.pathname.startsWith(API_PREFIX + "/")) {
      return handleApi(request, env, ctx, url, requestId);
    }
    if (SERVER_PREFIXES.some((p) => url.pathname.startsWith(p))) return handleServer(request, env, ctx, url, requestId);
    return env.ASSETS.fetch(request);
  },

  async scheduled(controller, env, ctx): Promise<void> {
    // Safety net (1/min): re-arm the alarms of the team's TeamDO and DispatchDO, then owner hooks.
    const jobs: Promise<unknown>[] = [
      env.TEAM.getByName(doName.team(DEFAULT_TEAM)).ensureAlarm(),
      env.DISPATCH.getByName(doName.dispatch(DEFAULT_TEAM)).ensureAlarm(),
      env.INDEX.getByName(doName.index(DEFAULT_TEAM)).ensureAlarm(),
      ...cronHooksList().map((h) => h(env, ctx)),
      scheduleRetention(env, controller.scheduledTime),
    ];
    for (const r of await Promise.allSettled(jobs)) {
      if (r.status === "rejected") console.error("cron: safety net step failed", r.reason);
    }
  },

  async queue(batch, env, _ctx): Promise<void> {
    // Unordered fan-out jobs (judge results, scoring, retention, compaction). Every handler is idempotent.
    await Promise.all(
      batch.messages.map(async (msg) => {
        try {
          await handleJob(env, msg.body);
          msg.ack();
        } catch (e) {
          console.error("queue: job failed", msg.body.kind, msg.body.msgId, e);
          msg.retry({ delaySeconds: Math.min(300, 2 ** Math.min(msg.attempts, 8)) });
        }
      }),
    );
  },
} satisfies ExportedHandler<Env, JobMessage>;
