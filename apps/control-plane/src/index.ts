// The control-plane Worker: fetch router, cron and queue handlers, and the
// Durable Object classes (exported for wrangler.jsonc "exports").
import { newId, notImplemented, problemResponse, OfError } from "@openfactory/core";
import type { JobMessage } from "./contracts/do-messages";
import type { Env } from "./contracts/env";
import { errorResponse } from "./http/handler";
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

const router = new Router(ROUTES);

/** Server paths outside /api/v1 (wrangler.jsonc run_worker_first). Owners fill them in. */
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
  return given && given.length <= 128 ? given : newId("req");
}

async function handleApi(
  request: Request,
  env: Env,
  ctx: ExecutionContext,
  url: URL,
  requestId: string,
): Promise<Response> {
  const path = url.pathname.slice(API_PREFIX.length) || "/";
  const m = router.match(request.method, path);
  const problemCtx = { instance: url.pathname, requestId };
  if (m.kind === "not_found")
    return problemResponse(new OfError("resource_not_found", `no route for ${url.pathname}`), problemCtx);
  if (m.kind === "method_not_allowed") {
    return new Response("Method Not Allowed\n", {
      status: 405,
      headers: { allow: m.allow.join(", "), "x-request-id": requestId },
    });
  }
  try {
    const res = await m.route.handler(request, env, {
      params: m.params,
      url,
      requestId,
      route: m.route,
      executionCtx: ctx,
    });
    if (!res.headers.has("x-request-id")) {
      const out = new Response(res.body, res);
      out.headers.set("x-request-id", requestId);
      return out;
    }
    return res;
  } catch (e) {
    if (!(e instanceof OfError)) console.error("api: internal error", request.method, url.pathname, e);
    return errorResponse(e, problemCtx);
  }
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
    if (SERVER_PREFIXES.some((p) => url.pathname.startsWith(p))) {
      return problemResponse(notImplemented(`${url.pathname} is not implemented yet`), {
        instance: url.pathname,
        requestId,
      });
    }
    return env.ASSETS.fetch(request);
  },

  async scheduled(_controller, _env, _ctx): Promise<void> {
    // Safety net (1/min): re-arm TeamDO and DispatchDO alarms. Owner: WS4/WS5.
  },

  async queue(batch, _env, _ctx): Promise<void> {
    // Unordered fan-out jobs (judge results, scoring, retention, compaction). Owner: WS13.
    for (const msg of batch.messages) msg.ack();
  },
} satisfies ExportedHandler<Env, JobMessage>;
