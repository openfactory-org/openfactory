import { describe, expect, it } from "vitest";
import { bootstrap, call, email } from "./helpers";

describe("bootstrap, me, users, API keys (internal/api platform_test ports)", () => {
  it("bootstraps once and authenticates with the key", async () => {
    const { key } = await bootstrap();
    expect(key).toMatch(/^of_[a-z0-9]{8}_[A-Za-z0-9]{40}$/);
    const again = await call("POST", "/api/v1/bootstrap", { body: { team_name: "X", admin_email: "b@example.com" } });
    expect(again.status).toBe(409);
    const me = await call("GET", "/api/v1/me", { key });
    expect(me.status).toBe(200);
    expect(me.body.role).toBe("admin");
    expect(me.body.team.id).toBe("default");
    expect(me.body.user.role).toBe("admin");
    expect(me.body.api_key.prefix).toBe(key.slice(3, 11));
    expect(me.body.login_providers.map((p: { name: string }) => p.name)).toContain("api_key");
  });

  it("rejects bad and malformed keys with 401", async () => {
    await bootstrap();
    expect((await call("GET", "/api/v1/me", { key: "of_aaaaaaaa_" + "A".repeat(40) })).status).toBe(401);
    expect((await call("GET", "/api/v1/me", { key: "of_nope" })).status).toBe(401);
    expect((await call("GET", "/api/v1/me", { key: "garbage" })).status).toBe(401);
  });

  it("creates users, enforces roles and keeps the last admin", async () => {
    const { key, userId } = await bootstrap();
    const e = email("Ed");
    const u = await call("POST", "/api/v1/users", { key, body: { email: e, create_api_key: true } });
    expect(u.status).toBe(201);
    expect(u.body.user.role).toBe("editor");
    expect(u.body.user.email).toBe(e.toLowerCase());
    const edKey = u.body.key as string;
    expect((await call("POST", "/api/v1/users", { key, body: { email: e.toLowerCase() } })).status).toBe(409);
    expect((await call("POST", "/api/v1/users", { key, body: { email: "not an email" } })).status).toBe(400);
    // Editor cannot create users (admin route).
    const forbidden = await call("POST", "/api/v1/users", { key: edKey, body: { email: "x@example.com" } });
    expect(forbidden.status).toBe(403);
    expect(forbidden.body.detail).toBe("this operation requires the admin role; your role is editor");
    const list = await call("GET", "/api/v1/users", { key: edKey });
    const admins = list.body.items.filter((x: { role: string }) => x.role === "admin");
    if (admins.length === 1) {
      const demote = await call("PATCH", `/api/v1/users/${userId}`, { key, body: { role: "user" } });
      expect(demote.status).toBe(409);
    }
    expect(list.body.page_info).toEqual({ has_next_page: false });
    expect((await call("GET", "/api/v1/users/usr_nope", { key })).status).toBe(404);
  });

  it("lists own keys for non-admins and revokes keys", async () => {
    const { key } = await bootstrap();
    const u = await call("POST", "/api/v1/users", {
      key,
      body: { email: email("u"), role: "user", create_api_key: true },
    });
    const userKey = u.body.key as string;
    const k2 = await call("POST", "/api/v1/api-keys", { key: userKey, body: { name: "laptop" } });
    expect(k2.status).toBe(201);
    const mine = await call("GET", "/api/v1/api-keys", { key: userKey });
    expect(mine.body.items).toHaveLength(2);
    const all = await call("GET", "/api/v1/api-keys", { key });
    expect(all.body.items.length).toBeGreaterThanOrEqual(3);
    const adminKeyId = all.body.items.find((k: { user_id: string }) => k.user_id !== u.body.user.id).id;
    expect((await call("DELETE", `/api/v1/api-keys/${adminKeyId}`, { key: userKey })).status).toBe(403);
    expect(
      (await call("POST", "/api/v1/api-keys", { key: userKey, body: { name: "x", user_id: "usr_other" } })).status,
    ).toBe(403);
    expect((await call("DELETE", `/api/v1/api-keys/${k2.body.api_key.id}`, { key: userKey })).status).toBe(204);
    expect((await call("GET", "/api/v1/me", { key: k2.body.key })).status).toBe(401);
  });

  it("manages service accounts and their ofa_ keys", async () => {
    const { key } = await bootstrap();
    const name = "ci-" + crypto.randomUUID().slice(0, 6);
    const sa = await call("POST", "/api/v1/service-accounts", { key, body: { name, create_api_key: true } });
    expect(sa.status).toBe(201);
    expect(sa.body.key).toMatch(/^ofa_/);
    expect(sa.body.service_account.id).toMatch(/^sa_/);
    const saKey = sa.body.key as string;
    expect((await call("GET", "/api/v1/me", { key: saKey })).body.role).toBe("user");
    // ofa_ key text with an of_ prefix does not authenticate.
    expect((await call("GET", "/api/v1/me", { key: "of_" + saKey.slice(4) })).status).toBe(401);
    expect((await call("POST", "/api/v1/service-accounts", { key, body: { name: name } })).status).toBe(409);
    expect((await call("POST", "/api/v1/service-accounts", { key, body: { name: "bad name" } })).status).toBe(400);
    expect((await call("GET", `/api/v1/service-accounts/${name}`, { key })).body.name).toBe(name);
    expect((await call("POST", "/api/v1/api-keys", { key: saKey, body: { name: "self" } })).status).toBe(403);
    const users = await call("GET", "/api/v1/users", { key });
    expect(users.body.items.every((u: { kind?: string }) => u.kind !== "service_account")).toBe(true);
    expect((await call("DELETE", `/api/v1/service-accounts/${name}`, { key })).status).toBe(204);
    expect((await call("GET", "/api/v1/me", { key: saKey })).status).toBe(401);
  });
});

