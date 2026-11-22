import { describe, expect, it } from "vitest";
import { PLUGIN_API_VERSION, checkPluginInfo } from "@openfactory/plugin-api";
import { FakeHostApi, webhookRequest } from "@openfactory/plugin-api/testing";
import { Plugin } from "./index";

const plugin = () => new Plugin({} as ExecutionContext, {});
const URL = "https://cp.test/plugins/hello/webhook";

describe("hello plugin", () => {
  it("passes the version check", async () => {
    const info = await plugin().info();
    expect(info.apiVersion).toBe(PLUGIN_API_VERSION);
    expect(checkPluginInfo(info)).toEqual({ ok: true });
  });

  it("turns a webhook body into one event", async () => {
    const host = new FakeHostApi();
    const r = await plugin().handleWebhook(webhookRequest(URL, { teamId: "team_1", factoryId: "fac_1", text: "hi" }, { "x-delivery-id": "d1" }), host);
    expect(r.status).toBe(202);
    expect(r.events).toHaveLength(1);
    expect(r.events[0]).toMatchObject({ teamId: "team_1", factoryId: "fac_1", provider: "hello", type: "received", deliveryId: "d1", text: "hi" });
    expect(host.logs).toHaveLength(1);
  });

  it("rejects a bad body", async () => {
    const host = new FakeHostApi();
    expect((await plugin().handleWebhook(webhookRequest(URL, { text: "x" }), host)).status).toBe(400);
    expect((await plugin().handleWebhook(new Request(URL, { method: "POST", body: "{" }), host)).status).toBe(400);
  });

  it("replies without error", async () => {
    await expect(
      plugin().reply("hello", "team_1", { workItemId: "w", source: { provider: "hello", kind: "message" }, text: "t", kind: "update" }),
    ).resolves.toBeUndefined();
  });
});
