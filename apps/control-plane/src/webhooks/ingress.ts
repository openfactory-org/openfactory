// Custom webhook ingress (POST /webhooks/w/{uid} and /webhooks/w/{uid}/{token}).
// Port of internal/webhooks/webhooks.go. The Worker authenticates the
// delivery and hands the event to IngressDO (partition "webhook:<id>"), which
// dedupes exactly and delivers in order. The failed-authentication budget is
// counted per isolate, as Go counted it per process.
import { clipBytes, newId, type Event, type Webhook, type WebhookDelivery } from "@openfactory/core";
import { constantTimeEqual, sha256Hex, utf8 } from "../lib/bytes";
import {
  AUTH_SIGNATURE,
  AUTH_TOKEN,
  AUTH_URL_TOKEN,
  DEFAULT_AUTH_FAILURE_LIMIT,
  DEFAULT_MAX_BODY_BYTES,
  DEFAULT_RATE_LIMIT,
  REASON_AUTH,
  REASON_INVALID_JSON,
  REASON_RATE_LIMITED,
  REASON_SECRET,
  REASON_TOO_LARGE,
  SENDER_DELIVERY_ID_HEADER,
  STATUS_ACCEPTED,
  STATUS_DEDUPLICATED,
  STATUS_REJECTED,
  redactHeaders,
} from "./config";
import { deliveryHeaders, verify } from "./signature";

/** A fixed one-minute window counter per key. */
export class WindowLimiter {
  private readonly m = new Map<string, { start: number; count: number }>();
  constructor(
    readonly limit: number,
    readonly windowMs = 60_000,
  ) {}

  private get(key: string, now: number) {
    let w = this.m.get(key);
    if (!w || now - w.start >= this.windowMs || now < w.start) {
      if (this.m.size > 10_000) for (const [k, x] of this.m) if (now - x.start >= this.windowMs) this.m.delete(k);
      w = { start: now, count: 0 };
      this.m.set(key, w);
    }
    return w;
  }

  /** Consumes one unit; false and the wait (ms) when over the limit. */
  allow(key: string, now: number): { ok: boolean; retryMs: number } {
    if (this.limit <= 0) return { ok: true, retryMs: 0 };
    const w = this.get(key, now);
    if (w.count >= this.limit) return { ok: false, retryMs: w.start + this.windowMs - now };
    w.count++;
    return { ok: true, retryMs: 0 };
  }

  /** Over the limit, without consuming. */
  exhausted(key: string, now: number): { over: boolean; retryMs: number } {
    if (this.limit <= 0) return { over: false, retryMs: 0 };
    const w = this.get(key, now);
    return w.count >= this.limit ? { over: true, retryMs: w.start + this.windowMs - now } : { over: false, retryMs: 0 };
  }

  add(key: string, now: number): void {
    if (this.limit > 0) this.get(key, now).count++;
  }
}

/** Action key of a custom webhook delivery. */
export function actionKey(webhookId: string, identity: string): string {
  return "webhook:" + webhookId + ":" + identity;
}

/** IngressDO partition of a custom webhook. */
export function webhookPartition(webhookId: string): string {
  return "webhook:" + webhookId;
}

/**
 * The dedupe identity of a delivery: the configured delivery ID header, the
 * scheme's delivery header, X-OF-Delivery-Id, else "sha256:<hex of body>".
 */
export async function deliveryIdentity(
  wh: Pick<Webhook, "delivery_id_header" | "auth_mode" | "signature_scheme">,
  h: Headers,
  body: Uint8Array,
): Promise<string> {
  const clip = (s: string | null) => (s ?? "").trim().slice(0, 200);
  if (wh.delivery_id_header) {
    const v = clip(h.get(wh.delivery_id_header));
    if (v) return v;
  }
  if (wh.auth_mode === AUTH_SIGNATURE) {
    for (const n of deliveryHeaders(wh.signature_scheme ?? "")) {
      const v = clip(h.get(n));
      if (v) return v;
    }
  }
  const v = clip(h.get(SENDER_DELIVERY_ID_HEADER));
  if (v) return v;
  return "sha256:" + (await sha256Hex(body));
}

