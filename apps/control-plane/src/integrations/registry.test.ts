import { describe, expect, it } from "vitest";
import { INBOUND_PATHS, createIntegrationHandlers } from "./registry";
import { kit, post } from "./testkit";

describe("integration registry", () => {
  const h = createIntegrationHandlers(kit({}).deps);

  it("exposes responders, trackers, resolvers and context builders by provider", () => {
    expect(Object.keys(h.responders).sort()).toEqual(["jira", "linear", "slack", "teams"]);
    expect(Object.keys(h.trackers).sort()).toEqual(["jira", "linear"]);
    expect(Object.keys(h.nameResolvers).sort()).toEqual(["jira", "linear", "microsoft_teams", "slack", "teams"]);
    expect(Object.keys(h.contextBuilders).sort()).toEqual(["slack", "teams"]);
    for (const [name, r] of Object.entries(h.responders)) expect(r.provider()).toBe(name);
  });

  it("routes each inbound path to its handler and ignores other requests", async () => {
    for (const path of INBOUND_PATHS) {
      const out = await h.handleRequest(post("https://of.example.com" + path, "{}"));
      expect(out, path).not.toBeNull();
      expect([400, 401, 413, 200, 202]).toContain(out!.response.status);
    }
    expect(await h.handleRequest(post("https://of.example.com/other", "{}"))).toBeNull();
    expect(await h.handleRequest(new Request("https://of.example.com" + INBOUND_PATHS[0]))).toBeNull();
  });
});
