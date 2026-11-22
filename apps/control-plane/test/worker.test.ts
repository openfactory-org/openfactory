import { env, exports } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { TeamDO } from "../src/index";

describe("control-plane Worker (C0 skeleton)", () => {
  it("answers 501 problem+json for a stubbed /api/v1 route", async () => {
    const res = await exports.default.fetch("http://localhost/api/v1/runs/run_x", {
      headers: { "x-request-id": "req-test-1" },
    });
    expect(res.status).toBe(501);
    expect(res.headers.get("content-type")).toBe("application/problem+json");
    expect(res.headers.get("x-request-id")).toBe("req-test-1");
    const body = (await res.json()) as { code: string; instance: string };
    expect(body.code).toBe("feature_not_available");
    expect(body.instance).toBe("/api/v1/runs/run_x");
  });

  it("prefers a literal segment over a parameter", async () => {
    const res = await exports.default.fetch("http://localhost/api/v1/secrets/rewrap", { method: "POST" });
    const body = (await res.json()) as { detail: string };
    expect(body.detail).toContain("rewrapSecrets");
  });

  it("answers 404 and 405 like the Go mux", async () => {
    expect((await exports.default.fetch("http://localhost/api/v1/nope")).status).toBe(404);
    const res = await exports.default.fetch("http://localhost/api/v1/runs/run_x", { method: "DELETE" });
    expect(res.status).toBe(405);
    expect(res.headers.get("allow")).toBe("GET");
  });

  it("serves the probes", async () => {
    expect((await exports.default.fetch("http://localhost/healthz")).status).toBe(200);
  });

  it("instantiates a DO stub", async () => {
    const stub = env.TEAM.getByName("team:default");
    expect(await stub.ping()).toBe("TeamDO");
    await runInDurableObject(stub, async (instance, state) => {
      expect(instance).toBeInstanceOf(TeamDO);
      state.storage.sql.exec("CREATE TABLE t(x INTEGER)");
      expect(state.storage.sql.exec("SELECT COUNT(*) AS n FROM t").one().n).toBe(0);
    });
  });

  it("binds every DO class", async () => {
    for (const ns of [env.WORK, env.DISPATCH, env.ROUTER, env.INDEX, env.INGRESS, env.WORKER_HUB, env.BENCHMARK]) {
      expect(typeof (await ns.getByName("x").ping())).toBe("string");
    }
  });
});
