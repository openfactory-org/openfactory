import { describe, expect, it } from "vitest";
import { PLUGIN_API_VERSION, type HostApi, type PluginEvent, type WebhookResult } from "@openfactory/plugin-api";
import { FakeHostApi } from "@openfactory/plugin-api/testing";
import { forwardPluginWebhook, pluginWebhookName, toDeliveries, webhookResponse, type PluginDelivery } from "./ingress";
import { eventDedupeKey, eventToCore } from "./mappers";
import { loadPlugins, type PluginStub } from "./registry";

const ev = (over: Partial<PluginEvent> = {}): PluginEvent => ({
  id: "evt_1",
  teamId: "team_1",
  provider: "gitlab",
  type: "push",
  deliveryId: "d1:push",
  actor: { provider: "gitlab", externalId: "2", handle: "alice", isBot: false },
  source: { provider: "gitlab", kind: "push", repo: "acme/api", threadRef: "d1" },
  attributes: { branch: "main" },
  payload: { x: 1 },
  text: "hello",
  receivedAt: "2026-01-02T03:04:05.000Z",
  actionKey: "gitlab:d1",
  ...over,
});

async function registryWith(
  handle: PluginStub["handleWebhook"],
  roles: Array<"forge" | "ingress"> = ["forge", "ingress"],
) {
  const stub = {
    info: async () => ({ name: "gitlab", version: "1", apiVersion: PLUGIN_API_VERSION, roles }),
    ping: async () => {},
    handleWebhook: handle,
  } as PluginStub;
  return loadPlugins({ PLUGIN_GITLAB: stub }, { log: () => {} });
}

const post = (path = "/plugins/gitlab/webhook") =>
  new Request("https://cp.test" + path, { method: "POST", body: "{}" });

function opts(
  registry: Awaited<ReturnType<typeof registryWith>>,
  deliver: (d: PluginDelivery[]) => Promise<void> = async () => {},
) {
  return { registry, host: () => new FakeHostApi() as HostApi, deliver, log: () => {} };
}

describe("pluginWebhookName", () => {
  it.each([
    ["/plugins/gitlab/webhook", "gitlab"],
    ["/plugins/azure-devops_2/webhook/", "azure-devops_2"],
    ["/plugins/gitlab", undefined],
    ["/plugins/Git Lab/webhook", undefined],
    ["/plugins/gitlab/webhook/x", undefined],
  ])("%s", (path, want) => {
    expect(pluginWebhookName(path)).toBe(want);
  });
});

describe("forwardPluginWebhook", () => {
  it("returns the plugin answer and delivers the events", async () => {
    const delivered: PluginDelivery[][] = [];
    const reg = await registryWith(async (_req, host) => {
      expect(host).toBeDefined();
      return {
        status: 202,
        headers: { "content-type": "application/json", "x-evil": "1" },
        body: '{"status":"accepted"}',
        events: [ev()],
      };
    });
    const res = await forwardPluginWebhook(
      "gitlab",
      post(),
      opts(reg, async (d) => void delivered.push(d)),
    );
    expect(res.status).toBe(202);
    expect(await res.text()).toBe('{"status":"accepted"}');
    expect(res.headers.get("x-evil")).toBeNull();
    expect(res.headers.get("content-type")).toBe("application/json");
    expect(delivered).toHaveLength(1);
    expect(delivered[0]![0]).toMatchObject({
      dedupeKey: "gitlab:d1:push",
      event: {
        id: "evt_1",
        team_id: "team_1",
        provider: "gitlab",
        type: "push",
        delivery_id: "d1:push",
        action_key: "gitlab:d1",
      },
    });
  });

  it("answers the sender without delivering when there are no events", async () => {
    let called = false;
    const reg = await registryWith(async () => ({ status: 401, body: "no", events: [] }));
    const res = await forwardPluginWebhook(
      "gitlab",
      post(),
      opts(reg, async () => void (called = true)),
    );
    expect(res.status).toBe(401);
    expect(called).toBe(false);
  });

  it("answers 404 for an unknown plugin or one without the ingress role, and 400 for GET", async () => {
    const reg = await registryWith(async () => ({ status: 200, events: [] }));
    expect((await forwardPluginWebhook("nope", post(), opts(reg))).status).toBe(404);
    const noIngress = await registryWith(async () => ({ status: 200, events: [] }), ["forge"]);
    expect((await forwardPluginWebhook("gitlab", post(), opts(noIngress))).status).toBe(404);
    expect((await forwardPluginWebhook("gitlab", new Request("https://cp.test/x"), opts(reg))).status).toBe(400);
  });

  it("answers 503 when the plugin throws an unknown error and keeps a known code", async () => {
    const boom = await registryWith(async () => {
      throw new Error("kaboom");
    });
    const r1 = await forwardPluginWebhook("gitlab", post(), opts(boom));
    expect(r1.status).toBe(503);
    expect(await r1.text()).not.toContain("kaboom");
    const known = await registryWith(async () => {
      throw new Error("not_authorized: nope");
    });
    expect((await forwardPluginWebhook("gitlab", post(), opts(known))).status).toBe(403);
  });

  it("answers 503 when the events cannot be stored, so the sender retries", async () => {
    const reg = await registryWith(async () => ({ status: 202, events: [ev()] }));
    const res = await forwardPluginWebhook(
      "gitlab",
      post(),
      opts(reg, async () => Promise.reject(new Error("do down"))),
    );
    expect(res.status).toBe(503);
  });
});

