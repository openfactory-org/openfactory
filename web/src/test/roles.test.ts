import { describe, expect, it } from "vitest";
import { atLeast, normalizeRole, roleFor } from "../auth/AuthContext";
import type { Me } from "../api/client";

const me = (role: string, factory_roles?: Record<string, string>) =>
  ({ user: { id: "usr_1", email: "a@example.test", role }, team: { id: "t", name: "T" }, role, factory_roles, login_providers: [] }) as unknown as Me;

describe("roles", () => {
  it("accepts only the built-in role names", () => {
    expect(normalizeRole("viewer")).toBeNull();
    expect(normalizeRole(" Editor ")).toBe("editor");
    expect(normalizeRole("owner")).toBeNull();
  });

  it("uses the factory override before the team-wide role", () => {
    const m = me("user", { fac_a: "editor" });
    expect(roleFor(m)).toBe("user");
    expect(roleFor(m, "fac_a")).toBe("editor");
    expect(roleFor(m, "fac_b")).toBe("user");
    expect(atLeast(roleFor(m, "fac_a"), "editor")).toBe(true);
    expect(atLeast(roleFor(m, "fac_b"), "editor")).toBe(false);
    // An override can lower the role, and an unknown override is ignored.
    expect(roleFor(me("editor", { fac_a: "user", fac_b: "owner" }), "fac_a")).toBe("user");
    expect(roleFor(me("editor", { fac_b: "owner" }), "fac_b")).toBe("editor");
    expect(roleFor(null, "fac_a")).toBeNull();
    expect(atLeast(null, "user")).toBe(false);
  });
});
