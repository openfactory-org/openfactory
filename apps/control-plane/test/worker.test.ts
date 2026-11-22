import { env, exports } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { TeamDO } from "../src/index";
import { Router } from "../src/http/router";
import { ROUTES } from "../src/http/routes";

describe("control-plane Worker (C0 skeleton)", () => {
  it("answers 401 problem+json without credentials", async () => {
    const res = await exports.default.fetch("http://localhost/api/v1/runs/run_x", {
      headers: { "x-request-id": "req-test-1" },
    });
    expect(res.status).toBe(401);
    expect(res.headers.get("content-type")).toBe("application/problem+json");
    expect(res.headers.get("x-request-id")).toBe("req-test-1");
    const body = (await res.json()) as { code: string; instance: string; login_providers: unknown[] };
    expect(body.code).toBe("authentication_required");
    expect(body.instance).toBe("/api/v1/runs/run_x");
    expect(Array.isArray(body.login_providers)).toBe(true);
  });

  it("prefers a literal segment over a parameter", () => {
    const m = new Router(ROUTES).match("POST", "/secrets/rewrap");
    expect(m.kind === "match" && m.route.operationId).toBe("rewrapSecrets");
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
