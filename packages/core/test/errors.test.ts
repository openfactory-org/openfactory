import { describe, expect, it } from "vitest";
import { ERROR_CODES, ERROR_SPECS, OfError, problemBody, problemResponse, toOfError } from "../src/index";

describe("OfError", () => {
  it("renders a problem body like internal/api/errors.go", () => {
    const e = new OfError("resource_not_found", "run not found");
    expect(problemBody(e, { instance: "/api/v1/runs/x", requestId: "req1" })).toEqual({
      type: "/errors/resource-not-found",
      title: "The referenced resource does not exist.",
      status: 404,
      detail: "run not found",
      instance: "/api/v1/runs/x",
      code: "resource_not_found",
      error: { code: "resource_not_found", message: "The referenced resource does not exist. (run not found)" },
      retryable: false,
      request_id: "req1",
    });
  });

  it("keeps extra members without overriding standard ones", () => {
    const e = new OfError("invalid_request", "bad", { extra: { errors: [], code: "x" } });
    const p = problemBody(e);
    expect(p.errors).toEqual([]);
    expect(p.code).toBe("invalid_request");
  });

  it("sets WWW-Authenticate on 401", async () => {
    const r = problemResponse(new OfError("authentication_required"));
    expect(r.status).toBe(401);
    expect(r.headers.get("www-authenticate")).toBe('Bearer realm="openfactory"');
    expect(r.headers.get("content-type")).toBe("application/problem+json");
    expect(((await r.json()) as { error: { message: string } }).error.message).toBe("A valid API key is required.");
  });

  it("rebuilds an error that crossed RPC as a plain Error", () => {
    const plain = new Error(new OfError("conflict", "stored version changed").message);
    const { error, known } = toOfError(plain);
    expect(known).toBe(true);
    expect(error.code).toBe("conflict");
    expect(error.detail).toBe("stored version changed");
    expect(toOfError(new Error("boom")).error.code).toBe("internal_error");
    expect(toOfError("x").known).toBe(false);
  });

  it("has a spec for every code", () => {
    for (const c of ERROR_CODES) expect(ERROR_SPECS[c].status).toBeGreaterThan(0);
  });
});
