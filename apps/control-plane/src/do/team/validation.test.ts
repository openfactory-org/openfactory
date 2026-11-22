// TeamDO request bodies: a field of the wrong JSON type is a 400
// (invalid_request) that names the field, not a 500, in the webhook, personal
// forge token, integration and MCP ops.
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { doName } from "@openfactory/core";
import type { CallMeta, Principal } from "../../contracts/do-messages";
import { bootstrap, call } from "../../../test/helpers";

const uniq = () => crypto.randomUUID().slice(0, 6);
const admin: Principal = {
  kind: "user",
  id: "usr_valid",
  teamId: "default",
  role: "admin",
  factoryRoles: {},
  userId: "usr_valid",
};
const meta: CallMeta = { requestId: "test", principal: admin };
type Out = { $error?: { code: string; detail: string } } & Record<string, unknown>;
const op = async (name: string, params: Record<string, string>, body: unknown): Promise<Out> =>
  (await env.TEAM.getByName(doName.team("default")).api(meta, name, params, body)) as Out;

describe("webhook and forge token bodies", () => {
  it("answers 400 for fields of the wrong type", async () => {
    const { key } = await bootstrap();
    const fr = await call("POST", "/api/v1/factories", { key, body: { name: "Valid " + uniq() } });
    expect(fr.status, JSON.stringify(fr.body)).toBe(201);
    const fid = fr.body.factory.id as string;
    const name = "hook-" + uniq();
    const ok = await call("POST", `/api/v1/factories/${fid}/webhooks`, { key, body: { name } });
    expect(ok.status, JSON.stringify(ok.body)).toBe(201);
    const id = ok.body.webhook.id as string;

    const cases: { what: string; method: string; path: string; body: unknown; field: string }[] = [
      {
        what: "create name",
        method: "POST",
        path: `/api/v1/factories/${fid}/webhooks`,
        body: { name: 5 },
        field: "name",
      },
      {
        what: "create enabled",
        method: "POST",
        path: `/api/v1/factories/${fid}/webhooks`,
        body: { name: "x-" + uniq(), enabled: "yes" },
        field: "enabled",
      },
      { what: "update secret", method: "PATCH", path: `/api/v1/webhooks/${id}`, body: { secret: 5 }, field: "secret" },
      {
        what: "update enabled",
        method: "PATCH",
        path: `/api/v1/webhooks/${id}`,
        body: { enabled: 1 },
        field: "enabled",
      },
      {
        what: "rotate secret",
        method: "POST",
        path: `/api/v1/webhooks/${id}/rotate-secret`,
        body: { secret: ["a"] },
        field: "secret",
      },
      {
        what: "forge token",
        method: "PUT",
        path: "/api/v1/me/forge-tokens/GITHUB",
        body: { token: 5 },
        field: "token",
      },
    ];
    for (const c of cases) {
      const r = await call(c.method, c.path, { key, body: c.body });
      expect(r.status, `${c.what}: ${JSON.stringify(r.body)}`).toBe(400);
      expect(JSON.stringify(r.body), c.what).toContain(`field \\"${c.field}\\"`);
    }
  });
});

describe("internal op bodies", () => {
  it.each([
    { op: "integrationsSetStatus", body: { id: "int_x", status: "bogus" }, detail: "status must be one of" },
    { op: "integrationsSetStatus", body: { id: 5, status: "active" }, detail: 'field "id" must be a string' },
    { op: "identity.link", body: { provider: 5, external_id: "u1" }, detail: 'field "provider" must be a string' },
    { op: "createIntegration", body: { provider: "slack", config: { team_id: 5 } }, detail: 'field "config"' },
    { op: "mcp.server.get", body: { serverId: 5 }, detail: 'field "serverId" must be a string' },
    { op: "mcp.oauth.token", body: "not an object", detail: "expected an object" },
    { op: "mcpauth.token", body: { form: "x" }, detail: 'field "form"' },
    { op: "userForgeTokenGet", body: { user_id: 1 }, detail: 'field "user_id" must be a string' },
  ])("$op answers 400 for $body", async ({ op: name, body, detail }) => {
    await bootstrap();
    const out = await op(name, {}, body);
    expect(out.$error?.code, JSON.stringify(out)).toBe("invalid_request");
    expect(out.$error?.detail).toContain(detail);
  });

  it("sets a known integration status", async () => {
    await bootstrap();
    const out = await op("integrationsSetStatus", {}, { id: "int_none", status: "auth_failed" });
    expect(out).toEqual({ changed: false });
  });
});
