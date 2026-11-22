// Meta endpoints, probes and generic problem+json behavior.
import { expect, it } from "vitest";
import { Api, expectProblem } from "../harness/api.ts";
import { adminApi, adminContext } from "../harness/context.ts";
import { describeFeature, TAGS, target } from "../harness/target.ts";

describeFeature([TAGS.rest], "meta and probes", () => {
  it("serves /healthz and /readyz", async () => {
    for (const p of ["/healthz", "/readyz"]) {
      const r = await fetch(target.baseUrl + p);
      expect(r.status, p).toBe(200);
    }
  });

  it("GET /version", async () => {
    const v = await adminApi().get("/version");
    expect(v).toBeTypeOf("object");
  });

  it("GET /openapi.json returns the REST contract", async () => {
    const doc = await adminApi().get<{ openapi: string; paths: Record<string, unknown> }>("/openapi.json");
    expect(doc.openapi).toMatch(/^3\./);
    expect(Object.keys(doc.paths)).toContain("/runs");
  });

  it("GET /factory-files/schemas lists the definition schemas", async () => {
    await adminApi().get("/factory-files/schemas");
  });

  it("GET /login-providers works without credentials", async () => {
    await new Api().get("/login-providers");
  });

  it("the bootstrap result matches the contract", () => {
    const ctx = adminContext();
    expect(ctx.key).toMatch(/^of_/);
    expect(ctx.teamId).not.toBe("");
  });
});

describeFeature([TAGS.rest], "problem+json", () => {
  it("an unknown API path is 404", async () => {
    // Outside the spec: no operation, so the body is not validated.
    const res = await adminApi().request("GET", "/no-such-resource", { noValidate: true });
    expect(res.status).toBe(404);
  });

  it("an unknown run is resource_not_found and carries the request ID", async () => {
    const res = await adminApi().request("GET", "/runs/run_doesnotexist00000000");
    expect(res.status).toBe(404);
    const p = expectProblem(res);
    expect(p.code).toBe("resource_not_found");
    expect(res.headers.get("x-request-id")).toBeTruthy();
    expect(p.retryable).toBe(false);
  });

  it("a body that is not JSON is invalid_request", async () => {
    await adminApi().problem(400, "POST", "/secrets", { raw: "{not json", code: "invalid_request" });
  });

  it("a wrong method is refused (405)", async () => {
    const res = await adminApi().request("PATCH", "/runs", { body: {}, noValidate: true });
    expect(res.status).toBe(405);
    expect(res.headers.get("allow") ?? "").toMatch(/GET|POST/);
  });

  it("echoes a given X-Request-Id", async () => {
    const res = await adminApi().request("GET", "/runs/run_doesnotexist00000000", {
      headers: { "x-request-id": "contract-req-1" },
    });
    expect(res.headers.get("x-request-id")).toBe("contract-req-1");
  });
});