/** The first non-empty title, subject or summary of a JSON object body (first line, max 200 bytes), else def. */
export function payloadTitle(body: string, def: string): string {
  let obj: unknown;
  try {
    obj = JSON.parse(body);
  } catch {
    return def;
  }
  if (typeof obj !== "object" || obj === null || Array.isArray(obj)) return def;
  for (const k of ["title", "subject", "summary"]) {
    const v = (obj as Record<string, unknown>)[k];
    if (typeof v !== "string") continue;
    const s = v.trim().split("\n")[0]!.trim();
    if (s === "") continue;
    return clipBytes(s, 200);
  }
  return def;
}

class SecretUnavailable extends Error {}

export interface WebhookIngressDeps {
  /** The webhook and the team of its factory, or null. */
  getWebhook(uid: string): Promise<{ webhook: Webhook; teamId: string } | null>;
  /** The secret value; undefined when it does not exist; throws when unavailable. */
  getSecret(teamId: string, factoryId: string, name: string): Promise<string | undefined>;
  /** Hands the event to IngressDO; returns whether it was a duplicate. */
  enqueue(
    teamId: string,
    partition: string,
    event: Event,
    dedupeKey: string,
  ): Promise<{ accepted: boolean; deduplicated: boolean }>;
  /** Writes a delivery log entry (IngressDO). Optional: without it entries are logged only. */
  record?(teamId: string, rec: WebhookDelivery): Promise<void>;
  /** Shared accepted-delivery limit (IngressDO). Without it, an isolate limiter applies. */
  rateLimit?(webhookId: string, nowMs: number): Promise<{ ok: boolean; retryMs: number }>;
  now?: () => number;
  maxBodyBytes?: number;
  signatureToleranceMs?: number;
}

const authFailures = new WindowLimiter(DEFAULT_AUTH_FAILURE_LIMIT);
const localRate = new WindowLimiter(DEFAULT_RATE_LIMIT);

/** Resets the isolate limiters (tests). */
export function resetWebhookLimiters(): void {
  (authFailures as unknown as { m: Map<string, unknown> }).m.clear();
  (localRate as unknown as { m: Map<string, unknown> }).m.clear();
}

function errJSON(status: number, code: string, message: string, headers?: Record<string, string>): Response {
  return new Response(JSON.stringify({ error: { code, message } }) + "\n", {
    status,
    headers: { "content-type": "application/json", ...(headers ?? {}) },
  });
}

function retryAfter(retryMs: number, msg: string): Response {
  return errJSON(429, "rate_limited", msg, { "retry-after": String(Math.max(1, Math.ceil(retryMs / 1000))) });
}

