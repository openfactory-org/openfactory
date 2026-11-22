// The integration wiring of src/wiring.ts and src/index.ts: every server path
// reaches its owner module (none answers the 501 fallback).
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { doName } from "@openfactory/core";
import { dispatchRun } from "../src/api/runs";
import { matchServerRoutes } from "../src/builder";
import type { Principal } from "../src/contracts/do-messages";
import type { Env } from "../src/contracts/env";
import { providerFor } from "../src/do/dispatch/providers";
import { intakeHandler, setIntakeHandler } from "../src/do/ingress/index";
import { platformIntakeHandler } from "../src/do/ingress/intake";
import { ROUTES } from "../src/http/routes";
import worker from "../src/index";
import { bootstrapControlPlane, integrationPartition } from "../src/wiring";
import { bootstrap, call } from "./helpers";

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

describe("composition root", () => {
  it("registers the intake handler; without one, intake fails instead of dropping events", () => {
    const prev = setIntakeHandler(undefined);
    try {
      expect(prev).toBe(platformIntakeHandler);
      expect(() => intakeHandler()).toThrow(/no intake handler is registered/);
    } finally {
      setIntakeHandler(prev);
    }
  });

  it("a second bootstrap registers nothing twice", () => {
    bootstrapControlPlane();
    expect(matchServerRoutes("/plugins/gitlab/webhook")).toHaveLength(1);
    expect(matchServerRoutes("/runner/v1/sessions")).toHaveLength(1);
  });

  it("the cron safety net re-arms the DispatchDO alarm once per tick", async () => {
    let ensured = 0;
    const fake = {
      ...env,
      DISPATCH: { getByName: () => ({ ensureAlarm: async () => void ensured++ }) },
    } as unknown as Env;
    const ctx = { waitUntil: () => {}, passThroughOnException: () => {}, props: {} } as unknown as ExecutionContext;
    // Minute 1: no retention step and no git definition sync.
    const controller = { scheduledTime: Date.UTC(2026, 0, 1, 0, 1), cron: "* * * * *", noRetry: () => {} };
    await worker.scheduled(controller as ScheduledController, fake, ctx);
    expect(ensured).toBe(1);
  });

  it("GET /api/v1/workers/connect is a public socket route", async () => {
    const r = await call("GET", "/api/v1/workers/connect");
    expect(r.status).toBe(426);
    expect(r.res.headers.get("upgrade")).toBe("websocket");
  });
});

describe("dispatch authorization", () => {
  it("a refused caller leaves no router reservation", async () => {
    const { key } = await bootstrap();
    const f = (
      await call("POST", "/api/v1/factories", { key, body: { name: "Reserve " + crypto.randomUUID().slice(0, 6) } })
    ).body.factory as { id: string };
    // A principal without a valid role has less than the user role on every factory.
    const principal = {
      kind: "user",
      id: "usr_refused",
      userId: "usr_refused",
      teamId: "default",
      role: "",
      factoryRoles: {},
    } as unknown as Principal;
    const route = ROUTES.find((r) => r.operationId === "dispatchRun")!;
    const url = new URL(`http://localhost/api/v1/factories/${f.id}/runs`);
    const req = new Request(url, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ prompt: "Do the work.", ticket_ref: "linear:PAY-1" }),
    });
    const ctx = {
      params: { uid: f.id },
      url,
      requestId: "test",
      route,
      principal,
      executionCtx: {} as ExecutionContext,
    };
    const err = await dispatchRun(req, env, ctx).then(
      () => undefined,
      (e: unknown) => e,
    );
    // Before the fix WorkDO refused the call, after the router had reserved the key.
    expect(String(err)).toMatch(/requires the user role/);
    expect(await env.ROUTER.getByName(doName.router("default")).resolve(f.id, "linear:PAY-1")).toBeNull();
  });
});
