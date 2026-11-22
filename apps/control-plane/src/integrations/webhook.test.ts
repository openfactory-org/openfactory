import { describe, expect, it } from "vitest";
import type { Integration } from "@openfactory/core";
import type { IntakeResult } from "./select";
import { post } from "./testkit";
import type { Logger } from "./types";
import { Dedupe, MAX_WEBHOOK_BODY } from "./util";
import { acceptDelivery, handleIntegrationWebhook, type WebhookPipeline } from "./webhook";

const rec = { id: "int_1", team_id: "team_1", provider: "linear" } as Integration;
const quiet: Logger = { debug() {}, info() {}, warn() {}, error() {} };

function pipeline(over: Partial<WebhookPipeline<{ ok?: boolean }>> = {}): WebhookPipeline<{ ok?: boolean }> {
  return {
    parse: (b) => JSON.parse(b) as { ok?: boolean },
    invalidPayload: "not JSON",
    select: async () => ({ integration: rec, error: null }),
    verify: async (_r, _b, p) => p.ok === true,
    unauthorized: { code: "invalid_signature", message: "bad signature" },
    accept: () => ({ response: new Response("done", { status: 202 }) }),
    ...over,
  };
}

async function errorOf(res: Response): Promise<{ status: number; code?: string; message?: string }> {
  const text = await res.text();
  try {
    const j = JSON.parse(text) as { error?: { code: string; message: string } };
    return { status: res.status, code: j.error?.code, message: j.error?.message };
  } catch {
    return { status: res.status };
  }
}

describe("handleIntegrationWebhook", () => {
  it.each([
    ["a body over the cap", "x".repeat(MAX_WEBHOOK_BODY + 1), 413, "payload_too_large"],
    ["a body that does not parse", "{", 400, "invalid_payload"],
    ["a request that fails verification", '{"ok":false}', 401, "invalid_signature"],
  ])("rejects %s", async (_name, body, status, code) => {
    const out = await handleIntegrationWebhook(post("https://of.test/hook", body), pipeline());
    expect(await errorOf(out.response)).toMatchObject({ status, code });
  });

  it("passes the payload, the selected record and the body to accept", async () => {
    let seen: { found?: IntakeResult; body?: string } = {};
    const out = await handleIntegrationWebhook(
      post("https://of.test/hook", '{"ok":true}'),
      pipeline({
        accept: (_p, found, body) => {
          seen = { found, body };
          return { response: new Response(null, { status: 204 }) };
        },
      }),
    );
    expect(out.response.status).toBe(204);
    expect(seen.found?.integration?.id).toBe("int_1");
    expect(seen.body).toBe('{"ok":true}');
  });

  it("selects no record without select", async () => {
    let found: IntakeResult | undefined;
    await handleIntegrationWebhook(
      post("https://of.test/hook", '{"ok":true}'),
      pipeline({
        select: undefined,
        verify: async (_r, _b, _p, f) => ((found = f), true),
      }),
    );
    expect(found).toEqual({ integration: null, error: null });
  });
});

describe("acceptDelivery", () => {
  it("answers ignored, accepted and duplicate", async () => {
    const dedupe = new Dedupe(60_000);
    const ran: string[] = [];
    const opts = (delivery: string) => ({
      log: quiet,
      logFields: {},
      dedupe,
      delivery,
      background: async (i: Integration) => void ran.push(i.id),
    });

    const ignored = acceptDelivery({ integration: null, error: new Error("none") }, opts("d1"));
    expect(ignored.response.status).toBe(202);
    expect(await ignored.response.json()).toEqual({ status: "ignored", reason: "no matching integration" });
    expect(ignored.background).toBeUndefined();

    const first = acceptDelivery({ integration: rec, error: null }, opts("d1"));
    expect(first.response.status).toBe(202);
    expect(await first.response.json()).toEqual({ status: "accepted" });
    await first.background!();
    expect(ran).toEqual(["int_1"]);

    const again = acceptDelivery({ integration: rec, error: null }, opts("d1"));
    expect(again.response.status).toBe(200);
    expect(await again.response.json()).toEqual({ status: "duplicate" });
  });
});
