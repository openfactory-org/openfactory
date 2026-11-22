// Planner and transfer tests. Ported cases from internal/engine
// harnessauth_test.go, credentials_test.go, runner_plan_test.go and
// internal/runnerapi transfer_test.go.
import { describe, expect, it } from "vitest";
import type { Run, RunSession } from "../engine/types";
import type { Env } from "../contracts/env";
import {
  applyRepoRefs,
  credentialScope,
  effectiveCredentialStrategy,
  harnessAuthEnv,
  headerSecrets,
  parseRepoSpec,
  Planner,
  redactList,
  secretsOf,
  splitCloneUrl,
  startConfigOf,
  type PlannerDeps,
} from "./planner";
import { objectPrefix, ownsKey, parseUploadId, uploadId, validateUpload } from "./transfer";
import { deriveRunnerKey } from "./token";
import { defaultStartConfig, deniedWithoutRequired } from "./startconfig";

const run = (extra: Partial<Run> = {}, cfg: Record<string, unknown> = {}): Run =>
  ({
    id: "run_wabcdefghijklmnoxyz",
    version: 1,
    team_id: "default",
    factory_id: "fac_1",
    work_item_id: "wi_wabcdefghijklmno1",
    agent_name: "foreman",
    name: "foreman",
    state: "claimed",
    prompt: "Fix it",
    consumed_seq: 0,
    usage: { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 },
    created_at: "2026-01-01T00:00:00Z",
    config: {
      id: "agt_1",
      factory_id: "fac_1",
      name: "foreman",
      description: "",
      type: "FOREMAN",
      instructions: "Do it.",
      harness: { type: "claude", model: "m1" },
      network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
      spawnable_by: null,
      ...cfg,
    },
    ...extra,
  }) as Run;

function deps(over: Partial<PlannerDeps> = {}): PlannerDeps & { recorded: unknown[] } {
  const recorded: unknown[] = [];
  const secrets: Record<string, string> = { "fac_1/API_TOKEN": "s3cr3t-value", "/CLAUDE_KEY": "sk-ant-team" };
  return {
    recorded,
    snapshot: async () => ({
      factory: { id: "fac_1" },
      runtime: { repositories: [{ codeForge: "GITHUB", owner: "acme", name: "api" }] },
      agents: [
        { name: "foreman", type: "FOREMAN" },
        { name: "coder", type: "IMPLEMENT", spawnable_by: ["foreman"] },
      ],
      integrations: { slack: {} },
    }),
    state: async () => ({ repoRefs: [] }),
    recordStartCommits: async (_r, l) => {
      recorded.push(l);
    },
    environment: async () => undefined,
    secret: async (fid, name) => secrets[`${fid}/${name}`],
    listSecrets: async (fid) =>
      fid
        ? [{ name: "API_TOKEN", type: "generic", factory_id: fid }]
        : [{ name: "CLAUDE_KEY", type: "anthropic_api_key" }],
    resolveManagedMcp: async () => ({ url: "http://cp/mcp/proxy/x", headers: { Authorization: "Bearer old" } }),
    forges: {
      forge: async () =>
        ({
          cloneURL: async (o: string, n: string) => `https://x-access-token:ghs_tok@github.com/${o}/${n}.git`,
        }) as never,
    },
    sandboxUrl: "http://cp",
    now: () => Date.parse("2026-01-01T00:00:00Z"),
    ...over,
  };
}

