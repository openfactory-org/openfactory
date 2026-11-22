import { describe, expect, it } from "vitest";
import { bootstrap, call, email } from "./helpers";

const uniq = () => crypto.randomUUID().slice(0, 6);

describe("factories and definitions (internal/api factories_test ports)", () => {
  it("creates a factory with the default definition and reads it by ID and alias", async () => {
    const { key } = await bootstrap();
    const name = "Payments " + uniq();
    const r = await call("POST", "/api/v1/factories", { key, body: { name } });
    expect(r.status, JSON.stringify(r.body)).toBe(201);
    const f = r.body.factory;
    expect(f.uid).toBe(f.id);
    expect(f.id).toMatch(/^fac_/);
    expect(f.alias).toBe(name.toLowerCase().replace(" ", "-"));
    expect(f.active_version_id).toBe(r.body.version.id);
    expect(f.url).toBe(`http://localhost/f/${f.id}`);
    expect((await call("GET", `/api/v1/factories/${f.alias}`, { key })).body.id).toBe(f.id);
    expect((await call("GET", `/api/v1/factories/fac_missing`, { key })).status).toBe(404);
    expect((await call("POST", "/api/v1/factories", { key, body: { name: name + "x", alias: f.alias } })).status).toBe(409);

    const agents = await call("GET", `/api/v1/factories/${f.id}/agents`, { key });
    expect(agents.body.items.length).toBeGreaterThan(0);
    const foreman = agents.body.items.find((a: { type: string }) => a.type === "FOREMAN");
    expect(foreman.id).toMatch(/^agt_[0-9a-f]{20}$/);
    expect(Array.isArray(foreman.spawnable_by)).toBe(true);
    expect((await call("GET", `/api/v1/factories/${f.id}/agents/${foreman.name}`, { key })).body.id).toBe(foreman.id);

    const def = await call("GET", `/api/v1/factories/${f.id}/definition`, { key });
    expect(def.body.files["factory.yaml"]).toContain(name);
    expect(def.body.version.state).toBe("active");

    const list = await call("GET", "/api/v1/factories?search=" + encodeURIComponent(name.toUpperCase()), { key });
    expect(list.body.items.map((x: { id: string }) => x.id)).toEqual([f.id]);
  });

  it("puts a definition, keeps invalid versions out of the active slot and lists versions", async () => {
    const { key } = await bootstrap();
    const f = (await call("POST", "/api/v1/factories", { key, body: { name: "Defs " + uniq() } })).body.factory;
    const files = (await call("GET", `/api/v1/factories/${f.id}/definition`, { key })).body.files as Record<string, string>;
    const bad = await call("PUT", `/api/v1/factories/${f.id}/definition`, { key, body: { files: { "factory.yaml": "name: [" } } });
    expect(bad.status).toBe(400);
    expect(Array.isArray(bad.body.errors)).toBe(true);
    const missingSecret = { ...files, "factory.yaml": files["factory.yaml"] + "secrets:\n  - NOT_A_SECRET_X\n" };
    const pre = await call("PUT", `/api/v1/factories/${f.id}/definition`, { key, body: { files: missingSecret } });
    expect(pre.status).toBe(400);
    expect(pre.body.errors[0].message).toContain("NOT_A_SECRET_X");
    const v = await call("POST", `/api/v1/factories/${f.id}/definition/validate`, { key, body: { files: missingSecret } });
    expect(v.body.valid).toBe(false);
    await call("POST", "/api/v1/secrets", { key, body: { name: "NOT_A_SECRET_X", value: "v1" } });
    const ok = await call("PUT", `/api/v1/factories/${f.id}/definition`, { key, body: { files: missingSecret, message: "add secret" } });
    expect(ok.status, JSON.stringify(ok.body)).toBe(200);
    const stale = await call("PUT", `/api/v1/factories/${f.id}/definition`, {
      key,
      body: { files, base_version_id: f.active_version_id },
    });
    expect(stale.status).toBe(409);
    const vs = await call("GET", `/api/v1/factories/${f.id}/definition/versions?limit=2`, { key });
    expect(vs.body.items).toHaveLength(2);
    expect(vs.body.page_info.has_next_page).toBe(true);
    expect(vs.body.active_version_id).toBe(ok.body.version.id);
    const next = await call("GET", `/api/v1/factories/${f.id}/definition/versions?limit=2&cursor=${vs.body.page_info.next_cursor}`, { key });
    const states = [...vs.body.items, ...next.body.items].map((x: { state: string }) => x.state).sort();
    expect(states).toEqual(["active", "invalid", "superseded"]);
    expect(next.body.page_info.has_next_page).toBe(false);
    const restored = await call("POST", `/api/v1/factories/${f.id}/definition/versions/${f.active_version_id}/restore`, { key });
    expect(restored.status).toBe(200);
    expect(restored.body.message).toMatch(/^Restore version /);
  });

  it("updates factory settings through factory.yaml and enforces factory roles", async () => {
    const { key } = await bootstrap();
    const f = (await call("POST", "/api/v1/factories", { key, body: { name: "Roles " + uniq() } })).body.factory;
    const upd = await call("PATCH", `/api/v1/factories/${f.id}`, { key, body: { description: "New text" } });
    expect(upd.status, JSON.stringify(upd.body)).toBe(200);
    expect(upd.body.description).toBe("New text");
    const def = await call("GET", `/api/v1/factories/${f.id}/definition`, { key });
    expect(def.body.files["factory.yaml"]).toContain("description: New text");

    const u = await call("POST", "/api/v1/users", { key, body: { email: email("viewer"), role: "user", create_api_key: true } });
    const userKey = u.body.key as string;
    const denied = await call("PATCH", `/api/v1/factories/${f.id}`, { key: userKey, body: { description: "x" } });
    expect(denied.status).toBe(403);
    expect(denied.body.detail).toBe("this operation requires the editor role; your role for this factory is user");
    await call("PATCH", `/api/v1/users/${u.body.user.id}`, { key, body: { factory_roles: { [f.id]: "editor" } } });
    expect((await call("PATCH", `/api/v1/factories/${f.id}`, { key: userKey, body: { description: "y" } })).status).toBe(200);
    expect((await call("DELETE", `/api/v1/factories/${f.id}`, { key: userKey })).status).toBe(403);
    expect((await call("DELETE", `/api/v1/factories/${f.id}`, { key })).status).toBe(204);
    expect((await call("GET", `/api/v1/factories/${f.id}`, { key })).status).toBe(404);
  });

  it("edits automations and agent specs", async () => {
    const { key } = await bootstrap();
    const f = (await call("POST", "/api/v1/factories", { key, body: { name: "Auto " + uniq() } })).body.factory;
    const content = "---\nenabled: true\nagent: foreman\ntriggers:\n  - provider: schedule\n    event: cron_fired\n    schedule:\n      cron: \"0 9 * * 1\"\n---\n\nDo the weekly thing.\n";
    const put = await call("PUT", `/api/v1/factories/${f.id}/automations/weekly`, { key, body: { content } });
    expect(put.status, JSON.stringify(put.body)).toBe(200);
    expect(put.body.automation.name).toBe("weekly");
    expect((await call("GET", `/api/v1/factories/${f.id}/automations`, { key })).body.items.map((a: { name: string }) => a.name)).toContain("weekly");
    expect((await call("PUT", `/api/v1/factories/${f.id}/automations/new`, { key, body: { content } })).status).toBe(400);
    expect((await call("DELETE", `/api/v1/factories/${f.id}/automations/weekly`, { key })).status).toBe(204);
    expect((await call("GET", `/api/v1/factories/${f.id}/automations/weekly`, { key })).status).toBe(404);

    const spec = await call("GET", `/api/v1/factories/${f.id}/agents/foreman/spec`, { key });
    expect(spec.status, JSON.stringify(spec.body)).toBe(200);
    expect(spec.body.has_file).toBe(true);
    const put2 = await call("PUT", `/api/v1/factories/${f.id}/agents/foreman/spec`, {
      key,
      body: { ...spec.body, description: "Plans the work", idle_timeout_minutes: "5" },
    });
    expect(put2.status, JSON.stringify(put2.body)).toBe(200);
    expect(put2.body.description).toBe("Plans the work");
    expect(put2.body.idle_timeout_minutes).toBe("5");
    const settings = await call("GET", `/api/v1/factories/${f.id}/settings`, { key });
    expect(settings.body.credential_strategy).toBe("EXECUTOR");
    const ints = await call("PUT", `/api/v1/factories/${f.id}/integrations`, { key, body: { integrations: ["slack"] } });
    expect(ints.status, JSON.stringify(ints.body)).toBe(200);
    expect(Object.keys(ints.body.integrations)).toEqual(["slack"]);
    expect((await call("PUT", `/api/v1/factories/${f.id}/integrations`, { key, body: { integrations: ["linear", "jira"] } })).status).toBe(400);
  });
});

