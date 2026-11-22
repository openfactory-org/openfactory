// TeamDO hot paths: lookups use an index (query plans), the definition check
// reuses a compile result that the caller already has (create and put compile
// once), and GET routes read the compiled runtime instead of compiling the
// active files again.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { compile } from "@openfactory/definition";
import type { Principal } from "../../contracts/do-messages";
import { bootstrap } from "../../../test/helpers";
import * as factories from "./factories";
import { applyDefinition, checkDefinition, type CompileResult } from "./factories/definitions";
import { getFactoryById } from "./factories/repo";
import type { OpCall, TeamKit } from "./kit";

const uniq = () => crypto.randomUUID().slice(0, 6);

/** The query plan lines of sql in the default team. */
async function plan(sql: string, ...args: SqlStorageValue[]): Promise<string[]> {
  return runInDurableObject(env.TEAM.getByName("team:default"), async (instance) => {
    const kit = (instance as unknown as { kit: TeamKit }).kit;
    return kit.sql
      .exec<{ detail: string }>("EXPLAIN QUERY PLAN " + sql, ...args)
      .toArray()
      .map((r) => r.detail);
  });
}

describe("TeamDO lookups use an index", () => {
  it.each([
    {
      name: "factory by alias (findFactory, preflight alias check)",
      sql: "SELECT data FROM factories WHERE alias = ? COLLATE NOCASE ORDER BY created_ms LIMIT 1",
      args: ["x"],
      index: "factories_alias_nocase",
    },
    {
      name: "alias taken by another factory (checkAliasFree)",
      sql: "SELECT data FROM factories WHERE alias = ? COLLATE NOCASE AND id != ? ORDER BY rowid LIMIT 1",
      args: ["x", "fac_1"],
      index: "factories_alias_nocase",
    },
    {
      name: "service account by name",
      sql: "SELECT data FROM users WHERE kind = ? AND name = ? ORDER BY created_ms, id LIMIT 1",
      args: ["service_account", "bot"],
      index: "users_kind_name",
    },
    {
      name: "number of admins",
      sql: "SELECT COUNT(*) AS n FROM users WHERE role = 'admin' AND kind != ?",
      args: ["service_account"],
      index: "users_role_kind",
    },
    {
      name: "automation by ID (automationForRun)",
      sql: "SELECT factory_id, data FROM automations WHERE id = ?",
      args: ["aut_1"],
      index: "sqlite_autoindex_automations_1",
    },
  ])("$name", async ({ sql, args, index }) => {
    await bootstrap();
    const lines = await plan(sql, ...args);
    expect(lines.join("\n")).toContain(`USING`);
    expect(lines.join("\n")).toContain(index);
    expect(lines.some((l) => /^SCAN (factories|users|automations)\b/.test(l))).toBe(false);
  });
});

/** Runs a factories op on the default team with an admin principal. */
async function runOp(op: string, params: Record<string, string>, body?: unknown): Promise<unknown> {
  return runInDurableObject(env.TEAM.getByName("team:default"), async (instance) => {
    const kit = (instance as unknown as { kit: TeamKit }).kit;
    const fn = factories.ops[op]!;
    const c: OpCall = {
      op,
      meta: { requestId: "test", principal: admin },
      params,
      query: {},
      body,
      role: "",
      principal: () => admin,
      authorize: () => admin,
    };
    return fn(kit, c);
  });
}

const admin: Principal = {
  kind: "user",
  id: "usr_queries",
  teamId: "default",
  role: "admin",
  factoryRoles: {},
  userId: "usr_queries",
};

/** A compile result that differs from compile(files): proves the caller's result is used. */
const marker: CompileResult = {
  definition: null,
  errors: [{ file: "factory.yaml", field: "marker", message: "from the given compile result" }],
} as unknown as CompileResult;

describe("definition compiles", () => {
  it("checkDefinition and applyDefinition use a given compile result instead of compiling again", async () => {
    await bootstrap();
    const cr = (await runOp("createFactory", {}, { name: "Once " + uniq() })) as {
      $status: number;
      body: { factory: { id: string } };
    };
    expect(cr.$status).toBe(201);
    const fid = cr.body.factory.id;
    const files = ((await runOp("getDefinition", { uid: fid })) as { files: Record<string, string> }).files;
    expect(compile(files).errors).toEqual([]);
    const out = await runInDurableObject(env.TEAM.getByName("team:default"), async (instance) => {
      const kit = (instance as unknown as { kit: TeamKit }).kit;
      const f = getFactoryById(kit, fid)!;
      const chk = await checkDefinition(kit, f, files, marker);
      const r = await applyDefinition(kit, fid, { files, message: "marker", compiled: marker });
      return { chk: chk.errors, apply: r };
    });
    expect(out.chk.map((e) => e.field)).toEqual(["marker"]);
    expect(out.apply.invalid).toBe(true);
    expect(out.apply.version.errors?.map((e) => e.field)).toEqual(["marker"]);
  });

  it("reads the settings and the agent machines from the runtime, with the compiled values", async () => {
    await bootstrap();
    const cr = (await runOp(
      "createFactory",
      {},
      { name: "Settings " + uniq(), repositories: [{ owner: "acme", name: "app" }] },
    )) as { body: { factory: { id: string } } };
    const fid = cr.body.factory.id;
    const files = ((await runOp("getDefinition", { uid: fid })) as { files: Record<string, string> }).files;
    const want = compile(files).definition!;

    const st = (await runOp("getFactorySettings", { uid: fid })) as {
      credential_strategy: string;
      repositories: unknown[];
      machines: { name: string }[];
    };
    expect(st.credential_strategy).toBe(want.credentialStrategy || "EXECUTOR");
    expect(st.repositories).toEqual(JSON.parse(JSON.stringify(want.repositories ?? [])));
    expect(st.repositories.length).toBe(1);
    expect(st.machines.map((m) => m.name)).toEqual(Object.keys(want.machines ?? {}).sort());

    const agent = want.agents![0]!.name;
    const spec = (await runOp("getAgentSpec", { uid: fid, name: agent })) as { machines: string[] };
    expect(spec.machines).toEqual(Object.keys(want.machines ?? {}).sort());
  });
});