describe("sessions and CSRF (internal/auth auth_test ports)", () => {
  it("logs in with an API key, uses the cookie, and needs CSRF for unsafe requests", async () => {
    const { key } = await bootstrap();
    expect((await call("POST", "/auth/session", { body: { api_key: key } })).status).toBe(403);
    const login = await call("POST", "/auth/session", { body: { api_key: key }, headers: { "x-of-csrf": "1" } });
    expect(login.status).toBe(200);
    const setCookie = login.res.headers.get("set-cookie")!;
    expect(setCookie).toMatch(
      /^of_session=[A-Za-z0-9_-]+; Path=\/; Expires=.*; Max-Age=604800; HttpOnly; SameSite=Lax$/,
    );
    const cookie = setCookie.split(";")[0]!;
    const me = await call("GET", "/api/v1/me", { headers: { cookie } });
    expect(me.status).toBe(200);
    expect(me.body.role).toBe("admin");
    const noCsrf = await call("POST", "/api/v1/api-keys", { headers: { cookie }, body: { name: "x" } });
    expect(noCsrf.status).toBe(403);
    const withOrigin = await call("POST", "/api/v1/api-keys", {
      headers: { cookie, origin: "http://localhost" },
      body: { name: "x" },
    });
    expect(withOrigin.status).toBe(201);
    const tampered = await call("GET", "/api/v1/me", { headers: { cookie: cookie.slice(0, -2) + "xx" } });
    expect(tampered.status).toBe(401);
    expect(tampered.res.headers.get("set-cookie")).toContain("Max-Age=0");
    const out = await call("POST", "/auth/logout", { headers: { cookie, "x-of-csrf": "1" } });
    expect(out.status).toBe(204);
  });

  it("ends a session when its API key is deleted", async () => {
    const { key } = await bootstrap();
    const k2 = await call("POST", "/api/v1/api-keys", { key, body: { name: "login" } });
    const login = await call("POST", "/auth/session", {
      body: { api_key: k2.body.key },
      headers: { "x-of-csrf": "1" },
    });
    const cookie = login.res.headers.get("set-cookie")!.split(";")[0]!;
    expect((await call("GET", "/api/v1/me", { headers: { cookie } })).status).toBe(200);
    await call("DELETE", `/api/v1/api-keys/${k2.body.api_key.id}`, { key });
    expect((await call("GET", "/api/v1/me", { headers: { cookie } })).status).toBe(401);
  });

  it("serves public routes", async () => {
    expect((await call("GET", "/api/v1/version")).body.version).toBeTruthy();
    expect((await call("GET", "/api/v1/login-providers")).body.login_providers).toEqual([
      { name: "api_key", display_name: "API key", login_url: "/auth/session" },
    ]);
    const sv = await call("GET", "/api/v1/factory-files/schemas");
    expect(sv.body.current_version).toBe("v1");
    expect((await call("GET", "/api/v1/factory-files/schemas/v1/factory")).status).toBe(200);
    expect((await call("GET", "/api/v1/factory-files/schemas/v9")).status).toBe(404);
    expect((await call("GET", "/api/v1/openapi.json")).body.openapi).toBeTruthy();
  });
});
