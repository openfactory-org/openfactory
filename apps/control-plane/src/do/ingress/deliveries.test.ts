// The webhook delivery log and the shared accepted-delivery limit of IngressDO
// (webhooks.go handleListDeliveries / handleGetDelivery / test-filter lookup).
import { env } from "cloudflare:workers";
import { doName, newToken, type WebhookDelivery } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import { deliveryView, WEBHOOK_RATE_LIMIT, type IngressDO } from "./index";

function stub(webhookId: string) {
  return env.INGRESS.getByName(
    doName.ingress("default", "webhook:" + webhookId),
  ) as unknown as DurableObjectStub<IngressDO>;
}

const at = (s: number) => new Date(Date.UTC(2026, 10, 22, 12, 0, s)).toISOString();

describe("IngressDO delivery log", () => {
  it("lists newest first without bodies, gets one with its body, finds the latest accepted", async () => {
    const wh = "whk_" + newToken("w").slice(0, 10);
    const s = stub(wh);
    const recs: WebhookDelivery[] = [
      { id: "whd_1", webhook_id: wh, status: "accepted", body: { n: 1 }, received_at: at(1) },
      { id: "whd_2", webhook_id: wh, status: "rejected", reason: "rate_limited", received_at: at(2) },
      { id: "whd_3", webhook_id: wh, status: "deduplicated", reason: "duplicate", received_at: at(3) },
    ];
    for (const r of recs) await s.recordDelivery(r);
    const list = (await s.deliveries({}, "listDeliveries", { id: wh })) as { items: Record<string, unknown>[] };
    expect(list.items.map((d) => d.id)).toEqual(["whd_3", "whd_2", "whd_1"]);
    expect(list.items.map((d) => d.label)).toEqual(["Duplicate", "Rejected (rate limited)", "Accepted"]);
    expect(list.items.every((d) => !("body" in d))).toBe(true);
    const one = (await s.deliveries({}, "getDelivery", { id: wh, deliveryId: "whd_1" })) as Record<string, unknown>;
    expect(one.body).toEqual({ n: 1 });
    expect(one.size).toBe(7);
    const latest = (await s.deliveries({}, "latestAcceptedDelivery", { id: wh })) as { id: string };
    expect(latest.id).toBe("whd_1");
    expect(await s.deliveries({}, "getDelivery", { id: "whk_other", deliveryId: "whd_1" })).toEqual({
      $error: { code: "resource_not_found", detail: "delivery whd_1 not found" },
    });
  });

  it("latestAcceptedDelivery is null without an accepted delivery", async () => {
    const wh = "whk_" + newToken("w").slice(0, 10);
    expect(await stub(wh).deliveries({}, "latestAcceptedDelivery", { id: wh })).toBeNull();
  });

  it("allows 60 accepted deliveries per minute, then asks the sender to wait", async () => {
    const wh = "whk_" + newToken("w").slice(0, 10);
    const s = stub(wh);
    const t0 = 1_000_000;
    for (let i = 0; i < WEBHOOK_RATE_LIMIT; i++) expect((await s.rateLimit(wh, t0 + i)).ok).toBe(true);
    const over = await s.rateLimit(wh, t0 + 100);
    expect(over.ok).toBe(false);
    expect(over.retryMs).toBe(60_000 - 100);
    expect((await s.rateLimit(wh, t0 + 60_000)).ok).toBe(true);
  });
});

describe("deliveryView", () => {
  it("labels rejections by reason", () => {
    const base: WebhookDelivery = { id: "d", webhook_id: "w", status: "rejected", received_at: at(0) };
    expect(deliveryView({ ...base, reason: "invalid_json" }, false).label).toBe("Rejected (invalid JSON)");
    expect(deliveryView({ ...base, reason: "too_large" }, false).label).toBe("Rejected (too large)");
    expect(deliveryView({ ...base, reason: "auth_failed" }, false).label).toBe("Rejected (auth)");
  });
});