/** Serves one custom webhook delivery. urlToken is set for the /webhooks/w/{uid}/{token} form. */
export async function handleWebhookDelivery(
  req: Request,
  uid: string,
  urlToken: string | null,
  deps: WebhookIngressDeps,
): Promise<Response> {
  const viaURLToken = urlToken !== null;
  let found: { webhook: Webhook; teamId: string } | null;
  try {
    found = await deps.getWebhook(uid);
  } catch (e) {
    console.error("webhook lookup failed", uid, e);
    return errJSON(500, "internal", "webhook lookup failed");
  }
  if (!found || !found.webhook.enabled) return errJSON(404, "not_found", "webhook not found");
  const { webhook: wh, teamId } = found;
  const now = (deps.now ?? Date.now)();
  const rec: Omit<WebhookDelivery, "status"> = {
    id: newId("whd"),
    webhook_id: wh.id,
    headers: redactHeaders(req.headers),
    received_at: new Date(now).toISOString(),
  };
  const reject = async (reason: string) => {
    const r: WebhookDelivery = { ...rec, status: STATUS_REJECTED, reason };
    if (deps.record)
      await deps.record(teamId, r).catch((e) => console.warn("recording rejected delivery failed", wh.id, e));
    else console.info("webhook delivery rejected", wh.id, reason);
  };
  const ex = authFailures.exhausted(wh.id, now);
  if (ex.over) return retryAfter(ex.retryMs, "too many failed authentication attempts");
  const max = deps.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES;
  let body: Uint8Array;
  try {
    if (Number(req.headers.get("content-length") ?? "0") > max) throw new RangeError();
    body = new Uint8Array(await req.arrayBuffer());
    if (body.length > max) throw new RangeError();
  } catch (e) {
    if (e instanceof RangeError) {
      await reject(REASON_TOO_LARGE);
      return errJSON(413, "too_large", `request body exceeds ${Math.floor(max / 1024)} KB`);
    }
    return errJSON(400, "bad_request", "could not read request body");
  }
  try {
    await authenticate(deps, teamId, wh, req.headers, body, urlToken ?? "", viaURLToken, now);
  } catch (e) {
    if (e instanceof SecretUnavailable) {
      console.error("webhook secret unavailable", wh.id, wh.secret_name, e.message);
      await reject(REASON_SECRET);
      return errJSON(500, "internal", "webhook secret unavailable");
    }
    authFailures.add(wh.id, now);
    console.info("webhook delivery rejected: authentication failed", wh.id, (e as Error).message);
    await reject(REASON_AUTH);
    return errJSON(401, "unauthorized", "authentication failed");
  }
  const rl = deps.rateLimit ? await deps.rateLimit(wh.id, now) : localRate.allow(wh.id, now);
  if (!rl.ok) {
    await reject(REASON_RATE_LIMITED);
    return retryAfter(rl.retryMs, `webhook exceeded ${DEFAULT_RATE_LIMIT} deliveries per minute`);
  }
  const text = new TextDecoder().decode(body);
  let payload: unknown;
  try {
    payload = JSON.parse(text);
  } catch {
    await reject(REASON_INVALID_JSON);
    return errJSON(400, "invalid_json", "request body is not valid JSON");
  }
  const identity = await deliveryIdentity(wh, req.headers, body);
  const ev: Event = {
    id: newId("evt"),
    team_id: teamId,
    factory_id: wh.factory_id,
    provider: "webhook",
    type: "received",
    delivery_id: identity,
    action_key: actionKey(wh.id, identity),
    actor: { provider: "webhook", external_id: wh.id, handle: wh.name, is_bot: false },
    source: { provider: "webhook", kind: "delivery", external_id: identity, title: payloadTitle(text, wh.name) },
    attributes: { webhook_id: wh.id, webhook_name: wh.name, delivery_record_id: rec.id },
    payload,
    received_at: new Date(now).toISOString(),
  };
  let res: { accepted: boolean; deduplicated: boolean };
  try {
    res = await deps.enqueue(teamId, webhookPartition(wh.id), ev, actionKey(wh.id, identity));
  } catch (e) {
    console.error("recording delivery failed", wh.id, e);
    return errJSON(500, "internal", "could not record delivery");
  }
  const status = res.deduplicated ? STATUS_DEDUPLICATED : STATUS_ACCEPTED;
  if (deps.record) {
    const r: WebhookDelivery = {
      ...rec,
      delivery_id: identity,
      status,
      ...(res.deduplicated ? { reason: "duplicate" } : { body: payload }),
    };
    await deps.record(teamId, r).catch((e) => console.warn("recording delivery failed", wh.id, e));
  }
  return new Response(
    JSON.stringify({ delivery_id: identity, status: res.deduplicated ? "duplicate" : "accepted" }) + "\n",
    {
      status: 202,
      headers: { "content-type": "application/json" },
    },
  );
}

async function authenticate(
  deps: WebhookIngressDeps,
  teamId: string,
  wh: Webhook,
  h: Headers,
  body: Uint8Array,
  urlToken: string,
  viaURLToken: boolean,
  now: number,
): Promise<void> {
  const mode = wh.auth_mode || AUTH_TOKEN;
  if (viaURLToken !== (mode === AUTH_URL_TOKEN)) throw new Error("wrong ingress URL form for auth mode " + mode);
  let s: string | undefined;
  try {
    s = await deps.getSecret(teamId, wh.factory_id, wh.secret_name);
  } catch (e) {
    throw new SecretUnavailable((e as Error).message);
  }
  if (s === undefined) throw new Error("webhook secret not found");
  if (s === "") throw new Error("webhook secret is empty");
  const secret = utf8(s);
  switch (mode) {
    case AUTH_TOKEN: {
      const v = (h.get("authorization") ?? "").trim();
      if (v.length <= 7 || v.slice(0, 7).toLowerCase() !== "bearer ") throw new Error("missing bearer token");
      if (!constantTimeEqual(utf8(v.slice(7).trim()), secret)) throw new Error("bearer token mismatch");
      return;
    }
    case AUTH_URL_TOKEN:
      if (!constantTimeEqual(utf8(urlToken), secret)) throw new Error("URL token mismatch");
      return;
    case AUTH_SIGNATURE:
      await verify(wh.signature_scheme ?? "", secret, h, body, now, deps.signatureToleranceMs ?? 0);
      return;
  }
  throw new Error("unknown auth mode " + mode);
}
