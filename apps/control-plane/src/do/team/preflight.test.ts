// Definition preflight against the team, through TeamDO.api on a fresh team
// per test. Ports of internal/engine/preflight_test.go,
// preflight_principals_test.go, preflight_refs_test.go,
// definition_runtime_test.go and internal/api/definition_preflight_test.go.
//
// Not ported: TestNameFiltersResolveToIDs and TestJiraAndTeamsNameFilters-
// ResolveToIDs (filter names are not resolved at apply: the lookups need
// network calls that are not wired), TestApplyIsOneTransaction (the apply is
// one transactionSync by construction), the sa-as-user runAs case (the
// compiler now rejects "user:<name>" without an email), and
// TestSourceChangeToGitChecksTheBranch / TestCreateWithGitSourceChecksTheBranch
// (forges/defsync.ts checkBranch is not wired to the factory routes).
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CallMeta, Principal } from "../../contracts/do-messages";
import { knownModel, webhookFilterIds } from "./preflight";

interface Issue {
  file: string;
  field?: string;
  line?: number;
  message: string;
}

// Loose JSON of the op results.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Loose = any;

const MODEL = "claude-opus-5-5";

/** One fresh team: its TeamDO, an admin principal and op helpers. */
function team() {
  const id = "pf" + crypto.randomUUID().slice(0, 8);
  const admin: Principal = {
    kind: "user",
    id: "usr_admin",
    teamId: id,
    role: "admin",
    factoryRoles: {},
    userId: "usr_admin",
  };
  const meta: CallMeta = { requestId: "req_test", principal: admin };
  const stub = env.TEAM.getByName(doName.team(id));
  /** Calls an op; returns the body, or the $error object. */
  const raw = async (name: string, params: Record<string, string> = {}, body: unknown = null): Promise<Loose> => {
    const out = (await stub.api(meta, name, params, body)) as Loose;
    if (out && typeof out === "object" && "$status" in out) return out.body;
    return out;
  };
  const op = async (name: string, params: Record<string, string> = {}, body: unknown = null): Promise<Loose> => {
    const out = await raw(name, params, body);
    if (out && typeof out === "object" && "$error" in out) throw new Error(`${name}: ${JSON.stringify(out.$error)}`);
    return out;
  };
  const sql = async (q: string, ...args: unknown[]) =>
    runInDurableObject(stub, async (_i, state) => {
      state.storage.sql.exec(q, ...args);
    });
  return { id, stub, raw, op, sql };
}

type Team = ReturnType<typeof team>;

const agentMd = (type: string, front = `model: ${MODEL}\n`) =>
  `---\ndescription: d\nagentType: ${type}\n${front}---\nGo.\n`;
const schedule = `triggers:\n  - provider: schedule\n    event: cron_fired\n    schedule:\n      cron: "@daily"\n`;
const automationMd = (front: string) => `---\n${front}${schedule}---\nGo.\n`;

/** A factory and the base files of its tests: foreman, triage and review. */
async function factory(t: Team, factoryYaml = ""): Promise<{ id: string; files: Record<string, string> }> {
  const f = await t.op("createFactory", {}, { name: "Payments " + crypto.randomUUID().slice(0, 6) });
  const def = await t.op("getDefinition", { uid: f.factory.id });
  const files: Record<string, string> = {
    "factory.yaml": (def.files["factory.yaml"] as string) + factoryYaml,
    "agents/foreman/agent.md": agentMd("FOREMAN"),
    "agents/triage/agent.md": agentMd("TRIAGE"),
    "agents/review/agent.md": agentMd("REVIEW"),
  };
  return { id: f.factory.id as string, files };
}

async function validate(t: Team, fid: string, files: Record<string, string>): Promise<Loose> {
  return t.op("validateDefinition", { uid: fid }, { files });
}

const has = (list: Issue[], file: string, field: string, text: string) =>
  list.some((e) => e.file === file && (field === "" || e.field === field) && e.message.includes(text));

