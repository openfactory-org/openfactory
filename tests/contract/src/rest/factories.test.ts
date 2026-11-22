// Factories and definition apply, with the example definition trees.
import { beforeAll, expect, it } from "vitest";
import { uniq } from "../harness/api.ts";
import {
  adminApi,
  createFactory,
  createTestFactory,
  ensureExampleSecrets,
  exampleDirs,
  minimalDefinition,
  readTree,
} from "../harness/context.ts";
import { describeFeature, TAGS } from "../harness/target.ts";

/**
 * Examples that need a team resource the suite does not create. Apply must
 * fail with invalid_request, and each error must be about one of these fields.
 */
const NEEDS_RESOURCE: Record<string, RegExp> = {
  // A registered MCP server installation.
  "05-incident-response": /^mcpServers\.[^.]+\.managedId$/,
  // An enrolled worker named acme-prod-worker.
  "07-self-hosted-worker": /^workerHost$/,
};

describeFeature([TAGS.rest], "definition validate with the examples", () => {
  let scratch = "";
  beforeAll(async () => {
    await ensureExampleSecrets(adminApi());
    scratch = (await createTestFactory(adminApi())).uid;
  });

  for (const ex of exampleDirs()) {
    it(`validates examples/${ex}`, async () => {
      const r = await adminApi().post<{ valid: boolean; errors: { field?: string; message: string }[] }>(
        `/factories/${scratch}/definition/validate`,
        { files: readTree(`examples/${ex}`) },
      );
      const allowed = NEEDS_RESOURCE[ex];
      if (!allowed) {
        expect(r.errors, JSON.stringify(r.errors)).toEqual([]);
        expect(r.valid).toBe(true);
      } else {
        for (const e of r.errors) expect(e.field ?? "", e.message).toMatch(allowed);
      }
    });
  }

  it("reports schema errors with file and field", async () => {
    const r = await adminApi().post<{ valid: boolean; errors: { file?: string; field?: string }[] }>(
      `/factories/${scratch}/definition/validate`,
      { files: { "factory.yaml": "schemaVersion: v1\nname: 5\nbogus: true\n" } },
    );
    expect(r.valid).toBe(false);
    expect(r.errors.some((e) => e.file === "factory.yaml" && e.field === "bogus")).toBe(true);
    expect(r.errors.some((e) => e.file === "agents")).toBe(true);
  });
});

describeFeature([TAGS.rest], "factories with the examples", () => {
  beforeAll(async () => {
    await ensureExampleSecrets(adminApi());
  });

  for (const ex of exampleDirs()) {
    it(`applies examples/${ex}`, async () => {
      const files = readTree(`examples/${ex}`);
      // Factory names are unique per team: give each run its own name.
      files["factory.yaml"] = (files["factory.yaml"] ?? "")
        .replace(/^name:.*$/m, `name: ${uniq("ex")}`)
        .replace(/^alias:.*$/m, "");
      const allowed = NEEDS_RESOURCE[ex];
      if (!allowed) {
        const f = await createFactory(adminApi(), files);
        const def = await adminApi().get<{ files?: Record<string, string> }>(`/factories/${f.uid}/definition`);
        expect(Object.keys(def.files ?? {})).toEqual(expect.arrayContaining(Object.keys(files)));
        const agents = await adminApi().get<{ items: { name: string }[] }>(`/factories/${f.uid}/agents`);
        const declared = Object.keys(files)
          .map((p) => /^agents\/([^/]+)\/agent\.md$/.exec(p)?.[1])
          .filter((n): n is string => !!n);
        expect(agents.items.map((a) => a.name)).toEqual(expect.arrayContaining(declared));
      } else {
        const p = await adminApi().problem(400, "POST", "/factories", { body: { files }, code: "invalid_request" });
        expect(p.errors?.length ?? 0).toBeGreaterThan(0);
        for (const e of p.errors ?? []) expect(e.field ?? "", e.message).toMatch(allowed);
      }
    });
  }
});