describe("planner helpers", () => {
  it("parseRepoSpec", () => {
    expect(parseRepoSpec("acme/api")).toEqual({ codeForge: "GITHUB", owner: "acme", name: "api" });
    expect(parseRepoSpec("gitlab:grp/sub/repo")).toEqual({ codeForge: "GITLAB", owner: "grp/sub", name: "repo" });
    expect(parseRepoSpec("https://dev.azure.com/org/proj/_git/repo.git")).toEqual({
      codeForge: "AZURE_DEVOPS",
      owner: "org/proj/_git",
      name: "repo",
    });
    expect(() => parseRepoSpec("nope")).toThrow(/invalid repository/);
  });

  it("harnessAuthEnv (HarnessAuthEnvOAuthToken)", () => {
    expect(harnessAuthEnv("claude", "", "anthropic_oauth_token", " tok\n")).toEqual({ CLAUDE_CODE_OAUTH_TOKEN: "tok" });
    expect(harnessAuthEnv("codex", "", "anthropic_oauth_token", "tok")).toEqual({});
    expect(harnessAuthEnv("codex", "", "generic", "k")).toEqual({ OPENAI_API_KEY: "k", CODEX_API_KEY: "k" });
    expect(harnessAuthEnv("open", "gpt-5", "generic", "k")).toEqual({ OPENAI_API_KEY: "k" });
  });

  it("credentials", () => {
    expect(effectiveCredentialStrategy("", "creator")).toBe("CREATOR");
    expect(effectiveCredentialStrategy("executor", "CREATOR")).toBe("EXECUTOR");
    expect(
      credentialScope(
        [{ codeForge: "github", owner: "a", name: "b" }],
        [{ codeForge: "GITHUB", owner: "A", repo: "B", ref: "x" }],
      ),
    ).toEqual({ GITHUB: ["a/b"] });
    expect(splitCloneUrl("https://u:p@h/x.git")).toEqual({ clean: "https://h/x.git", username: "u", password: "p" });
    expect(headerSecrets("Bearer abc")).toEqual(["Bearer abc", "abc"]);
    expect(redactList(["abc", "abcd", "abcdef", "abcd"])).toEqual(["abcdef", "abcd"]);
  });

  it("applyRepoRefs pins and adds", async () => {
    const repos = [{ forge: "GITHUB", owner: "acme", name: "api", path: "api", ref: "", cloneUrl: "u" }];
    await applyRepoRefs(
      repos,
      [
        { codeForge: "github", owner: "ACME", repo: "api", ref: "a".repeat(40) },
        { codeForge: "github", owner: "acme", repo: "web", ref: "b".repeat(40) },
      ],
      async () => "u2",
    );
    expect(repos[0]!.ref).toBe("a".repeat(40));
    expect(repos[1]).toMatchObject({ name: "web", cloneUrl: "u2", path: "web" });
  });
});

describe("planner", () => {
  it("plans a session: env, secrets, default credential, repositories, skills, MCP", async () => {
    const d = deps();
    const r = run(
      {},
      { secrets: ["API_TOKEN"], mcp_servers: { reg: { managedId: "slack-x" }, factory: { url: "http://evil" } } },
    );
    const p = await new Planner(d).plan(r);
    expect(p.env.OF_RUN_ID).toBe(r.id);
    expect(p.env.OF_PUBLIC_URL).toBe("http://cp");
    expect(p.env.API_TOKEN).toBeUndefined();
    expect(p.secretEnv).toEqual({ API_TOKEN: "s3cr3t-value" });
    expect(p.credential).toEqual({ source: "default", secret: "CLAUDE_KEY", scope: "team", type: "anthropic_api_key" });
    expect(p.harnessAuth).toEqual({ ANTHROPIC_API_KEY: "sk-ant-team" });
    expect(p.repos).toEqual([
      {
        forge: "GITHUB",
        owner: "acme",
        name: "api",
        path: "api",
        ref: "",
        cloneUrl: "https://x-access-token:ghs_tok@github.com/acme/api.git",
      },
    ]);
    expect(p.workDir).toBe("api");
    expect(p.skills.map((s) => s.name)).toEqual(expect.arrayContaining(["github", "chat"]));
    expect(p.systemPrompt).toContain("Do it.");
    expect(p.mcpServers.factory?.url).toBe(`http://cp/mcp/run/${r.id}`);
    expect(p.sessionTokenServers.has("factory")).toBe(true);
    expect(p.sessionTokenServers.has("reg")).toBe(true);
    expect(p.mcpServers.reg!.headers).toEqual({});
    expect(d.recorded).toHaveLength(1);

    const env = { OF_PUBLIC_URL: "http://cp", OF_R2_ACCOUNT_ID: "acct" } as unknown as Env;
    const sc = startConfigOf(env, r, { session: 1 } as RunSession, p);
    expect(sc.workspace.repositories).toEqual([
      { forge: "GITHUB", clone_url: "https://github.com/acme/api.git", path: "api", ref: "" },
    ]);
    expect(sc.harness.mcp_servers?.factory).toEqual({ url: `http://cp/mcp/run/${r.id}`, auth: "session_token" });
    expect(sc.env?.OF_WORKDIR).toBe("api");
    expect(sc.idle_timeout_seconds).toBe(600);
    expect(sc.workspace.skills?.find((s) => s.name === "github")?.files?.["SKILL.md"]).toBeTruthy();
    const sec = secretsOf(p);
    expect(sec.env).toEqual({ API_TOKEN: "s3cr3t-value" });
    expect(sec.harness_auth_default).toBe(true);
    expect(sec.redact).toEqual(["s3cr3t-value", "sk-ant-team"]);
  });

  it("a run environment credential wins; harness.auth workerEnvironment passes nothing", async () => {
    const d = deps({ snapshot: async () => ({ factory: { id: "fac_1" }, runtime: { repositories: [] }, agents: [] }) });
    const r1 = run({}, { secrets: ["ANTHROPIC_API_KEY"] });
    const d1 = { ...d, secret: async (_f: string, n: string) => (n === "ANTHROPIC_API_KEY" ? "own" : undefined) };
    expect((await new Planner(d1).plan(r1)).credential).toEqual({ source: "none", reason: "run_environment" });
    const r2 = run({}, { harness: { type: "claude", model: "m", auth: { source: "workerEnvironment" } } });
    expect((await new Planner(d).plan(r2)).credential).toEqual({ source: "workerEnvironment" });
  });

  it("two fitting team secrets are ambiguous", async () => {
    const d = deps({
      listSecrets: async (fid) =>
        fid
          ? []
          : [
              { name: "A", type: "anthropic_api_key" },
              { name: "B", type: "anthropic_oauth_token" },
            ],
    });
    expect(await new Planner(d).findDefaultCredential(run())).toEqual({
      source: "none",
      reason: "ambiguous",
      scope: "team",
      candidates: ["A", "B"],
    });
  });

  it("a missing secret is a configuration error", async () => {
    await expect(new Planner(deps()).plan(run({}, { secrets: ["NOPE"] }))).rejects.toMatchObject({
      code: "resource_not_found",
    });
  });

  it("git credential for a repository of the run, forbidden otherwise", async () => {
    const pl = new Planner(deps());
    const c = await pl.gitCredential(run(), "github.com", "/acme/api.git");
    expect(c).toEqual({ username: "x-access-token", password: "ghs_tok", expiresAt: "2026-01-01T00:50:00.000Z" });
    await expect(pl.gitCredential(run(), "github.com", "acme/other")).rejects.toMatchObject({ code: "not_authorized" });
  });
});