describe("webhookResponse", () => {
  it("keeps allowed headers, repairs a bad status and omits a body for 204", async () => {
    const r = webhookResponse({
      status: 401,
      headers: { "WWW-Authenticate": 'Basic realm="x"', "Set-Cookie": "a=b" },
      body: "x",
    });
    expect(r.headers.get("www-authenticate")).toBe('Basic realm="x"');
    expect(r.headers.get("set-cookie")).toBeNull();
    expect(webhookResponse({ status: 99 } as WebhookResult).status).toBe(502);
    expect(webhookResponse({ status: 204, body: "x" }).body).toBeNull();
    expect(webhookResponse({ status: 200 }).status).toBe(200);
  });
});

describe("events", () => {
  it("drops an event without a team, provider or type and gives ids to events without one", () => {
    const logs: string[] = [];
    const out = toDeliveries(
      [ev({ teamId: "" }), ev({ type: "" }), ev({ id: "", deliveryId: "" })],
      "gitlab",
      undefined,
      (m) => logs.push(m),
    );
    expect(out).toHaveLength(1);
    expect(out[0]!.event.id).toMatch(/^evt_/);
    expect(out[0]!.dedupeKey).toBe(out[0]!.event.id);
    expect(logs).toHaveLength(2);
  });

  it("uses the delivery as the dedupe key", () => {
    expect(eventDedupeKey(ev())).toBe("gitlab:d1:push");
    expect(eventDedupeKey(ev({ deliveryId: undefined }))).toBe("evt_1");
  });

  it("converts a plugin event to the core event", () => {
    const c = eventToCore(ev({ factoryId: "fac_1" }), "evt_1");
    expect(c).toEqual({
      id: "evt_1",
      team_id: "team_1",
      factory_id: "fac_1",
      provider: "gitlab",
      type: "push",
      delivery_id: "d1:push",
      action_key: "gitlab:d1",
      actor: { provider: "gitlab", external_id: "2", handle: "alice", is_bot: false },
      source: { provider: "gitlab", kind: "push", repo: "acme/api", thread_ref: "d1" },
      attributes: { branch: "main" },
      payload: { x: 1 },
      text: "hello",
      received_at: "2026-01-02T03:04:05.000Z",
    });
    const bare = eventToCore({ provider: "p", type: "t", teamId: "t" }, "e", () => new Date("2026-05-06T00:00:00Z"));
    expect(bare.received_at).toBe("2026-05-06T00:00:00.000Z");
    expect(bare.actor).toEqual({ provider: "p", external_id: "", handle: "", is_bot: false });
    expect(bare.attributes).toEqual({});
  });
});
