// TeamDO webhooks: when the definition apply of an update or a secret
// rotation fails, the webhook secret keeps its previous value.
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { bootstrap, call } from "../../../test/helpers";

const uniq = () => crypto.randomUUID().slice(0, 6);

async function secretValue(name: string, factoryId = ""): Promise<string | undefined> {
  const team = env.TEAM.getByName("team:default");
  const out = (await team.api({ requestId: "test" }, "secretsGet", {}, { factory_id: factoryId, name })) as {
    found: boolean;
    value: string;
  };
  return out.found ? out.value : undefined;
}

async function newFactory(key: string): Promise<string> {
  const r = await call("POST", "/api/v1/factories", { key, body: { name: "Hooks " + uniq() } });
  expect(r.status, JSON.stringify(r.body)).toBe(201);
  return r.body.factory.id as string;
}

describe("webhook secret rollback", () => {
  it("keeps the old signing secret when the update cannot be applied", async () => {
    const { key } = await bootstrap();
    const fid = await newFactory(key);
    const name = "gh-" + uniq();
    const secretName = `GH_${uniq().toUpperCase()}_SECRET`.replace(/-/g, "_");
    const cr = await call("POST", `/api/v1/factories/${fid}/webhooks`, {
      key,
      body: {
        name,
        auth_mode: "signature",
        signature_scheme: "github",
        secret_name: secretName,
        secret: "old-signing-secret",
      },
    });
    expect(cr.status, JSON.stringify(cr.body)).toBe(201);
    const id = cr.body.webhook.id as string;
    expect(await secretValue(secretName)).toBe("old-signing-secret");

    // An invalid deliveryIdHeader makes the apply fail after the new secret is stored.
    const up = await call("PATCH", `/api/v1/webhooks/${id}`, {
      key,
      body: { secret: "new-signing-secret", delivery_id_header: "Bad Header!" },
    });
    expect(up.status, JSON.stringify(up.body)).toBe(400);
    expect(await secretValue(secretName)).toBe("old-signing-secret");

    const ok = await call("PATCH", `/api/v1/webhooks/${id}`, { key, body: { secret: "new-signing-secret" } });
    expect(ok.status, JSON.stringify(ok.body)).toBe(200);
    expect(await secretValue(secretName)).toBe("new-signing-secret");
  });

  it("keeps the old token when the rotation cannot be applied", async () => {
    const { key } = await bootstrap();
    const fid = await newFactory(key);
    const name = "tok-" + uniq();
    const secretName = `TOK_${uniq().toUpperCase()}_SECRET`.replace(/-/g, "_");
    const cr = await call("POST", `/api/v1/factories/${fid}/webhooks`, {
      key,
      body: { name, auth_mode: "url_token", secret_name: secretName },
    });
    expect(cr.status, JSON.stringify(cr.body)).toBe(201);
    const id = cr.body.webhook.id as string;
    const old = await secretValue(secretName);
    expect(old).toBeTruthy();

    // A factory secret of the same name shadows the team secret in the
    // preflight; its value cannot be used in a URL, so the apply fails.
    const shadow = await call("POST", "/api/v1/secrets", {
      key,
      body: { name: secretName, value: "not a url token!", factory_id: fid },
    });
    expect(shadow.status, JSON.stringify(shadow.body)).toBe(201);
    const rot = await call("POST", `/api/v1/webhooks/${id}/rotate-secret`, { key });
    expect(rot.status, JSON.stringify(rot.body)).toBe(400);
    expect(await secretValue(secretName)).toBe(old);
  });
});
