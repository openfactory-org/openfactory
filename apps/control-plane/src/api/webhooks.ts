// REST handlers: webhooks (owner WS10a). Webhook records live in TeamDO
// (src/do/team/schedules.ts); the delivery log lives in IngressDO
// (partition "webhook:<id>"). The worker-side ingress handlers for
// POST /webhooks/w/{uid}[/{token}] and POST /integrations/github/webhook are
// at the end of this file.
import { OfError, doName, type Event, type Webhook, type WebhookDelivery } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { requireRole } from "../auth/roles";
import { callMeta, json, opParams, opResult, teamApi, teamOf, type Handler, type RouteContext } from "../http/handler";
import { teamIntegrations, teamSecrets, resolverFor } from "../forges/env";
import { handleGitHubWebhook, type FactoryInfo } from "../forges/github/webhook";
import { matchPayload } from "../triggers/payload";
import { handleWebhookDelivery, webhookPartition } from "../webhooks/ingress";
import { isOpError, unwrapOp } from "../lib/rpc";

/** GET /api/v1/factories/{uid}/webhooks */
export const listWebhooks: Handler = teamApi();
/** POST /api/v1/factories/{uid}/webhooks */
export const createWebhook: Handler = teamApi();
/** GET /api/v1/webhooks/{id} */
export const getWebhook: Handler = teamApi();
/** PATCH /api/v1/webhooks/{id} */
export const updateWebhook: Handler = teamApi();
/** DELETE /api/v1/webhooks/{id} */
export const deleteWebhook: Handler = teamApi();
/** POST /api/v1/webhooks/{id}/rotate-secret */
export const rotateWebhookSecret: Handler = teamApi();

/**
 * The webhook of the route. TeamDO checks that the caller is of the team;
 * this checks the route role on the webhook's factory (the webhookForRoute op
 * has no route, so TeamDO cannot know the route role).
 */
async function webhookForRoute(env: Env, ctx: RouteContext): Promise<Webhook> {
  const stub = env.TEAM.getByName(doName.team(teamOf(ctx)));
  const res = opResult(await stub.api(callMeta(ctx), "webhookForRoute", opParams(ctx), undefined));
  const wh = (await res.json()) as Webhook;
  requireRole(ctx.principal, wh.factory_id, ctx.route.role);
  return wh;
}

function ingressStub(env: Env, ctx: RouteContext, webhookId: string) {
  return env.INGRESS.getByName(doName.ingress(teamOf(ctx), webhookPartition(webhookId)));
}

/** GET /api/v1/webhooks/{id}/deliveries (IngressDO). */
export const listDeliveries: Handler = async (_req, env, ctx) => {
  const wh = await webhookForRoute(env, ctx);
  return opResult(await ingressStub(env, ctx, wh.id).deliveries(callMeta(ctx), "listDeliveries", opParams(ctx)));
};

/** GET /api/v1/webhooks/{id}/deliveries/{deliveryId} (IngressDO). */
export const getDelivery: Handler = async (_req, env, ctx) => {
  const wh = await webhookForRoute(env, ctx);
  return opResult(await ingressStub(env, ctx, wh.id).deliveries(callMeta(ctx), "getDelivery", opParams(ctx)));
};

interface Check {
  passed: boolean;
  detail: string;
}

function parseWebhookIds(raw: unknown): string[] {
  if (Array.isArray(raw) && raw.every((x) => typeof x === "string")) return raw as string[];
  const inn = (raw as { in?: unknown } | null)?.in;
  if (Array.isArray(inn) && inn.every((x) => typeof x === "string")) return inn as string[];
  throw new OfError("invalid_request", "filter.webhook_ids must be a list of webhook UIDs");
}