describe("definition preflight (engine/preflight_test.go)", () => {
  it("checks secrets, environments, worker hosts, MCP servers and url_token secrets against the team", async () => {
    const t = team();
    const { id, files } = await factory(t);
    await t.op("createSecret", {}, { name: "BAD_TOKEN", value: "a/b" });
    await t.op("createSecret", {}, { name: "GOOD_TOKEN", value: "good-token_1" });
    const env0 = await t.op("createEnvironment", {}, { name: "ok" });
    const bad = {
      ...files,
      "agents/triage/agent.md": agentMd(
        "TRIAGE",
        `model: ${MODEL}\nsecrets: [MISSING]\nenvironmentId: env_gone\nworkerHost: build-box\nmcpServers:\n  gone:\n    managedId: mcp_gone\n`,
      ),
      "agents/review/agent.md": agentMd("REVIEW", `model: ${MODEL}\nenvironmentId: ${env0.id}\n`),
      "webhooks/deploys.yaml": "authMode: url_token\nsecretName: BAD_TOKEN\n",
    };
    const chk = await validate(t, id, bad);
    expect(chk.valid).toBe(false);
    for (const [file, field, text] of [
      ["agents/triage/agent.md", "secrets", 'secret "MISSING"'],
      ["agents/triage/agent.md", "environmentId", 'environment "env_gone"'],
      ["agents/triage/agent.md", "workerHost", 'worker host "build-box"'],
      ["agents/triage/agent.md", "mcpServers.gone.managedId", '"mcp_gone"'],
      ["webhooks/deploys.yaml", "secretName", "cannot be used in a URL"],
    ] as const) {
      expect(has(chk.errors, file, field, text), `${file} ${field}: ${JSON.stringify(chk.errors)}`).toBe(true);
    }
    expect(chk.errors).toHaveLength(5);

    const good = {
      ...bad,
      "agents/triage/agent.md": agentMd("TRIAGE"),
      "webhooks/deploys.yaml": "authMode: url_token\nsecretName: GOOD_TOKEN\n",
    };
    const ok = await validate(t, id, good);
    expect(ok.errors).toEqual([]);
    expect(ok.valid).toBe(true);
  });

  it("an apply with a missing secret stores an invalid version and keeps the active one", async () => {
    const t = team();
    const { id, files } = await factory(t);
    const before = (await t.op("getFactory", { uid: id })).active_version_id;
    const out = await t.raw(
      "putDefinition",
      { uid: id },
      { files: { ...files, "agents/foreman/agent.md": agentMd("FOREMAN", `model: ${MODEL}\nsecrets: [NOT_THERE]\n`) } },
    );
    expect(out.$error.code).toBe("invalid_request");
    expect(has(out.$error.extra.errors, "agents/foreman/agent.md", "secrets", "NOT_THERE")).toBe(true);
    expect((await t.op("getFactory", { uid: id })).active_version_id).toBe(before);
  });

  it("webhooks keep their UID on re-apply, refuse auth changes and get a new UID when added again", async () => {
    const t = team();
    const { id, files } = await factory(t);
    await t.op("createSecret", {}, { name: "HOOK", value: "tok" });
    const withHook = (yaml: string | undefined) => {
      const f: Record<string, string> = { ...files };
      if (yaml !== undefined) f["webhooks/deploys.yaml"] = yaml;
      return f;
    };
    const apply = (f: Record<string, string>) => t.raw("putDefinition", { uid: id }, { files: f });
    const hook = async () => {
      const ws = (await t.op("listWebhooks", { uid: id })).items as { id: string; created_at: string }[];
      expect(ws).toHaveLength(1);
      return ws[0]!;
    };
    expect((await apply(withHook("authMode: token\nsecretName: HOOK\n"))).$error).toBeUndefined();
    const first = await hook();
    expect(first.id).toMatch(/^whk_/);
    expect(first.created_at).toBeTruthy();
    expect((await apply(withHook("authMode: token\nsecretName: HOOK\nenabled: false\n"))).$error).toBeUndefined();
    const again = await hook();
    expect(again.id).toBe(first.id);
    expect(again.created_at).toBe(first.created_at);

    const changed = await apply(
      withHook("authMode: signature\nsignatureScheme: standard_webhooks\nsecretName: HOOK\n"),
    );
    expect(changed.$error.code).toBe("invalid_request");
    const errs = changed.$error.extra.errors as Issue[];
    expect(has(errs, "webhooks/deploys.yaml", "authMode", "cannot change")).toBe(true);
    expect(has(errs, "webhooks/deploys.yaml", "signatureScheme", "cannot change")).toBe(true);

    // Removed and added again: a new UID and so a new URL.
    expect((await apply(withHook(undefined))).$error).toBeUndefined();
    expect((await t.op("listWebhooks", { uid: id })).items).toEqual([]);
    expect(
      (await apply(withHook("authMode: signature\nsignatureScheme: standard_webhooks\nsecretName: HOOK\n"))).$error,
    ).toBeUndefined();
    expect((await hook()).id).not.toBe(first.id);
  });
});

