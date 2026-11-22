// Secrets: values are write-only.
import { expect, it } from "vitest";
import { uniq } from "../harness/api.ts";
import { adminApi } from "../harness/context.ts";
import { describeFeature, TAGS } from "../harness/target.ts";

function secretName(): string {
  return uniq("CT_SECRET").toUpperCase().replace(/-/g, "_");
}

describeFeature([TAGS.rest], "secrets", () => {
  it("create, read metadata, replace, delete", async () => {
    const api = adminApi();
    const name = secretName();
    const value = "contract-test-fake-value-1";
    const created = await api.post<{ name: string; type: string }>("/secrets", { name, type: "generic", value }, 201);
    expect(created.name).toBe(name);
    expect(created.type).toBe("generic");
    expect(JSON.stringify(created)).not.toContain(value);

    const list = await api.get<{ items: { name: string }[] }>("/secrets");
    expect(list.items.map((s) => s.name)).toContain(name);
    expect(JSON.stringify(list)).not.toContain(value);

    const got = await api.get(`/secrets/${name}`);
    expect(JSON.stringify(got)).not.toContain(value);

    const replaced = await api.put<{ name: string }>(`/secrets/${name}`, { value: "contract-test-fake-value-2" });
    expect(replaced.name).toBe(name);

    await api.del(`/secrets/${name}`);
    await api.problem(404, "GET", `/secrets/${name}`, { code: "resource_not_found" });
  });

  it("a duplicate name is a conflict", async () => {
    const api = adminApi();
    const name = secretName();
    await api.post("/secrets", { name, type: "generic", value: "a" }, 201);
    await api.problem(409, "POST", "/secrets", { body: { name, type: "generic", value: "b" }, code: "conflict" });
    await api.del(`/secrets/${name}`);
  });

  it("a missing value is invalid_request", async () => {
    await adminApi().problem(400, "POST", "/secrets", {
      body: { name: secretName(), type: "generic" },
      code: "invalid_request",
    });
  });

  it("an invalid name is invalid_request", async () => {
    await adminApi().problem(400, "POST", "/secrets", {
      body: { name: "not a valid name!", type: "generic", value: "x" },
      code: "invalid_request",
    });
  });

  it("an unknown type is invalid_request", async () => {
    await adminApi().problem(400, "POST", "/secrets", {
      body: { name: secretName(), type: "no_such_type", value: "x" },
      code: "invalid_request",
    });
  });

  it("a factory-scoped secret is listed with factory_id", async () => {
    const api = adminApi();
    const f = await api.post<{ factory: { uid: string } }>(
      "/factories",
      { name: uniq("ct-sec"), definition: "defaults" },
      201,
    );
    const name = secretName();
    const s = await api.post<{ factory_id?: string }>(
      "/secrets",
      { name, type: "generic", value: "x", factory_id: f.factory.uid },
      201,
    );
    expect(s.factory_id).toBe(f.factory.uid);
    const list = await api.get<{ items: { name: string }[] }>("/secrets", { query: { factory_id: f.factory.uid } });
    expect(list.items.map((i) => i.name)).toContain(name);
    await api.del(`/secrets/${name}`, 204, { query: { factory_id: f.factory.uid } });
  });
});

describeFeature([TAGS.rest, TAGS.secretsRewrap], "secrets rewrap", () => {
  it("POST /secrets/rewrap re-encrypts with the current key", async () => {
    const res = await adminApi().request("POST", "/secrets/rewrap", { body: {} });
    expect(res.status).toBe(200);
  });
});