/** POST /api/v1/webhooks/{id}/test-filter (pure, plus an IngressDO read of a stored delivery). */
export const testWebhookFilter: Handler = async (request, env, ctx) => {
  const wh = await webhookForRoute(env, ctx);
  const text = await request.text();
  let req: { filter?: unknown; pattern?: unknown; delivery_id?: string; body?: unknown };
  try {
    req = text.trim() === "" ? {} : (JSON.parse(text) as typeof req);
  } catch (e) {
    throw new OfError("invalid_request", `malformed JSON body: ${(e as Error).message}`);
  }
  let pattern = req.pattern;
  let ids: string[] = [];
  let hasIds = false;
  if (req.filter !== undefined && req.filter !== null) {
    if (typeof req.filter !== "object" || Array.isArray(req.filter))
      throw new OfError("invalid_request", "filter must be an object");
    const f = req.filter as { webhook_ids?: unknown; payload?: unknown };
    if (f.webhook_ids !== undefined && f.webhook_ids !== null) {
      hasIds = true;
      ids = parseWebhookIds(f.webhook_ids);
    }
    if (pattern === undefined || pattern === null) pattern = f.payload;
  }
  const res: {
    would_trigger: boolean;
    result: string;
    webhook_check: Check;
    payload_check: Check;
    delivery_id?: string;
  } = {
    would_trigger: false,
    result: "",
    webhook_check: !hasIds
      ? { passed: true, detail: "no webhook_ids constraint" }
      : ids.includes(wh.id)
        ? { passed: true, detail: `webhook ${wh.id} is selected` }
        : { passed: false, detail: `webhook_ids does not include ${wh.id}` },
    payload_check: { passed: false, detail: "" },
  };
  let body: unknown = req.body;
  if (body === undefined || body === null) {
    const params = { ...opParams(ctx), ...(req.delivery_id ? { deliveryId: req.delivery_id } : {}) };
    const op = req.delivery_id ? "getDelivery" : "latestAcceptedDelivery";
    const d = (await (
      await opResult(await ingressStub(env, ctx, wh.id).deliveries(callMeta(ctx), op, params))
    ).json()) as { id?: string; body?: unknown } | null;
    if (!d)
      throw new OfError(
        "invalid_request",
        "no stored accepted delivery to test against; send a test delivery or pass body",
      );
    if (d.body === undefined || d.body === null)
      throw new OfError(
        "invalid_request",
        `delivery ${d.id} has no stored payload (only accepted deliveries keep their body)`,
      );
    body = d.body;
    if (d.id) res.delivery_id = d.id;
  }
  if (pattern === undefined || pattern === null) {
    res.payload_check = { passed: true, detail: "no payload filter: every delivery matches" };
  } else {
    let ok: boolean;
    try {
      // A raw body string keeps its numbers exact.
      ok = matchPayload(pattern, typeof body === "string" ? body : body);
    } catch (e) {
      throw new OfError("invalid_request", `invalid payload filter: ${(e as Error).message}`);
    }
    res.payload_check = ok
      ? { passed: true, detail: "payload matches the filter" }
      : { passed: false, detail: "payload does not match the filter" };
  }
  res.would_trigger = res.webhook_check.passed && res.payload_check.passed && wh.enabled;
  if (!wh.enabled) res.webhook_check.detail += " (webhook is disabled)";
  res.result = res.would_trigger ? "Would trigger" : "Would not trigger";
  return json(res);
};

// ---------- ingress (server paths outside /api/v1) ----------

/** The core has one team (D3): forge and custom webhooks carry no team. */
const INGRESS_TEAM = "default";

async function enqueue(env: Env, teamId: string, partition: string, event: Event, dedupeKey: string) {
  return env.INGRESS.getByName(doName.ingress(teamId, partition)).enqueue(partition, event as never, dedupeKey);
}

interface DeliveryLogRpc {
  recordDelivery(rec: WebhookDelivery): Promise<void>;
  rateLimit(webhookId: string, nowMs: number): Promise<{ ok: boolean; retryMs: number }>;
}

function deliveryLog(env: Env, teamId: string, webhookId: string): DeliveryLogRpc {
  return env.INGRESS.getByName(doName.ingress(teamId, webhookPartition(webhookId))) as unknown as DeliveryLogRpc;
}