describe("environment keys and webhook filters (engine/preflight_refs_test.go)", () => {
  it("names the real key and line, and puts an inherited environment in factory.yaml", async () => {
    const t = team();
    const { id, files } = await factory(t);
    const f = {
      ...files,
      "factory.yaml": files["factory.yaml"]!.replace(
        "agentDefaults:\n",
        "agentDefaults:\n    environmentId: env_def\n",
      ),
      "agents/foreman/agent.md": `---\ndescription: d\nagentType: FOREMAN\nenvironmentId: env_x\nmodel: ${MODEL}\n---\nGo.\n`,
      "automations/issue-intake/automation.md": automationMd("environmentId: env_auto\n"),
    };
    const chk = await validate(t, id, f);
    const defLine = f["factory.yaml"].split("\n").indexOf("    environmentId: env_def") + 1;
    expect(chk.errors).toEqual([
      {
        file: "agents/foreman/agent.md",
        line: 4,
        field: "environmentId",
        message: 'environment "env_x" does not exist in the team',
      },
      {
        file: "automations/issue-intake/automation.md",
        line: 2,
        field: "environmentId",
        message: 'environment "env_auto" does not exist in the team',
      },
      {
        file: "factory.yaml",
        line: defLine,
        field: "agentDefaults.environmentId",
        message: 'environment "env_def" does not exist in the team',
      },
    ]);
  });

  it("a webhook_ids UID must belong to a webhook that the definition keeps", async () => {
    const t = team();
    const { id, files } = await factory(t);
    await t.op("createSecret", {}, { name: "HOOK", value: "tok" });
    const hookFile = "authMode: token\nsecretName: HOOK\n";
    await t.op("putDefinition", { uid: id }, { files: { ...files, "webhooks/deploys.yaml": hookFile } });
    const uid = (await t.op("listWebhooks", { uid: id })).items[0].id as string;
    const auto = (filter: string) =>
      `---\ntriggers:\n  - provider: webhook\n    event: received\n    filter:\n      webhook_ids: ${filter}\n---\nGo.\n`;
    for (const tc of [
      { name: "stored uid", filter: `["${uid}"]`, hook: true, want: "" },
      { name: "stored uid in", filter: `{in: ["${uid}"]}`, hook: true, want: "" },
      { name: "name", filter: `[deploys]`, hook: true, want: "" },
      {
        name: "unknown uid",
        filter: `[whk_nope]`,
        hook: true,
        want: 'webhook "whk_nope" does not exist; known: deploys',
      },
      { name: "uid of a removed webhook", filter: `["${uid}"]`, hook: false, want: "known: none" },
    ]) {
      const f: Record<string, string> = { ...files, "automations/hooked/automation.md": auto(tc.filter) };
      if (tc.hook) f["webhooks/deploys.yaml"] = hookFile;
      const chk = await validate(t, id, f);
      if (tc.want === "") {
        expect(chk.errors, tc.name).toEqual([]);
        continue;
      }
      const field = tc.filter.startsWith("{")
        ? "triggers[0].filter.webhook_ids.in[0]"
        : "triggers[0].filter.webhook_ids[0]";
      const e = (chk.errors as Issue[]).find((x) => x.field === field);
      expect(e?.message, `${tc.name}: ${JSON.stringify(chk.errors)}`).toContain(tc.want);
      expect(e?.line, tc.name).toBe(6);
    }
  });

  it("webhookFilterIds reads a list or an in matcher", () => {
    expect(webhookFilterIds({ webhook_ids: ["a"] })).toEqual({ field: "webhook_ids", ids: ["a"] });
    expect(webhookFilterIds({ webhook_ids: { in: ["b"] } })).toEqual({ field: "webhook_ids.in", ids: ["b"] });
    expect(webhookFilterIds('{"webhook_ids":["c"]}')).toEqual({ field: "webhook_ids", ids: ["c"] });
    expect(webhookFilterIds({ payload: {} })).toEqual({ field: "", ids: [] });
    expect(webhookFilterIds("not json")).toEqual({ field: "", ids: [] });
  });
});