describe("denied hosts", () => {
  // Host names compare without case: a required host denied in another case stays allowed.
  it.each([
    { denied: null, required: ["cp"], want: [] },
    { denied: ["evil.example"], required: ["cp"], want: ["evil.example"] },
    { denied: ["CP", "Evil.Example"], required: ["cp"], want: ["Evil.Example"] },
    { denied: [" GitHub.com "], required: ["github.com"], want: [] },
  ])("deniedWithoutRequired($denied, $required)", ({ denied, required, want }) => {
    expect(deniedWithoutRequired(denied, required)).toEqual(want);
  });

  it("the start configurations drop a required host denied in another case", async () => {
    const env = { OF_PUBLIC_URL: "http://cp", OF_R2_ACCOUNT_ID: "acct" } as unknown as Env;
    const policy = { restricted: true, allowed_hosts: null, denied_hosts: ["CP", "evil.example"] };
    const r = run({}, { network_policy: policy });
    const planned = startConfigOf(env, r, { session: 1 } as RunSession, await new Planner(deps()).plan(r));
    expect(planned.network_policy?.denied_hosts).toEqual(["evil.example"]);
    expect(planned.network_policy?.always_allow).toContain("cp");
    const minimal = await defaultStartConfig(env, r, { session: 1 } as RunSession);
    expect(minimal.network_policy?.denied_hosts).toEqual(["evil.example"]);
  });
});

describe("transfer", () => {
  it("upload IDs carry the key and its MAC; old keys verify", async () => {
    const k1 = await deriveRunnerKey("one");
    const k2 = await deriveRunnerKey("two");
    const id = await uploadId([k1], "runs/r/sessions/1/artifact/a.txt");
    expect(await parseUploadId([k2, k1], id)).toBe("runs/r/sessions/1/artifact/a.txt");
    expect(await parseUploadId([k2], id)).toBeUndefined();
    expect(await parseUploadId([k1], id.slice(0, -2) + "xx")).toBeUndefined();
  });

  it("object prefixes and validation", () => {
    const r = { id: "run_x", team_id: "" };
    expect(objectPrefix(r, 2, "snapshot", "workspace")).toBe("snapshots/default/run_x/2/workspace/");
    expect(objectPrefix(r, 2, "artifact")).toBe("runs/run_x/sessions/2/artifact/");
    expect(ownsKey(r, "snapshots/default/run_x/1/x")).toBe(true);
    expect(ownsKey(r, "runs/run_y/x")).toBe(false);
    const sha = "0".repeat(64);
    expect(validateUpload({ kind: "artifact", name: "a/../b", bytes: 1, sha256: sha })?.message).toBe(
      "name has a .. path element",
    );
    expect(validateUpload({ kind: "snapshot", name: "a", bytes: 1, sha256: sha })?.code).toBe("bad_request");
    expect(validateUpload({ kind: "artifact", name: "a", bytes: 200 << 20, sha256: sha })?.status).toBe(413);
    expect(validateUpload({ kind: "transcript", name: "t.jsonl", bytes: 10, sha256: sha })).toBeUndefined();
  });
});
