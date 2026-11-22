// Ports of internal/webhooks signature_test.go, webhooks_test.go (handler
// with fake dependencies) and the config rules.
import { beforeEach, describe, expect, it } from "vitest";
import type { Event, Webhook, WebhookDelivery } from "@openfactory/core";
import { utf8 } from "../lib/bytes";
import { sign, verify, signatureSchemes, standardWebhooksKey } from "./signature";
import { generateSecret, ingressURL, redactHeaders, testFilter, validateConfig, validURLToken } from "./config";
import {
  WindowLimiter,
  deliveryIdentity,
  handleWebhookDelivery,
  payloadTitle,
  resetWebhookLimiters,
  type WebhookIngressDeps,
} from "./ingress";

const NOW = Date.parse("2026-01-01T00:00:00Z");
const body = utf8('{"event":"deploy","n":1}');

describe("signature schemes", () => {
  for (const scheme of signatureSchemes()) {
    it(`${scheme} round-trips and rejects tampering`, async () => {
      const secret = utf8(scheme === "standard_webhooks" ? "whsec_" + btoa("0123456789abcdef0123") : "s3cret");
      const h = await sign(scheme, secret, body, NOW, "id1");
      await verify(scheme, secret, h, body, NOW);
      await expect(verify(scheme, secret, h, utf8('{"event":"other"}'), NOW)).rejects.toThrow(
        /signature verification failed/,
      );
      await expect(verify(scheme, utf8("wrong"), h, body, NOW)).rejects.toThrow(/mismatch/);
      await expect(verify(scheme, secret, new Headers(), body, NOW)).rejects.toThrow(/missing/);
    });
  }
  it("timestamps outside the tolerance fail", async () => {
    const h = await sign("stripe", utf8("s"), body, NOW, "");
    await expect(verify("stripe", utf8("s"), h, body, NOW + 6 * 60_000)).rejects.toThrow(/older than 5m0s/);
    await expect(verify("stripe", utf8("s"), h, body, NOW - 6 * 60_000)).rejects.toThrow(/in the future/);
  });
  it("Standard Webhooks reference vector", async () => {
    const h = new Headers({
      "webhook-id": "msg_p5jXN8AQM9LWM0D4loKWxJek",
      "webhook-timestamp": "1614265330",
      "webhook-signature": "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=",
    });
    await verify(
      "standard_webhooks",
      utf8("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"),
      h,
      utf8('{"test": 2432232314}'),
      1614265330_000,
    );
    expect(standardWebhooksKey(utf8("not base64!")).length).toBe(11);
  });
  it("PagerDuty accepts any of several signatures", async () => {
    const h = await sign("pagerduty", utf8("s"), body, NOW, "");
    const v = h.get("X-PagerDuty-Signature")!;
    await verify("pagerduty", utf8("s"), new Headers({ "X-PagerDuty-Signature": "v1=00ff," + v }), body, NOW);
    await expect(verify("nope", utf8("s"), h, body, NOW)).rejects.toThrow(/unknown signature scheme/);
    await expect(verify("github", new Uint8Array(), h, body, NOW)).rejects.toThrow(/no signing secret/);
  });
});