/** POST /webhooks/w/{uid} and /webhooks/w/{uid}/{token}. */
export async function serveWebhookIngress(
  request: Request,
  env: Env,
  uid: string,
  token: string | null,
): Promise<Response> {
  const team = env.TEAM.getByName(doName.team(INGRESS_TEAM));
  return handleWebhookDelivery(request, uid, token, {
    async getWebhook(id) {
      const out = unwrapOp<{ webhook: Webhook; team_id: string } | null>(
        await team.api({ requestId: "webhook-ingress" }, "webhooksResolve", { "?id": id }, undefined),
      );
      if (!out) return null;
      return { webhook: out.webhook, teamId: out.team_id };
    },
    getSecret: (teamId, factoryId, name) => teamSecrets(env).get(teamId, factoryId, name),
    enqueue: (teamId, partition, ev, key) => enqueue(env, teamId, partition, ev, key),
    // The delivery log and the shared 60/min limit live in the IngressDO of the webhook.
    record: (teamId, rec) => deliveryLog(env, teamId, rec.webhook_id).recordDelivery(rec),
    rateLimit: (webhookId, nowMs) => deliveryLog(env, INGRESS_TEAM, webhookId).rateLimit(webhookId, nowMs),
  });
}

/** POST /integrations/github/webhook. */
export async function serveGitHubWebhook(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
  const resolver = resolverFor(env);
  const factories = async (teamId: string): Promise<FactoryInfo[]> => {
    const out = await env.TEAM.getByName(doName.team(teamId)).api(
      { requestId: "github-webhook" },
      "forgeFactories",
      {},
      undefined,
    );
    if (isOpError(out)) return [];
    return (out as { items?: FactoryInfo[] } | null)?.items ?? [];
  };
  return handleGitHubWebhook(request, {
    teams: async () => [INGRESS_TEAM],
    integrations: teamIntegrations(env),
    secrets: teamSecrets(env),
    resolver,
    factories: {
      byAlias: async (teamId, alias) =>
        (await factories(teamId)).find((f) => (f.alias ?? "").toLowerCase() === alias.toLowerCase()) ?? null,
      list: factories,
      configChecks: async (teamId, f) => {
        const out = (await env.TEAM.getByName(doName.team(teamId)).api(
          { requestId: "github-webhook" },
          "defsyncConfigChecks",
          {},
          { factoryId: f.id },
        )) as { mode?: string } | null;
        return out?.mode ?? "main";
      },
    },
    publish: async (teamId, events) => {
      for (const ev of events) {
        const partition = "github:" + (ev.source.repo ?? "").toLowerCase();
        await enqueue(env, teamId, partition, ev, ev.delivery_id || ev.id);
      }
    },
    // defSync (server.go OnDefinitionPush / OnDefinitionPR): the TeamDO syncs and checks.
    onDefinitionPush: (teamId, repo, branch, sha) => teamOp(env, teamId, "defsyncPush", { repo, branch, sha }),
    onDefinitionPR: (pr) => teamOp(env, pr.teamId, "defsyncPullRequest", pr),
    waitUntil: (p) => ctx.waitUntil(p),
  });
}

async function teamOp(env: Env, teamId: string, op: string, body: unknown): Promise<void> {
  const out: unknown = await env.TEAM.getByName(doName.team(teamId)).api({ requestId: "github-webhook" }, op, {}, body);
  if (isOpError(out)) console.warn("github webhook:", op, out.$error.code, out.$error.detail);
}

/** Routes a server path to the ingress handlers; null when the path is not one of them. */
export async function routeWebhookIngress(request: Request, env: Env, ctx: ExecutionContext): Promise<Response | null> {
  const url = new URL(request.url);
  if (url.pathname === "/integrations/github/webhook") {
    if (request.method !== "POST")
      return new Response("Method Not Allowed\n", { status: 405, headers: { allow: "POST" } });
    return serveGitHubWebhook(request, env, ctx);
  }
  const m = /^\/webhooks\/w\/([^/]+)(?:\/([^/]+))?$/.exec(url.pathname);
  if (!m) return null;
  if (request.method !== "POST")
    return new Response("Method Not Allowed\n", { status: 405, headers: { allow: "POST" } });
  return serveWebhookIngress(
    request,
    env,
    decodeURIComponent(m[1]!),
    m[2] === undefined ? null : decodeURIComponent(m[2]),
  );
}
