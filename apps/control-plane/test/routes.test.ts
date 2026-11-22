import { describe, expect, it } from "vitest";
import openapi from "../../../api/openapi.json";
import { ROUTES } from "../src/http/routes";

// Port of TestOpenAPICoversRoutes: the route table and api/openapi.json have the same operations and roles.
describe("route table", () => {
  const ops: { key: string; operationId: string; role: string }[] = [];
  for (const [path, item] of Object.entries(
    openapi.paths as Record<string, Record<string, { operationId: string; "x-required-role"?: string }>>,
  )) {
    for (const method of ["get", "post", "put", "patch", "delete"]) {
      const op = item[method];
      if (op)
        ops.push({
          key: `${method.toUpperCase()} ${path}`,
          operationId: op.operationId,
          role: op["x-required-role"] ?? "public",
        });
    }
  }

  it("covers every openapi operation exactly once", () => {
    const keys = ROUTES.map((r) => `${r.method} ${r.path}`);
    expect(new Set(keys).size).toBe(keys.length);
    expect([...keys].sort()).toEqual(ops.map((o) => o.key).sort());
  });

  it("uses the operationId and role of openapi", () => {
    const byKey = new Map(ROUTES.map((r) => [`${r.method} ${r.path}`, r]));
    for (const o of ops) {
      const r = byKey.get(o.key)!;
      expect(r.operationId, o.key).toBe(o.operationId);
      expect(r.role, o.key).toBe(o.role);
      expect(r.handler.name === "" || typeof r.handler === "function").toBe(true);
    }
  });
});
