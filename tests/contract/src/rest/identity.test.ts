// Auth, bootstrap, users and API keys.
import { beforeAll, expect, it } from "vitest";
import { Api, uniq } from "../harness/api.ts";
import { adminApi, adminContext } from "../harness/context.ts";
import { describeFeature, TAGS } from "../harness/target.ts";

describeFeature([TAGS.rest], "auth and bootstrap", () => {
  it("a second bootstrap is a conflict", async () => {
    await new Api().problem(409, "POST", "/bootstrap", {
      body: { team_name: "Again", admin_email: "again@example.com" },
      code: "conflict",
    });
  });

  it("GET /me without credentials is authentication_required", async () => {
    await new Api().problem(401, "GET", "/me", { code: "authentication_required" });
  });

  it("GET /me with an unknown key is authentication_required", async () => {
    await new Api("of_zzzzzzzz_notarealkeynotarealkeynotarealkey00").problem(401, "GET", "/me", {
      code: "authentication_required",
    });
  });

  it("GET /me with the admin key", async () => {
    const me = await adminApi().get<{ role: string; user: { id: string; email: string }; team: { id: string } }>("/me");
    const ctx = adminContext();
    expect(me.role).toBe("admin");
    expect(me.user.id).toBe(ctx.userId);
    expect(me.team.id).toBe(ctx.teamId);
  });

  it("GET /teams lists the team of the caller", async () => {
    const body = await adminApi().get<{ items: { id: string }[] }>("/teams");
    expect(body.items.map((t) => t.id)).toContain(adminContext().teamId);
    await adminApi().get(`/teams/${adminContext().teamId}`);
  });
});

describeFeature([TAGS.rest], "users", () => {
  let userKey = "";
  let userId = "";
  const email = `${uniq("user")}@example.com`;

  beforeAll(async () => {
    const res = await adminApi().post<{ user: { id: string; role: string }; key?: string }>(
      "/users",
      { email, name: "Contract User", role: "user", create_api_key: true },
      201,
    );
    userId = res.user.id;
    userKey = res.key ?? "";
  });

  it("creates a user with an API key", () => {
    expect(userId).not.toBe("");
    expect(userKey).toMatch(/^of_/);
  });

  it("lists and reads the user", async () => {
    const list = await adminApi().get<{ items: { id: string; email: string }[] }>("/users");
    expect(list.items.find((u) => u.id === userId)?.email).toBe(email);
    const u = await adminApi().get<{ role: string }>(`/users/${userId}`);
    expect(u.role).toBe("user");
  });

  it("the user sees role user in /me", async () => {
    const me = await new Api(userKey).get<{ role: string }>("/me");
    expect(me.role).toBe("user");
  });

  it("a user cannot create users", async () => {
    await new Api(userKey).problem(403, "POST", "/users", {
      body: { email: `${uniq("x")}@example.com`, role: "user" },
      code: "not_authorized",
    });
  });

  it("a user cannot create secrets (editor role)", async () => {
    await new Api(userKey).problem(403, "POST", "/secrets", {
      body: { name: "CT_FORBIDDEN", type: "generic", value: "x" },
      code: "not_authorized",
    });
  });

  it("an admin changes the role", async () => {
    const u = await adminApi().patch<{ role: string }>(`/users/${userId}`, { role: "editor" });
    expect(u.role).toBe("editor");
    const me = await new Api(userKey).get<{ role: string }>("/me");
    expect(me.role).toBe("editor");
  });

  it("a duplicate email is a conflict", async () => {
    await adminApi().problem(409, "POST", "/users", { body: { email, role: "user" }, code: "conflict" });
  });
});

describeFeature([TAGS.rest], "API keys", () => {
  it("create, list, use, delete", async () => {
    const api = adminApi();
    const name = uniq("key");
    const created = await api.post<{ api_key: { id: string; name: string; prefix: string }; key: string }>(
      "/api-keys",
      { name },
      201,
    );
    expect(created.key).toMatch(/^of_/);
    expect(created.key).toContain(created.api_key.prefix);

    const list = await api.get<{ items: { id: string; name: string }[] }>("/api-keys");
    const item = list.items.find((k) => k.id === created.api_key.id);
    expect(item?.name).toBe(name);
    // The secret part is never listed.
    expect(JSON.stringify(list)).not.toContain(created.key);

    const me = await new Api(created.key).get<{ role: string }>("/me");
    expect(me.role).toBe("admin");

    await api.del(`/api-keys/${created.api_key.id}`);
    await new Api(created.key).problem(401, "GET", "/me", { code: "authentication_required" });
  });

  it("a missing name is invalid_request", async () => {
    await adminApi().problem(400, "POST", "/api-keys", { body: {}, code: "invalid_request" });
  });

  it("deleting an unknown key is resource_not_found", async () => {
    await adminApi().problem(404, "DELETE", "/api-keys/key_doesnotexist0000000", { code: "resource_not_found" });
  });
});