/** Users of the principal tests: dana (linked forge account), erin (none) and the service account ci. */
async function principals(t: Team): Promise<{ dana: string; erin: string; ci: string }> {
  const dana = (await t.op("createUser", {}, { email: "dana@example.com", name: "Dana" })).user.id as string;
  const erin = (await t.op("createUser", {}, { email: "erin@example.com", name: "Erin" })).user.id as string;
  const ci = (await t.op("createServiceAccount", {}, { name: "ci", role: "editor" })).service_account.id as string;
  await t.sql(
    "INSERT INTO user_forge_tokens (user_id, provider, envelope, updated_ms) VALUES (?, 'github', ?, 0)",
    dana,
    new Uint8Array([0]),
  );
  return { dana, erin, ci };
}

describe("principals, models and credentials (engine/preflight_principals_test.go)", () => {
  it("each runAs names a principal of the team", async () => {
    const t = team();
    await principals(t);
    const { id, files } = await factory(t);
    const f = { ...files };
    for (const [name, runAs] of [
      ["ok-user", "user:dana@example.com"],
      ["ok-user-case", "user:Dana@Example.com"],
      ["ok-sa", "serviceAccount:ci"],
      ["ok-factory", "factory"],
      ["none", ""],
      ["no-user", "user:ghost@example.com"],
      ["no-sa", "serviceAccount:ghost"],
    ])
      f[`automations/${name}/automation.md`] = automationMd(runAs ? `runAs: "${runAs}"\n` : "");
    const chk = await validate(t, id, f);
    expect(chk.valid).toBe(false);
    for (const name of ["no-user", "no-sa"])
      expect(has(chk.errors, `automations/${name}/automation.md`, "runAs", "names no principal of the team")).toBe(
        true,
      );
    expect((chk.errors as Issue[]).map((e) => e.file).sort()).toEqual([
      "automations/no-sa/automation.md",
      "automations/no-user/automation.md",
    ]);
  });

  it("warns about a CREATOR runAs person without a linked forge account", async () => {
    const t = team();
    await principals(t);
    const { id, files } = await factory(t, "credentialStrategy: CREATOR\n");
    const f = {
      ...files,
      "factory.yaml": files["factory.yaml"]!.replace(
        "repositories: []\n",
        "repositories:\n    - owner: acme\n      name: api\n",
      ),
      "automations/as-dana/automation.md": automationMd('runAs: "user:dana@example.com"\n'),
      "automations/as-erin/automation.md": automationMd('runAs: "user:erin@example.com"\n'),
      "automations/as-ci/automation.md": automationMd('runAs: "serviceAccount:ci"\n'),
    };
    const chk = await validate(t, id, f);
    expect(chk.errors).toEqual([]);
    expect(has(chk.warnings, "automations/as-erin/automation.md", "runAs", "no linked forge account")).toBe(true);
    for (const n of ["as-dana", "as-ci"])
      expect(has(chk.warnings, `automations/${n}/automation.md`, "runAs", "")).toBe(false);

    // With the strategy EXECUTOR the link does not matter.
    f["factory.yaml"] = f["factory.yaml"].replace("credentialStrategy: CREATOR\n", "credentialStrategy: EXECUTOR\n");
    const exec = await validate(t, id, f);
    expect(has(exec.warnings, "automations/as-erin/automation.md", "runAs", "")).toBe(false);
  });

  it("approvers are team principals; an unknown one stops the apply", async () => {
    const t = team();
    await principals(t);
    const { id, files } = await factory(t);
    const withApprovers = (list: string[]) => ({
      ...files,
      "factory.yaml": files["factory.yaml"] + "approvers:\n" + list.map((a) => `  - ${JSON.stringify(a)}\n`).join(""),
    });
    const chk = await validate(t, id, withApprovers(["Dana@Example.com", "ci", "ghost@example.com", "erin"]));
    expect(chk.valid).toBe(false);
    // erin is a person: only her email matches.
    for (const bad of ["ghost@example.com", "erin"])
      expect(has(chk.errors, "factory.yaml", "approvers", `"${bad}"`)).toBe(true);
    for (const good of ['"Dana@Example.com"', '"ci"'])
      expect(has(chk.errors, "factory.yaml", "approvers", good)).toBe(false);
    expect((await validate(t, id, withApprovers(["dana@example.com", "ci"]))).valid).toBe(true);
    const before = (await t.op("getFactory", { uid: id })).active_version_id;
    const out = await t.raw("putDefinition", { uid: id }, { files: withApprovers(["ghost@example.com"]) });
    expect(out.$error.code).toBe("invalid_request");
    expect((await t.op("getFactory", { uid: id })).active_version_id).toBe(before);
  });

  it("warns about models that are not in the model list of their harness", async () => {
    const t = team();
    const { id, files } = await factory(t);
    const f = {
      ...files,
      "agents/foreman/agent.md": agentMd("FOREMAN", "model: claude-sonnet-5-5\n"),
      "agents/triage/agent.md": agentMd("TRIAGE", "model: claude-sonet-5\n"),
      "agents/review/agent.md": agentMd("REVIEW", "harness:\n  type: claude\n  model: sonet\n"),
      "agents/spec/agent.md": agentMd("SPEC", "model: claude-sonnet-5-5-20260101\n"),
      "agents/implement/agent.md": agentMd("IMPLEMENT", "harness:\n  type: codex\n  model: gpt-5.3-codex\n"),
      "automations/tuned/automation.md": automationMd("agent: spec\nmodel: claude-haiku-x\n"),
    };
    const chk = await validate(t, id, f);
    expect(chk.errors).toEqual([]);
    for (const [file, field, text] of [
      ["agents/triage/agent.md", "model", '"claude-sonet-5"'],
      ["agents/review/agent.md", "harness.model", '"sonet"'],
      ["automations/tuned/automation.md", "model", '"claude-haiku-x"'],
    ] as const)
      expect(has(chk.warnings, file, field, text), `${file}: ${JSON.stringify(chk.warnings)}`).toBe(true);
    for (const file of ["agents/spec/agent.md", "agents/implement/agent.md", "agents/foreman/agent.md"])
      expect(
        has(
          chk.warnings.filter((w: Issue) => w.field !== "harness.auth"),
          file,
          "",
          "",
        ),
      ).toBe(false);
    expect(knownModel("claude", "claude-sonnet-5-5[1m]")).toBe(true);
    expect(knownModel("unknown-harness", "anything")).toBe(true);
  });

  // An agent without harness.auth gets a warning unless exactly one secret
  // fits as its default credential. An agent with harness.auth gets none.
  it.each([
    {
      name: "no secret",
      secrets: [],
      want: "has no credential; add a secret of type anthropic_api_key or anthropic_oauth_token",
    },
    { name: "generic secret", secrets: [{ name: "ANTHROPIC_API_KEY", type: "generic" }], want: "has no credential" },
    { name: "one team secret", secrets: [{ name: "CLAUDE_OAUTH_TOKEN", type: "anthropic_oauth_token" }], want: "" },
    {
      name: "two team secrets",
      secrets: [
        { name: "B", type: "anthropic_oauth_token" },
        { name: "A", type: "anthropic_api_key" },
      ],
      want: "more than one team secret fits (A, B)",
    },
    {
      name: "agent secret holds the key",
      secrets: [{ name: "ANTHROPIC_API_KEY", type: "generic" }],
      agentSecrets: ["ANTHROPIC_API_KEY"],
      want: "",
    },
  ])("credential warning: $name", async ({ secrets, agentSecrets, want }) => {
    const t = team();
    await t.op("createSecret", {}, { name: "MODEL_KEY", value: "sk-test-fake" });
    for (const s of secrets) await t.op("createSecret", {}, { name: s.name, type: s.type, value: "sk-test-fake" });
    const { id, files } = await factory(t);
    const auth = `harness:\n  type: claude\n  model: ${MODEL}\n  auth:\n    source: managedSecret\n    secretName: MODEL_KEY\n`;
    const f = {
      ...files,
      "agents/foreman/agent.md": agentMd(
        "FOREMAN",
        `model: ${MODEL}\n` + (agentSecrets ? `secrets: [${agentSecrets.join(", ")}]\n` : ""),
      ),
      "agents/triage/agent.md": agentMd("TRIAGE", auth),
      "agents/review/agent.md": agentMd("REVIEW", auth),
    };
    const chk = await validate(t, id, f);
    expect(chk.errors).toEqual([]);
    if (want === "") expect(has(chk.warnings, "agents/foreman/agent.md", "harness.auth", "")).toBe(false);
    else
      expect(has(chk.warnings, "agents/foreman/agent.md", "harness.auth", want), JSON.stringify(chk.warnings)).toBe(
        true,
      );
    for (const a of ["triage", "review"])
      expect(has(chk.warnings, `agents/${a}/agent.md`, "harness.auth", "")).toBe(false);
  });
});

