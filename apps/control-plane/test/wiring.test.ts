// The integration wiring of src/wiring.ts and src/index.ts: every server path
// reaches its owner module (none answers the 501 fallback).
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { providerFor } from "../src/do/dispatch/providers";
import { integrationPartition } from "../src/wiring";
import { call } from "./helpers";

describe("server route wiring", () => {
  it.each([
    ["POST", "/integrations/slack/events"],
    ["POST", "/integrations/slack/interactions"],
    ["POST", "/integrations/teams/messages"],
    ["POST", "/integrations/linear/webhook"],
    ["POST", "/integrations/jira/webhook"],
    ["POST", "/integrations/github/webhook"],
    ["POST", "/webhooks/w/wh_unknown"],
    ["GET", "/integrations/link/not-a-token"],
    ["GET", "/integrations/link-start/slack"],
    ["POST", "/plugins/gitlab/webhook"],
    ["POST", "/mcp"],
    ["POST", "/api/v1/mcp/factory"],
  ])("%s %s reaches its handler", async (method, path) => {
    const r = await call(method, path, method === "GET" ? {} : { body: "{}" });
    expect(r.status, JSON.stringify(r.body)).not.toBe(501);
    // The stub plugin Worker serves no webhooks: the forwarder answers 503.
    if (!path.startsWith("/plugins/")) expect(r.status).toBeLessThan(500);
  });

  it("an unknown /integrations path is still 501", async () => {
    expect((await call("GET", "/integrations/nothing-here")).status).toBe(501);
  });
});

describe("providers", () => {
  it("daytona and boatdev need their API key secrets", () => {
    expect(() => providerFor(env, "default", "daytona")).toThrow(/OF_DAYTONA_API_KEY/);
    expect(() => providerFor(env, "default", "boatdev")).toThrow(/OF_BOATDEV_API_KEY/);
    expect(providerFor({ ...env, OF_BOATDEV_API_KEY: "boat_test-only-fake" }, "default", "boatdev").id).toBe("boatdev");
  });
});

describe("integration events", () => {
  it("events of one thread share a partition; other threads do not", async () => {
    const ev = (thread: string) =>
      ({
        id: "e",
        team_id: "default",
        provider: "slack",
        type: "message",
        actor: {},
        attributes: null,
        received_at: "",
        source: { provider: "slack", kind: "thread", channel: "C1", thread_id: thread },
      }) as never;
    expect(await integrationPartition(ev("1"))).toBe(await integrationPartition(ev("1")));
    expect(await integrationPartition(ev("1"))).not.toBe(await integrationPartition(ev("2")));
  });
});
