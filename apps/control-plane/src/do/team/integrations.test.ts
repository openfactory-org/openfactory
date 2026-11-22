import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { doName } from "@openfactory/core";
import type { CallMeta, Principal } from "../../contracts/do-messages";
import { rpcStore } from "../../integrations/worker";
import { ROUTES } from "../../http/routes";

const TEAM = "inttest";
const admin: Principal = {
  kind: "user",
  id: "usr_adm",
  teamId: TEAM,
  role: "admin",
  factoryRoles: {},
  userId: "usr_adm",
};
const metaOf = (p?: Principal): CallMeta => (p ? { requestId: "r", principal: p } : { requestId: "r" });
const stub = () => env.TEAM.getByName(doName.team(TEAM));

type Err = { $error: { code: string; detail: string } };
const api = async <T = Record<string, unknown>>(
  op: string,
  params: Record<string, string> = {},
  body?: unknown,
  p: Principal | undefined = admin,
) => (await stub().api(metaOf(p), op, params, body)) as T & Partial<Err>;
const created = async (body: unknown) => {
  const out = (await api<{ $status: number; body: Record<string, unknown> }>("createIntegration", {}, body)) as {
    $status: number;
    body: Record<string, unknown>;
  };
  return out;
};

describe("integration routes", () => {
  it("maps the five operations to the module", () => {
    const ids = ROUTES.filter((r) => r.path.startsWith("/integrations")).map(
      (r) => `${r.method} ${r.operationId} ${r.role}`,
    );
    expect(ids.sort()).toEqual([
      "DELETE deleteIntegration admin",
      "GET getIntegration user",
      "GET listIntegrations user",
      "PATCH updateIntegration admin",
      "POST createIntegration admin",
    ]);
  });
});

describe("TeamDO integrations module", () => {
  it("creates a chat record with a normalized workspace, a status and a default name", async () => {
    const out = await created({
      provider: "jira",
      config: { site: "https://Acme.Atlassian.net/jira" },
      secret_ref: "JIRA_TOKEN",
      status: "disabled",
    });
    expect(out.$status).toBe(201);
    expect(out.body).toMatchObject({
      provider: "jira",
      name: "jira",
      team_id: TEAM,
      config: { site: "acme.atlassian.net" },
      secret_ref: "JIRA_TOKEN",
      status: "disabled",
      status_reason: "disabled by an admin",
    });
    expect(String(out.body["id"]).startsWith("int_")).toBe(true);
  });

  it("refuses a duplicate workspace, an unknown provider, a missing key and a bad secret name", async () => {
    await created({ provider: "slack", config: { team_id: "T100" } });
    const dup = await created({ provider: "slack", config: { team_id: "t100" } });
    expect((dup as unknown as Err).$error).toMatchObject({ code: "conflict" });
    expect(((await created({ provider: "nope" })) as unknown as Err).$error.code).toBe("invalid_request");
    expect(((await created({ provider: "linear", config: {} })) as unknown as Err).$error.detail).toContain(
      "config.organization_id is necessary",
    );
    expect(
      ((await created({ provider: "slack", config: { team_id: "T9" }, secret_ref: "bad name" })) as unknown as Err)
        .$error.detail,
    ).toContain("invalid secret_ref");
    expect(
      ((await created({ provider: "slack", config: { team_id: "T9" }, factory_id: "fac_none" })) as unknown as Err)
        .$error.detail,
    ).toContain("not a factory of this team");
  });

  it("checks forge records: org, credential, no slash, one record for each owner", async () => {
    const err = async (b: object) => ((await created(b)) as unknown as Err).$error.detail;
    expect(await err({ provider: "github" })).toContain("config.org is necessary");
    expect(await err({ provider: "github", config: { org: "a/b" } })).toContain("without a slash");
    expect(await err({ provider: "gitlab", config: { org: "grp" } })).toContain("needs a credential");
    expect(await err({ provider: "github", config: { org: "acme", app_id: "1" } })).toContain("private key");
    expect(
      (await created({ provider: "github", config: { org: "Acme", app_id: "1" }, secret_ref: "KEY" })).$status,
    ).toBe(201);
    expect(await err({ provider: "github", config: { org: "acme" }, secret_ref: "TOK" })).toContain(
      'already has a github record for the owner "acme"',
    );
  });

  it("lists with a provider filter, gets, updates and deletes", async () => {
    const rec = (await created({ provider: "linear", config: { organization_id: "ORG1" }, name: " My Linear " })).body;
    const id = String(rec["id"]);
    const list = await api<{ items: { provider: string }[]; page_info: unknown }>("listIntegrations", {
      "?provider": "linear",
    });
    expect(list.items.every((i) => i.provider === "linear")).toBe(true);
    expect(list.items.some((i) => (i as unknown as { id: string }).id === id)).toBe(true);
    expect(list.page_info).toEqual({ has_next_page: false });
    expect(await api("getIntegration", { id })).toMatchObject({ id, name: "My Linear" });
    const upd = await api<{ status: string; config: Record<string, string>; secret_ref?: string }>(
      "updateIntegration",
      { id },
      { config: { organization_id: "ORG2" }, secret_ref: "LIN", status: "disabled" },
    );
    expect(upd).toMatchObject({ status: "disabled", config: { organization_id: "ORG2" }, secret_ref: "LIN" });
    expect(((await api("updateIntegration", { id }, { provider: "jira" })) as Err).$error.detail).toBe(
      "provider cannot change",
    );
    expect(((await api("updateIntegration", { id }, { status: "auth_failed" })) as Err).$error.detail).toBe(
      "status must be active or disabled",
    );
    const back = await api<{ status: string; status_reason: string }>(
      "updateIntegration",
      { id },
      { status: "active", secret_ref: "" },
    );
    expect(back).toMatchObject({ status: "active", status_reason: "" });
    expect(back).not.toHaveProperty("secret_ref");
    expect(await api("deleteIntegration", { id })).toEqual({ $status: 204 });
    expect(((await api("getIntegration", { id })) as Err).$error.code).toBe("resource_not_found");
  });

  it("only an admin writes and a user reads", async () => {
    const user: Principal = { ...admin, role: "user" };
    expect(
      ((await api("createIntegration", {}, { provider: "slack", config: { team_id: "T7" } }, user)) as Err).$error.code,
    ).toBe("not_authorized");
    expect(
      ((await api("listIntegrations", {}, undefined, user)) as unknown as { items: unknown[] }).items,
    ).toBeDefined();
    const anon = (await stub().api(metaOf(), "listIntegrations", {}, undefined)) as Err;
    expect(anon.$error.code).toBe("authentication_required");
  });
});