describe("workerEnvironment auth targets development process workers", () => {
  it("refuses microVM workers, pools with a microVM worker and sandbox providers", async () => {
    const t = team();
    const { id, files } = await factory(t);
    const insert = (wid: string, kind: string, labels: string) =>
      t.sql(
        "INSERT INTO workers (id, name, kind, conn, labels, created_ms, updated_ms) VALUES (?, ?, ?, 'online', ?, 0, 0)",
        wid,
        wid,
        kind,
        labels,
      );
    await insert("dev-box", "process", '{"dev":"1"}');
    await insert("vm-host", "microvm", '{"mixed":"1"}');
    await insert("dev-box-2", "process", '{"mixed":"1"}');
    const workerEnv = (host: string) =>
      `harness:\n  type: claude\n  model: ${MODEL}\n  auth:\n    source: workerEnvironment\nworkerHost: ${host}\n`;
    const f = {
      ...files,
      "agents/foreman/agent.md": agentMd("FOREMAN", workerEnv("dev-box")),
      "agents/triage/agent.md": agentMd("TRIAGE", workerEnv("vm-host")),
      "agents/review/agent.md": agentMd("REVIEW", workerEnv("pool:mixed")),
      "agents/spec/agent.md": agentMd("SPEC", workerEnv("pool:dev")),
      "agents/implement/agent.md": agentMd("IMPLEMENT", workerEnv("daytona")),
      "automations/on-vm/automation.md": automationMd("agent: foreman\nworkerHost: vm-host\n"),
    };
    const chk = await validate(t, id, f);
    const got = (chk.errors as Issue[]).map((e) => `${e.file}|${e.field}`).sort();
    expect(got, JSON.stringify(chk.errors)).toEqual([
      "agents/implement/agent.md|harness.auth.source",
      "agents/review/agent.md|harness.auth.source",
      "agents/triage/agent.md|harness.auth.source",
      "automations/on-vm/automation.md|workerHost",
    ]);
    expect(has(chk.errors, "agents/triage/agent.md", "", "runs microVM workers (vm-host)")).toBe(true);
    expect(has(chk.errors, "agents/implement/agent.md", "", 'workerHost "daytona" is a sandbox provider')).toBe(true);

    // Process workers only: valid.
    const ok = await validate(t, id, {
      ...files,
      "agents/foreman/agent.md": agentMd("FOREMAN", workerEnv("dev-box")),
      "agents/triage/agent.md": agentMd("TRIAGE", workerEnv("pool:dev")),
    });
    expect(ok.errors).toEqual([]);
  });
});