describe("config", () => {
  it("validates", () => {
    validateConfig({ auth_mode: "token", secret_name: "S" });
    expect(() => validateConfig({ auth_mode: "token", signature_scheme: "github", secret_name: "S" })).toThrow(
      /only valid with authMode signature/,
    );
    expect(() => validateConfig({ auth_mode: "signature", secret_name: "S" })).toThrow(
      /required for authMode signature/,
    );
    expect(() => validateConfig({ auth_mode: "signature", signature_scheme: "x", secret_name: "" })).toThrow(
      /unknown signatureScheme "x".*secretName is required/,
    );
    expect(() => validateConfig({ auth_mode: "bogus", secret_name: "S" })).toThrow(/unknown authMode/);
    expect(() => validateConfig({ auth_mode: "", secret_name: "S", delivery_id_header: "X-Api-Token" })).toThrow(
      /carries credentials/,
    );
    expect(() => validateConfig({ auth_mode: "", secret_name: "S", delivery_id_header: "bad header" })).toThrow(
      /not a valid HTTP header/,
    );
  });
  it("secrets, URLs and redaction", () => {
    expect(generateSecret("token", "")).toMatch(/^whsec_[A-Za-z0-9_-]{43}$/);
    expect(generateSecret("signature", "standard_webhooks")).toMatch(/^whsec_[A-Za-z0-9+/]{32}$/);
    expect(() => generateSecret("signature", "stripe")).toThrow(/issues its own/);
    expect(ingressURL("https://cp.test/", { id: "whk_1", auth_mode: "url_token" })).toBe(
      "https://cp.test/webhooks/w/whk_1/{token}",
    );
    expect(validURLToken("a.b~c_d-1")).toBe(true);
    expect(validURLToken("a/b")).toBe(false);
    expect(
      redactHeaders(
        new Headers({ authorization: "x", "x-hub-signature-256": "y", "x-request-id": "r", "my-secret-thing": "z" }),
      ),
    ).toEqual({ "X-Request-Id": "r" });
  });
  it("tests a filter against a delivery", () => {
    const d: Pick<WebhookDelivery, "webhook_id" | "status" | "body"> = {
      webhook_id: "whk_1",
      status: "accepted",
      body: { env: "prod" },
    };
    expect(testFilter({ webhook_ids: ["whk_1"], payload: { env: ["prod"] } }, d)).toEqual({
      would_trigger: true,
      webhook_matched: true,
      payload_matched: true,
    });
    const r = testFilter({ webhook_ids: ["whk_2"], payload: { env: ["dev"] } }, d);
    expect(r.would_trigger).toBe(false);
    expect(r.reason).toBe("the delivery came from a different webhook; the payload does not match the filter");
    expect(testFilter({ webhook_ids: ["whk_1"] }, { ...d, status: "rejected" }).reason).toMatch(/not accepted/);
    expect(() => testFilter({ payload: {} }, d)).toThrow(/webhook_ids" is required/);
  });
});

describe("delivery identity and title", () => {
  it("prefers the configured header, the scheme header, X-OF-Delivery-Id, then the body hash", async () => {
    const wh: Pick<Webhook, "delivery_id_header" | "auth_mode" | "signature_scheme"> = {
      delivery_id_header: "X-Id",
      auth_mode: "signature",
      signature_scheme: "github",
    };
    expect(await deliveryIdentity(wh, new Headers({ "x-id": " a ", "x-github-delivery": "b" }), body)).toBe("a");
    expect(await deliveryIdentity(wh, new Headers({ "x-github-delivery": "b", "x-of-delivery-id": "c" }), body)).toBe(
      "b",
    );
    expect(
      await deliveryIdentity(
        { auth_mode: "token" },
        new Headers({ "x-github-delivery": "b", "x-of-delivery-id": "c" }),
        body,
      ),
    ).toBe("c");
    expect(await deliveryIdentity({ auth_mode: "token" }, new Headers(), body)).toMatch(/^sha256:[0-9a-f]{64}$/);
  });
  it("titles", () => {
    expect(payloadTitle('{"subject":"  Disk full\\nmore"}', "def")).toBe("Disk full");
    expect(payloadTitle('{"title":""}', "def")).toBe("def");
    expect(payloadTitle("[1]", "def")).toBe("def");
    expect(payloadTitle(JSON.stringify({ title: "é".repeat(150) }), "d").length).toBe(100);
  });
  it("window limiter", () => {
    const l = new WindowLimiter(2, 60_000);
    expect(l.allow("w", 0).ok).toBe(true);
    expect(l.allow("w", 1).ok).toBe(true);
    expect(l.allow("w", 2)).toEqual({ ok: false, retryMs: 59_998 });
    expect(l.allow("w", 60_000).ok).toBe(true);
  });
});

describe("ingress handler", () => {
  const wh: Webhook = {
    id: "whk_1",
    factory_id: "fac_1",
    name: "deploys",
    auth_mode: "token",
    secret_name: "S",
    enabled: true,
    created_at: "",
  };
  let enqueued: { partition: string; event: Event; key: string }[];
  let seen: Set<string>;
  const deps = (w: Partial<Webhook> = {}, over: Partial<WebhookIngressDeps> = {}): WebhookIngressDeps => ({
    getWebhook: async (id) => (id === wh.id ? { webhook: { ...wh, ...w }, teamId: "team_1" } : null),
    getSecret: async (_t, _f, n) => (n === "S" ? "tok_secret" : undefined),
    enqueue: async (_t, partition, event, key) => {
      enqueued.push({ partition, event, key });
      const dup = seen.has(key);
      seen.add(key);
      return { accepted: !dup, deduplicated: dup };
    },
    now: () => NOW,
    ...over,
  });
  const post = (headers: Record<string, string>, b = '{"title":"Deploy done"}') =>
    new Request("https://cp.test/webhooks/w/whk_1", { method: "POST", body: b, headers });
  beforeEach(() => {
    enqueued = [];
    seen = new Set();
    resetWebhookLimiters();
  });

  it("accepts a bearer token delivery and dedupes a retry", async () => {
    const r = await handleWebhookDelivery(
      post({ authorization: "Bearer tok_secret", "x-of-delivery-id": "d1" }),
      "whk_1",
      null,
      deps(),
    );
    expect(r.status).toBe(202);
    expect(await r.json()).toEqual({ delivery_id: "d1", status: "accepted" });
    expect(enqueued[0]!.partition).toBe("webhook:whk_1");
    expect(enqueued[0]!.event).toMatchObject({
      team_id: "team_1",
      factory_id: "fac_1",
      provider: "webhook",
      type: "received",
      action_key: "webhook:whk_1:d1",
      source: { kind: "delivery", title: "Deploy done" },
      attributes: { webhook_id: "whk_1", webhook_name: "deploys" },
      payload: { title: "Deploy done" },
    });
    const r2 = await handleWebhookDelivery(
      post({ authorization: "Bearer tok_secret", "x-of-delivery-id": "d1" }),
      "whk_1",
      null,
      deps(),
    );
    expect(await r2.json()).toEqual({ delivery_id: "d1", status: "duplicate" });
  });
  it("rejects", async () => {
    expect((await handleWebhookDelivery(post({}), "nope", null, deps())).status).toBe(404);
    expect(
      (
        await handleWebhookDelivery(
          post({ authorization: "Bearer tok_secret" }),
          "whk_1",
          null,
          deps({ enabled: false }),
        )
      ).status,
    ).toBe(404);
    expect((await handleWebhookDelivery(post({ authorization: "Bearer bad" }), "whk_1", null, deps())).status).toBe(
      401,
    );
    expect(
      (await handleWebhookDelivery(post({ authorization: "Bearer tok_secret" }, "{not json"), "whk_1", null, deps()))
        .status,
    ).toBe(400);
    expect(
      (
        await handleWebhookDelivery(
          post({ authorization: "Bearer tok_secret" }, "x".repeat(300_000)),
          "whk_1",
          null,
          deps(),
        )
      ).status,
    ).toBe(413);
    // The URL token form is only valid for url_token webhooks.
    expect((await handleWebhookDelivery(post({}), "whk_1", "tok_secret", deps())).status).toBe(401);
    expect(
      (await handleWebhookDelivery(post({}), "whk_1", "tok_secret", deps({ auth_mode: "url_token" }))).status,
    ).toBe(202);
    const se = await handleWebhookDelivery(
      post({ authorization: "Bearer x" }),
      "whk_1",
      null,
      deps({}, { getSecret: async () => Promise.reject(new Error("down")) }),
    );
    expect(se.status).toBe(500);
  });
  it("limits failed authentications and accepted deliveries", async () => {
    for (let i = 0; i < 60; i++)
      await handleWebhookDelivery(post({ authorization: "Bearer bad" }), "whk_1", null, deps());
    const r = await handleWebhookDelivery(post({ authorization: "Bearer tok_secret" }), "whk_1", null, deps());
    expect(r.status).toBe(429);
    expect(r.headers.get("retry-after")).toBe("60");
    resetWebhookLimiters();
    const limited = await handleWebhookDelivery(
      post({ authorization: "Bearer tok_secret" }),
      "whk_1",
      null,
      deps({}, { rateLimit: async () => ({ ok: false, retryMs: 1500 }) }),
    );
    expect(limited.status).toBe(429);
    expect(limited.headers.get("retry-after")).toBe("2");
  });
  it("verifies provider signatures", async () => {
    const h = await sign("github", utf8("tok_secret"), utf8('{"a":1}'), NOW, "gh-1");
    const hdrs: Record<string, string> = {};
    h.forEach((v, k) => (hdrs[k] = v));
    const r = await handleWebhookDelivery(
      post(hdrs, '{"a":1}'),
      "whk_1",
      null,
      deps({ auth_mode: "signature", signature_scheme: "github" }),
    );
    expect(await r.json()).toEqual({ delivery_id: "gh-1", status: "accepted" });
  });
});
