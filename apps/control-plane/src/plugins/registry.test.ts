import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { PLUGIN_API_VERSION, type ForgePlugin, type PluginInfo } from "@openfactory/plugin-api";
import { discoverPlugins, forgetPlugins, loadPlugins, type PluginStub } from "./registry";

const info = (over: Partial<PluginInfo> = {}): PluginInfo => ({
  name: "gitlab",
  version: "1.0.0",
  apiVersion: PLUGIN_API_VERSION,
  roles: ["forge", "ingress"],
  forgeProviders: ["GitLab"],
  ...over,
});

const stub = (i: PluginInfo | Error | (() => Promise<PluginInfo>)): PluginStub =>
  ({
    info: typeof i === "function" ? i : i instanceof Error ? () => Promise.reject(i) : async () => i,
    ping: async () => {},
  }) as PluginStub;

const quiet = { log: () => {} };

describe("loadPlugins", () => {
  it("loads plugins in binding order and ignores other vars", async () => {
    const reg = await loadPlugins(
      { PLUGIN_B: stub(info({ name: "b", roles: ["sandbox"] })), PLUGIN_A: stub(info()), PLUGIN_VERSION: "1", OTHER: stub(info({ name: "x" })), NULLISH: null },
      quiet,
    );
    expect(reg.plugins.map((p) => [p.binding, p.info.name])).toEqual([["PLUGIN_A", "gitlab"], ["PLUGIN_B", "b"]]);
    expect(reg.rejected).toEqual([]);
    expect(reg.get("b")?.binding).toBe("PLUGIN_B");
    expect(reg.withRole("sandbox").map((p) => p.info.name)).toEqual(["b"]);
    expect(reg.get("zzz")).toBeUndefined();
  });

  it("rejects a version mismatch, a bad name, a duplicate and an unreachable plugin", async () => {
    const logs: string[] = [];
    const reg = await loadPlugins(
      {
        PLUGIN_OLD: stub(info({ name: "old", apiVersion: PLUGIN_API_VERSION + 1 })),
        PLUGIN_BAD: stub(info({ name: "Bad Name" })),
        PLUGIN_ONE: stub(info({ name: "same" })),
        PLUGIN_TWO: stub(info({ name: "same" })),
        PLUGIN_DOWN: stub(new Error("no such worker")),
        PLUGIN_SLOW: stub(() => new Promise<PluginInfo>(() => {})),
      },
      { timeoutMs: 20, log: (m) => logs.push(m) },
    );
    expect(reg.plugins.map((p) => p.binding)).toEqual(["PLUGIN_ONE"]);
    const reasons = Object.fromEntries(reg.rejected.map((r) => [r.binding, r.reason]));
    expect(reasons["PLUGIN_OLD"]).toContain("API version");
    expect(reasons["PLUGIN_BAD"]).toContain("invalid plugin name");
    expect(reasons["PLUGIN_TWO"]).toContain("already bound");
    expect(reasons["PLUGIN_DOWN"]).toContain("unreachable: no such worker");
    expect(reasons["PLUGIN_SLOW"]).toContain("timed out");
    expect(logs).toHaveLength(5);
  });

  it("finds the plugin of a forge provider and builds the resolver map", async () => {
    const reg = await loadPlugins(
      { PLUGIN_GITLAB: stub(info()), PLUGIN_SBX: stub(info({ name: "sbx", roles: ["sandbox"], forgeProviders: ["gitlab"] })) },
      quiet,
    );
    expect(reg.forProvider("gitlab")?.info.name).toBe("gitlab");
    expect(reg.forProvider("github")).toBeUndefined();
    const forges = reg.forgePlugins();
    expect(Object.keys(forges).sort()).toEqual(["gitlab"]);
    expect(forges["gitlab"]).toBe(reg.get("gitlab")!.stub as ForgePlugin);
  });

  it("keys forge plugins by name and by provider", async () => {
    const reg = await loadPlugins({ PLUGIN_X: stub(info({ name: "acme-forge", forgeProviders: ["Acme", "acme_cloud"] })) }, quiet);
    expect(Object.keys(reg.forgePlugins()).sort()).toEqual(["acme", "acme-forge", "acme_cloud"]);
  });

  it("rejects the stub Workers behind the real service bindings that have no info()", async () => {
    const reg = await loadPlugins(env, { ...quiet, timeoutMs: 2000 });
    const bound = reg.rejected.map((r) => r.binding).sort();
    expect(bound).toContain("PLUGIN_GITLAB");
    expect(bound).toContain("PLUGIN_AZUREDEVOPS");
    expect(reg.rejected.every((r) => r.reason.startsWith("unreachable"))).toBe(true);
  });
});

describe("discoverPlugins", () => {
  it("caches per env until the TTL ends or the cache is forgotten", async () => {
    let calls = 0;
    const e = { PLUGIN_A: stub(async () => (calls++, info())) };
    let t = 1000;
    const opts = { ...quiet, ttlMs: 100, now: () => t };
    const a = await discoverPlugins(e, opts);
    const b = await discoverPlugins(e, opts);
    expect(b).toBe(a);
    expect(calls).toBe(1);
    t += 101;
    expect(await discoverPlugins(e, opts)).not.toBe(a);
    expect(calls).toBe(2);
    forgetPlugins(e);
    await discoverPlugins(e, opts);
    expect(calls).toBe(3);
    expect(await discoverPlugins(e, { ...opts, ttlMs: 0 })).toBeDefined();
    expect(calls).toBe(4);
  });
});