describe("the validate route (api/definition_preflight_test.go)", () => {
  it("returns warnings, skipped checks and a summary when valid, and no summary when invalid", async () => {
    const t = team();
    const { id, files } = await factory(t);
    const v = await validate(t, id, {
      ...files,
      "agents/triage/agent.md": agentMd("TRIAGE", "model: claude-sonet-5\n"),
    });
    expect(v.valid).toBe(true);
    expect(v.warnings.length).toBeGreaterThan(0);
    expect(Array.isArray(v.skipped)).toBe(true);
    expect(v.summary.agents).toEqual(["foreman", "review", "triage"]);
    const bad = await validate(t, id, { ...files, "factory.yaml": files["factory.yaml"] + "secrets: [DB_URL]\n" });
    expect(bad.valid).toBe(false);
    expect(bad.errors).toHaveLength(1);
    expect(bad.summary).toBeUndefined();
  });
});

describe("sender settings on the runtime (engine/definition_runtime_test.go)", () => {
  it("stores approvers, restrictFollowupsToStarter and requireLinkedActor, and keeps them on a failed apply", async () => {
    const t = team();
    await t.op("createUser", {}, { email: "ada@example.test", role: "user" });
    await t.op("createServiceAccount", {}, { name: "ci", role: "editor" });
    const { id, files } = await factory(t);
    const apply = async (extra: string) => {
      const out = await t.raw(
        "putDefinition",
        { uid: id },
        { files: { ...files, "factory.yaml": files["factory.yaml"] + extra } },
      );
      const snap = (await t.stub.configSnapshot(id)) as Loose;
      return { err: out?.$error, rt: snap.runtime };
    };
    let r = await apply("");
    expect(r.err).toBeUndefined();
    expect(r.rt.approvers ?? []).toEqual([]);
    expect(!!r.rt.restrict_followups_to_starter || !!r.rt.require_linked_actor).toBe(false);

    r = await apply(
      "approvers:\n  - ada@example.test\n  - ci\nrestrictFollowupsToStarter: true\nrequireLinkedActor: true\n",
    );
    expect(r.err).toBeUndefined();
    expect(r.rt.approvers).toEqual(["ada@example.test", "ci"]);
    expect(r.rt.restrict_followups_to_starter).toBe(true);
    expect(r.rt.require_linked_actor).toBe(true);

    // The settings follow the active version: an apply without them clears them.
    r = await apply("");
    expect(r.rt.approvers ?? []).toEqual([]);
    expect(!!r.rt.restrict_followups_to_starter || !!r.rt.require_linked_actor).toBe(false);

    // An approver who is not in the team stops the apply. The runtime stays.
    r = await apply("approvers:\n  - ada@example.test\nrequireLinkedActor: true\n");
    expect(r.rt.require_linked_actor).toBe(true);
    r = await apply("approvers:\n  - nobody@example.test\n");
    expect(r.err?.code).toBe("invalid_request");
    expect(r.rt.approvers).toEqual(["ada@example.test"]);
    expect(r.rt.require_linked_actor).toBe(true);
  });
});