describe("rpc store over TeamDO", () => {
  it("serves records, status, identities, users, settings and the key-value store", async () => {
    const store = rpcStore(env, TEAM);
    const rec = (await created({ provider: "slack", config: { team_id: "TRPC" }, secret_ref: "S" })).body as {
      id: string;
    };
    expect((await store.listIntegrations("")).some((i) => i.id === rec.id)).toBe(true);
    expect((await store.getIntegration(rec.id))?.provider).toBe("slack");
    expect(await store.getIntegration("int_none")).toBeNull();
    expect(await store.setIntegrationStatus(rec.id, "auth_failed", "token revoked")).toBe(true);
    expect(await store.setIntegrationStatus(rec.id, "auth_failed", "token revoked")).toBe(false);
    expect(await store.setIntegrationStatus("int_none", "disabled", "")).toBe(false);
    expect(await store.getIntegration(rec.id)).toMatchObject({ status: "auth_failed", status_reason: "token revoked" });

    await runInDurableObject(stub(), async (_i, state) => {
      const u = {
        id: "usr_rpc",
        team_id: TEAM,
        email: "rpc@example.com",
        name: "R",
        role: "editor",
        created_at: "2026-01-01T00:00:00Z",
      };
      state.storage.sql.exec(
        "INSERT INTO users (id, email, kind, role, name, created_ms, data) VALUES (?, ?, '', 'editor', 'R', 1, ?)",
        u.id,
        u.email,
        JSON.stringify(u),
      );
    });
    expect((await store.getUser("usr_rpc"))?.email).toBe("rpc@example.com");
    expect((await store.getUserByEmail(" RPC@example.com "))?.id).toBe("usr_rpc");
    expect(await store.getUserByEmail("")).toBeNull();

    const x = { user_id: "usr_rpc", provider: "slack", external_id: "UX", handle: "h" };
    expect(await store.findExternalIdentity("slack", "UX")).toBeNull();
    expect(await store.linkExternalIdentity(x)).toBe(true);
    expect(await store.linkExternalIdentity({ ...x, handle: "h2" })).toBe(true);
    expect(await store.linkExternalIdentity({ ...x, user_id: "usr_other" })).toBe(false);
    expect((await store.findExternalIdentity("slack", "UX"))?.handle).toBe("h2");
    await store.upsertExternalIdentity({ ...x, user_id: "" });
    expect((await store.findExternalIdentity("slack", "UX"))?.user_id).toBe("");
    expect(await store.linkExternalIdentity({ ...x, user_id: "usr_other" })).toBe(true); // an unlinked account can be linked

    expect(await store.getFactoryIntegrations("fac_none")).toEqual({});
    expect(await store.kvGet("teams:service_url:c")).toBeNull();
    await store.kvSet("teams:service_url:c", "https://smba.test/");
    await store.kvSet("teams:service_url:c", "https://smba2.test/");
    expect(await store.kvGet("teams:service_url:c")).toBe("https://smba2.test/");
  });

  it("exposes the forge side: records by provider and set status", async () => {
    await created({ provider: "gitlab", config: { org: "grp" }, secret_ref: "T" });
    const out = await api<{ items: { provider: string; id: string }[] }>("integrationsForProvider", {
      "?provider": "gitlab",
    });
    expect(out.items.length).toBeGreaterThan(0);
    expect(out.items.every((i) => i.provider === "gitlab")).toBe(true);
    const id = out.items[0]!.id;
    expect(await api("integrationsSetStatus", {}, { id, status: "auth_failed", reason: "401" })).toEqual({
      changed: true,
    });
    expect(((await api("integrationsSetStatus", {}, {})) as Err).$error.code).toBe("invalid_request");
  });
});
