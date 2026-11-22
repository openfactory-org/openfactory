import { describe, expect, it } from "vitest";
import { checkPluginInfo } from "../src/version";
import { PLUGIN_API_VERSION, type PluginInfo } from "../src/types";
import { PluginWorker, pluginInfo } from "../src/worker";
import { FakeHostApi, mockFetch } from "../src/testing";

const info = (over: Partial<PluginInfo> = {}): PluginInfo => ({
  name: "gitlab",
  version: "1.0.0",
  apiVersion: PLUGIN_API_VERSION,
  roles: ["forge"],
  ...over,
});

describe("checkPluginInfo", () => {
  it("accepts a matching plugin", () => {
    expect(checkPluginInfo(info())).toEqual({ ok: true });
  });

  it("rejects another API version, a bad name and no role", () => {
    const old = checkPluginInfo(info({ apiVersion: PLUGIN_API_VERSION + 1 }));
    expect(old.ok).toBe(false);
    expect(!old.ok && old.reason).toContain("API version");
    expect(checkPluginInfo(info({ name: "Bad Name" })).ok).toBe(false);
    expect(checkPluginInfo(info({ roles: [] })).ok).toBe(false);
    expect(checkPluginInfo(info({ apiVersion: 7 }), 7)).toEqual({ ok: true });
  });
});

describe("PluginWorker", () => {
  class Demo extends PluginWorker<{ V?: string }> {
    async info() {
      return pluginInfo({ name: "demo", roles: ["forge"] }, this.env.V);
    }
  }

  it("fills the API version and the default version", async () => {
    const p = new Demo({} as ExecutionContext, {});
    expect(await p.info()).toEqual({ name: "demo", roles: ["forge"], version: "0.0.0-dev", apiVersion: PLUGIN_API_VERSION });
    expect((await new Demo({} as ExecutionContext, { V: "3" }).info()).version).toBe("3");
    await expect(p.ping()).resolves.toBeUndefined();
    await expect(p.configure({ a: "b" })).resolves.toBeUndefined();
  });
});

describe("test harness", () => {
  it("answers 404 for an unmatched request and records it", async () => {
    const m = mockFetch({});
    const res = await m.fetch("https://x.test/nope");
    expect(res.status).toBe(404);
    expect(m.calls[0]!.rawPath).toBe("/nope");
  });

  it("fake host expands secret placeholders", async () => {
    const host = new FakeHostApi({
      integrations: [{ id: "i", teamId: "t", provider: "p", name: "n", config: { a_ref: "TOKEN" } }],
      secrets: { t: { TOKEN: "s3" } },
      teams: [{ id: "t", name: "T" }],
    });
    expect(await host.expandSecrets("team:t", "Bearer {{ TOKEN }}!")).toBe("Bearer s3!");
    expect(await host.listTeams()).toEqual([{ id: "t", name: "T" }]);
    await host.log("info", "hi", { a: "b" });
    expect(host.logs).toEqual([{ level: "info", message: "hi", attrs: { a: "b" } }]);
    await expect(host.getFactory("nope")).rejects.toMatchObject({ code: "resource_not_found" });
  });
});