describeFeature([TAGS.rest], "factory lifecycle", () => {
  it("create with defaults, read, search, update, delete", async () => {
    const api = adminApi();
    const name = uniq("ct-fac");
    const created = await api.post<{ factory: { uid: string; source: string } }>(
      "/factories",
      { name, definition: "defaults" },
      201,
    );
    const uid = created.factory.uid;
    expect(created.factory.source).toBe("managed");
    const byUid = await api.get<{ uid: string; name: string }>(`/factories/${uid}`);
    expect(byUid.name).toBe(name);
    const list = await api.get<{ items: { uid: string }[] }>("/factories", { query: { search: name } });
    expect(list.items.map((f) => f.uid)).toContain(uid);

    const patched = await api.patch<{ description?: string }>(`/factories/${uid}`, {
      description: "changed by the contract suite",
    });
    expect(patched.description).toBe("changed by the contract suite");

    await api.del(`/factories/${uid}`);
    await api.problem(404, "GET", `/factories/${uid}`, { code: "resource_not_found" });
  });

  it("a duplicate name is a conflict", async () => {
    const api = adminApi();
    const name = uniq("ct-dup");
    await api.post("/factories", { name, definition: "defaults" }, 201);
    await api.problem(409, "POST", "/factories", { body: { name, definition: "defaults" }, code: "conflict" });
  });

  it("an invalid definition is invalid_request with errors", async () => {
    const p = await adminApi().problem(400, "POST", "/factories", {
      body: { files: { "factory.yaml": "schemaVersion: v1\nname: x\n" } },
      code: "invalid_request",
    });
    expect(p.errors?.length ?? 0).toBeGreaterThan(0);
  });
});

describeFeature([TAGS.rest], "definition versions", () => {
  it("PUT a new version, list versions, read one, restore the first", async () => {
    const api = adminApi();
    const name = uniq("ct-def");
    const files = minimalDefinition({ name });
    const f = await createFactory(api, files);
    const v1 = f.activeVersionId;
    expect(v1).toBeTruthy();

    const files2 = minimalDefinition({ name, agents: { helper: { type: "CUSTOM", description: "Helper" } } });
    const put = await api.put<{ version?: { id: string } }>(`/factories/${f.uid}/definition`, {
      files: files2,
      message: "add helper",
      base_version_id: v1,
    });
    const v2 = put.version?.id;
    expect(v2).toBeTruthy();
    expect(v2).not.toBe(v1);

    const agents = await api.get<{ items: { name: string }[] }>(`/factories/${f.uid}/agents`);
    expect(agents.items.map((a) => a.name)).toContain("helper");
    await api.get(`/factories/${f.uid}/agents/helper`);

    const versions = await api.get<{ items: { id: string }[] }>(`/factories/${f.uid}/definition/versions`);
    expect(versions.items.map((v) => v.id)).toEqual(expect.arrayContaining([v1, v2]));
    const one = await api.get<{ id: string; files: Record<string, string> }>(
      `/factories/${f.uid}/definition/versions/${v1}`,
    );
    expect(Object.keys(one.files)).not.toContain("agents/helper/agent.md");

    await api.post(`/factories/${f.uid}/definition/versions/${v1}/restore`, {});
    const after = await api.get<{ items: { name: string }[] }>(`/factories/${f.uid}/agents`);
    expect(after.items.map((a) => a.name)).not.toContain("helper");
  });

  it("a stale base_version_id is a conflict", async () => {
    const api = adminApi();
    const name = uniq("ct-stale");
    const f = await createFactory(api, minimalDefinition({ name }));
    await api.put(`/factories/${f.uid}/definition`, {
      files: minimalDefinition({ name, agents: { a: { type: "CUSTOM" } } }),
      base_version_id: f.activeVersionId,
    });
    await api.problem(409, "PUT", `/factories/${f.uid}/definition`, {
      body: {
        files: minimalDefinition({ name, agents: { b: { type: "CUSTOM" } } }),
        base_version_id: f.activeVersionId,
      },
      code: "conflict",
    });
  });

  it("GET settings, automations, integrations, schedules", async () => {
    const api = adminApi();
    const f = await createTestFactory(api);
    for (const p of ["settings", "automations", "integrations", "schedules", "spend", "work-items", "inbox"]) {
      await api.get(`/factories/${f.uid}/${p}`);
    }
  });
});