describe("secrets (internal/api secrets and internal/secrets ports)", () => {
  it("creates, reads metadata, updates, rewraps and deletes secrets", async () => {
    const { key } = await bootstrap();
    const name = "API_TOKEN_" + uniq().toUpperCase().replace(/-/g, "_");
    const c = await call("POST", "/api/v1/secrets", { key, body: { name, value: "s3cret" } });
    expect(c.status, JSON.stringify(c.body)).toBe(201);
    expect(c.body.type).toBe("generic");
    expect(c.body).not.toHaveProperty("value");
    expect((await call("POST", "/api/v1/secrets", { key, body: { name, value: "x" } })).status).toBe(409);
    expect((await call("POST", "/api/v1/secrets", { key, body: { name: "OF_X", value: "x" } })).status).toBe(400);
    expect((await call("POST", "/api/v1/secrets", { key, body: { name: "K1", value: "a b", type: "openai_api_key" } })).status).toBe(400);
    expect((await call("POST", "/api/v1/secrets", { key, body: { name: "K2", value: "x", type: "aws_ecr_credential" } })).status).toBe(400);
    const u = await call("PUT", `/api/v1/secrets/${name}`, { key, body: { value: "new" } });
    expect(u.status).toBe(200);
    expect(u.body.id).toBe(c.body.id);
    const rw = await call("POST", "/api/v1/secrets/rewrap", { key });
    expect(rw.status, JSON.stringify(rw.body)).toBe(200);
    expect(rw.body.rewrapped).toBe(0);
    expect(rw.body.key_id).toMatch(/^[0-9a-f]{8}$/);
    expect((await call("GET", "/api/v1/secrets", { key })).body.items.some((s: { name: string }) => s.name === name)).toBe(true);
    expect((await call("DELETE", `/api/v1/secrets/${name}`, { key })).status).toBe(204);
    expect((await call("GET", `/api/v1/secrets/${name}`, { key })).status).toBe(404);
  });
});
