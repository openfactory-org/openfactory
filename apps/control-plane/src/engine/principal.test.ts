// Run principals. Ports of internal/engine/principal_test.go
// (TestRunPrincipals, TestRunAsThatDoesNotExistFails) and
// TestApproversMatchServiceAccountNames of preflight_principals_test.go: the
// pure rules, and the runs that intake (real RouterDO and WorkDO) and the
// TeamDO resolveRunAs op create.
//
// Not ported here: TestCreatorWithoutLink. The CREATOR credential check runs
// in the runner API planner at session start (runner-api/planner.test.ts);
// the creator link warning of the preflight is in do/team/preflight.test.ts.
import { env } from "cloudflare:workers";
import { doName, newToken } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CoreEvent } from "../contracts/do-messages";
import type { Automation } from "@openfactory/core";
import type { AgentConfig, Run } from "./types";
import type { WorkDO } from "../do/work/index";
import { handleEvent, platformIntakeDeps, type IntakeDeps } from "../do/ingress/intake";
import { bootstrap, call } from "../../test/helpers";
import { callRoute } from "../mcp/server/backend";
import {
  callerPrincipal,
  dispatchPrincipals,
  isApprover,
  platformTrigger,
  resolveRunAs,
  runPrincipals,
  type PrincipalUser,
} from "./principal";

const TEAM = "team1";
const dana: PrincipalUser = { id: "usr_dana", team_id: TEAM, email: "dana@example.com", name: "Dana" };
const erin: PrincipalUser = { id: "usr_erin", team_id: TEAM, email: "erin@example.com", name: "Erin" };
const ci: PrincipalUser = { id: "sa_ci", team_id: TEAM, name: "ci", kind: "service_account" };
const ghost: PrincipalUser = { id: "usr_ghost", team_id: "other", email: "ghost@example.com" };
const users = [dana, erin, ci];
const F = "fac_1";

describe("run principals (engine/principal.go)", () => {
  it("gives each kind of run its creator and run_as", () => {
    const user = (id: string) => ({ type: "user", id });
    const sa = (id: string) => ({ type: "service_account", id });
    const factory = { type: "factory", id: F };
    for (const tc of [
      {
        name: "person through the API",
        in: { creator: dana, triggeredBy: "api" },
        creator: user("usr_dana"),
        runAs: user("usr_dana"),
      },
      {
        name: "service account key",
        in: { creator: ci, triggeredBy: "api" },
        creator: sa("sa_ci"),
        runAs: sa("sa_ci"),
      },
      {
        name: "mention from a linked account",
        in: { creator: erin, triggeredBy: "mention" },
        creator: user("usr_erin"),
        runAs: user("usr_erin"),
      },
      {
        name: "automation without runAs",
        in: { creator: erin, triggeredBy: "automation:x", automation: true },
        creator: user("usr_erin"),
        runAs: factory,
      },
      { name: "schedule", in: { triggeredBy: "schedule:x", automation: true }, creator: undefined, runAs: factory },
      {
        name: "automation runs as a user",
        in: { triggeredBy: "automation:x", automationRunAs: resolveRunAs(users, F, "user:dana@example.com") },
        creator: undefined,
        runAs: user("usr_dana"),
      },
      {
        name: "automation runs as a service account",
        in: {
          creator: erin,
          triggeredBy: "automation:x",
          automationRunAs: resolveRunAs(users, F, "serviceAccount:ci"),
        },
        creator: user("usr_erin"),
        runAs: sa("sa_ci"),
      },
      { name: "benchmark", in: { creator: dana, triggeredBy: "benchmark:b1" }, creator: undefined, runAs: factory },
      { name: "scorer", in: { creator: dana, triggeredBy: "scorer:s1" }, creator: undefined, runAs: factory },
      { name: "self-improvement", in: { triggeredBy: "self_improvement" }, creator: undefined, runAs: factory },
      {
        name: "creator of another team",
        in: { creator: ghost, triggeredBy: "api" },
        creator: undefined,
        runAs: factory,
      },
    ]) {
      const got = dispatchPrincipals({ factoryId: F, teamId: TEAM, ...tc.in });
      expect(got.creator, tc.name).toEqual(tc.creator);
      expect(got.runAs, tc.name).toEqual(tc.runAs);
    }
  });

  it("a runAs that names no principal fails; the engine never falls back to the factory", () => {
    expect(resolveRunAs(users, F, "")).toEqual({ type: "factory", id: F });
    expect(resolveRunAs(users, F, "factory")).toEqual({ type: "factory", id: F });
    expect(resolveRunAs(users, F, "user:Dana@Example.com")).toEqual({ type: "user", id: "usr_dana" });
    for (const v of ["user:ghost@example.com", "serviceAccount:ghost", "serviceAccount:Dana", "user:x"]) {
      expect(() => resolveRunAs(users, F, v), v).toThrow(expect.objectContaining({ code: "invalid_request" }));
    }
    // ci is a service account: it has no email, and a person never matches by name.
    expect(() => resolveRunAs([{ ...ci, email: "ci@example.com" }], F, "user:ci@example.com")).toThrow();
  });

  it("a new run gets the factory as run_as when it names none, and a platform run drops its creator", () => {
    const u = { type: "user" as const, id: "usr_dana" };
    expect(runPrincipals(F, "api", u, u)).toEqual({ creator: u, runAs: u });
    expect(runPrincipals(F, "schedule:x", undefined, undefined)).toEqual({ runAs: { type: "factory", id: F } });
    expect(runPrincipals(F, "benchmark:b1", u, u)).toEqual({ runAs: { type: "factory", id: F } });
    expect(platformTrigger("self_improvement")).toBe(true);
    expect(platformTrigger("run:run_1")).toBe(false);
  });

  it("maps API callers to principals", () => {
    expect(callerPrincipal({ kind: "user", userId: "usr_a" })).toEqual({ type: "user", id: "usr_a" });
    expect(callerPrincipal({ kind: "service_account", userId: "sa_a" })).toEqual({
      type: "service_account",
      id: "sa_a",
    });
    expect(callerPrincipal({ kind: "factory_mcp", userId: "usr_a" })).toEqual({ type: "user", id: "usr_a" });
    expect(callerPrincipal({ kind: "run", userId: "x" })).toBeUndefined();
    expect(callerPrincipal(undefined)).toBeUndefined();
  });

  it("approvers match a person by email and a service account by name (TestApproversMatchServiceAccountNames)", () => {
    const person: PrincipalUser = { id: "usr_x", name: "ci", email: "x@example.com" };
    expect(isApprover(["ci"], ci)).toBe(true);
    expect(isApprover(["ci"], person)).toBe(false);
    expect(isApprover(["X@example.com"], person)).toBe(true);
  });
});

// ---------- runs created by intake ----------

const foreman: AgentConfig = {
  id: "agt_foreman",
  factory_id: "",
  name: "foreman",
  description: "",
  type: "FOREMAN",
  instructions: "lead",
  harness: { type: "claude", model: "m" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
};

function automation(fid: string, id: string, runAs: string): Automation {
  return {
    id,
    factory_id: fid,
    name: id,
    enabled: true,
    agent: "foreman",
    prompt: "Handle the issue.",
    triggers: [{ provider: "github", event: "issue_created" }],
    ...(runAs ? { run_as: runAs } : {}),
  };
}

function deps(team: string, fid: string, a: Automation, extra: Partial<IntakeDeps> = {}): IntakeDeps {
  return {
    ...platformIntakeDeps(env, team),
    listFactoryIds: async () => [fid],
    snapshot: async (id) => ({
      factory: { id, alias: "payments" },
      runtime: { repositories: [{ codeForge: "GITHUB", owner: "acme", name: "api" }] },
      agents: [{ ...foreman, factory_id: id }],
      automations: [a],
      integrations: {},
    }),
    factoryByAlias: async () => fid,
    user: async (id) => (id === "usr_erin" ? { ...erin, team_id: team } : undefined),
    prFacts: async () => {},
    ...extra,
  };
}

function event(team: string, n: number, userId?: string): CoreEvent {
  return {
    id: "ev_" + newToken("w"),
    team_id: team,
    provider: "github",
    type: "issue_created",
    delivery_id: "d_" + newToken("w"),
    actor: {
      provider: "github",
      external_id: "1",
      handle: "erin",
      is_bot: false,
      ...(userId ? { user_id: userId } : {}),
    },
    source: { provider: "github", kind: "issue", repo: "acme/api", number: n, title: "Crash" },
    attributes: {},
    text: "It crashes",
    received_at: new Date().toISOString(),
  };
}

async function rootRun(team: string, fid: string, n: number): Promise<Run> {
  const route = await env.ROUTER.getByName(doName.router(team)).resolve(fid, `github:acme/api#${n}`);
  const work = env.WORK.getByName(doName.work(team, route!.token)) as unknown as WorkDO;
  const st = await work.intakeState();
  return (await work.getRun(st.rootRunId!)) as Run;
}

describe("principals of the runs that intake starts", () => {
  it("an automation run has the linked actor as creator and its runAs (default: the factory) as run_as", async () => {
    const team = "pr" + newToken("w").slice(0, 8);
    const fid = "fac_p1";
    const resolved: string[] = [];
    const resolveRunAs0 = async (_f: string, runAs: string) => {
      resolved.push(runAs);
      return resolveRunAs(users, fid, runAs);
    };

    await handleEvent(deps(team, fid, automation(fid, "aut_plain", "")), team, event(team, 1, "usr_erin"));
    let run = await rootRun(team, fid, 1);
    expect(run.creator).toEqual({ type: "user", id: "usr_erin" });
    expect(run.run_as).toEqual({ type: "factory", id: fid });

    // No linked actor: no creator.
    await handleEvent(deps(team, fid, automation(fid, "aut_plain", "")), team, event(team, 2));
    run = await rootRun(team, fid, 2);
    expect(run.creator).toBeUndefined();
    expect(run.run_as).toEqual({ type: "factory", id: fid });

    const sa = deps(team, fid, automation(fid, "aut_ci", "serviceAccount:ci"), { resolveRunAs: resolveRunAs0 });
    await handleEvent(sa, team, event(team, 3, "usr_erin"));
    run = await rootRun(team, fid, 3);
    expect(run.creator).toEqual({ type: "user", id: "usr_erin" });
    expect(run.run_as).toEqual({ type: "service_account", id: "sa_ci" });
    expect(resolved).toEqual(["serviceAccount:ci"]);

    const asDana = deps(team, fid, automation(fid, "aut_dana", "user:dana@example.com"), {
      resolveRunAs: resolveRunAs0,
    });
    await handleEvent(asDana, team, event(team, 4));
    run = await rootRun(team, fid, 4);
    expect(run.run_as).toEqual({ type: "user", id: "usr_dana" });
  });

  it("an automation whose runAs names nobody starts no run (TestRunAsThatDoesNotExistFails)", async () => {
    const team = "pg" + newToken("w").slice(0, 8);
    const fid = "fac_p2";
    const d = deps(team, fid, automation(fid, "aut_gone", "serviceAccount:gone"), {
      resolveRunAs: async (_f, runAs) => resolveRunAs(users, fid, runAs),
    });
    // invalid_request is permanent: the event is dropped, not retried.
    expect(await handleEvent(d, team, event(team, 5, "usr_erin"))).toEqual({ done: true });
    expect(await env.ROUTER.getByName(doName.router(team)).resolve(fid, "github:acme/api#5")).toBeFalsy();
  });

  it("the TeamDO op resolves runAs against the users of the team", async () => {
    const team = "pt" + newToken("w").slice(0, 8);
    const stub = env.TEAM.getByName(doName.team(team));
    const meta = {
      requestId: "req_test",
      principal: {
        kind: "user" as const,
        id: "usr_admin",
        teamId: team,
        role: "admin" as const,
        factoryRoles: {},
        userId: "usr_admin",
      },
    };
    await stub.api(meta, "createServiceAccount", {}, { name: "ci", role: "editor" });
    const ok = (await stub.api(meta, "principals.resolveRunAs", {}, { factoryId: F, runAs: "serviceAccount:ci" })) as {
      type: string;
      id: string;
    };
    expect(ok.type).toBe("service_account");
    expect(ok.id).toMatch(/^sa_/);
    const bad = (await stub.api(
      meta,
      "principals.resolveRunAs",
      {},
      { factoryId: F, runAs: "user:nobody@example.com" },
    )) as {
      $error: { code: string };
    };
    expect(bad.$error.code).toBe("invalid_request");
  });
});

describe("principals of API runs", () => {
  it("a person and a service account are the creator and the run_as of their runs", async () => {
    const { key, userId } = await bootstrap();
    const f = (
      await call("POST", "/api/v1/factories", { key, body: { name: "Principals " + newToken("w").slice(0, 6) } })
    ).body.factory;
    const r = await call("POST", `/api/v1/factories/${f.id}/runs`, { key, body: { prompt: "Do the work." } });
    expect(r.status, JSON.stringify(r.body)).toBe(201);
    expect(r.body.creator).toEqual({ type: "user", id: userId });
    expect(r.body.run_as).toEqual({ type: "user", id: userId });

    const sa = await call("POST", "/api/v1/service-accounts", {
      key,
      body: { name: "ci-" + newToken("w").slice(0, 6), role: "editor", create_api_key: true },
    });
    expect(sa.status, JSON.stringify(sa.body)).toBe(201);
    const r2 = await call("POST", `/api/v1/factories/${f.id}/runs`, {
      key: sa.body.key,
      body: { prompt: "Build it." },
    });
    expect(r2.status, JSON.stringify(r2.body)).toBe(201);
    const want = { type: "service_account", id: sa.body.service_account.id };
    expect(r2.body.creator).toEqual(want);
    expect(r2.body.run_as).toEqual(want);
  });
});

describe("dispatch of the factory runs route (handleDispatch, mcpserver send_work_item)", () => {
  const newFactory = async (key: string) =>
    (await call("POST", "/api/v1/factories", { key, body: { name: "Dispatch " + newToken("w").slice(0, 6) } })).body
      .factory as { id: string };

  it("a Factory MCP dispatch has the person as creator and run_as, triggered_by mcp and the mcp source", async () => {
    const { key, userId } = await bootstrap();
    const f = await newFactory(key);
    const p = {
      kind: "factory_mcp" as const,
      id: userId,
      userId,
      teamId: "default",
      role: "admin" as const,
      factoryRoles: {},
      keyId: "mcg_test",
    };
    const run = await callRoute<{
      creator: unknown;
      run_as: unknown;
      source: string;
      work_item_id: string;
    }>(env as never, p, "POST", `/factories/${f.id}/runs`, {
      prompt: "Fix the build",
      title: "Fix the build",
      source: { provider: "mcp", kind: "work_item", title: "Fix the build" },
    });
    expect(run.creator).toEqual({ type: "user", id: userId });
    expect(run.run_as).toEqual({ type: "user", id: userId });
    expect(run.source).toBe("MCP");
    const wi = await call("GET", `/api/v1/work-items/${run.work_item_id}`, { key });
    expect(wi.body.source ?? wi.body.work_item?.source).toMatchObject({ provider: "mcp", kind: "work_item" });
  });

  it("ticket_ref routes to the active work item of its key; bad input is refused; the CLI is recorded", async () => {
    const { key } = await bootstrap();
    const f = await newFactory(key);
    const ref = "linear:PAY-" + newToken("w").slice(0, 4);
    const a = await call("POST", `/api/v1/factories/${f.id}/runs`, { key, body: { prompt: "One", ticket_ref: ref } });
    expect(a.status, JSON.stringify(a.body)).toBe(201);
    const b = await call("POST", `/api/v1/factories/${f.id}/runs`, { key, body: { prompt: "Two", ticket_ref: ref } });
    expect(b.status, JSON.stringify(b.body)).toBe(201);
    expect(b.body.work_item_id).toBe(a.body.work_item_id);
    expect(b.body.id).toBe(a.body.id);
    const wi = await call("GET", `/api/v1/work-items/${a.body.work_item_id}`, { key });
    expect(wi.body.source ?? wi.body.work_item?.source).toMatchObject({
      provider: "linear",
      kind: "issue",
      external_id: ref.slice("linear:".length),
    });

    const bad = await call("POST", `/api/v1/factories/${f.id}/runs`, {
      key,
      body: { prompt: "x", ticket_ref: "PAY 1" },
    });
    expect(bad.status).toBe(400);
    const meta = await call("POST", `/api/v1/factories/${f.id}/runs`, {
      key,
      body: { prompt: "x", metadata: { "bad key": "v" } },
    });
    expect(meta.status).toBe(400);
    const cli = await call("POST", `/api/v1/factories/${f.id}/runs`, {
      key,
      body: { prompt: "From the CLI" },
      headers: { "user-agent": "openfactory/1.0" },
    });
    expect(cli.body.source).toBe("CLI");
    const api = await call("POST", `/api/v1/factories/${f.id}/runs`, { key, body: { prompt: "From a script" } });
    expect(api.body.source).toBe("API");
  });

  it("applies the overrides of the dispatch to its agent (DispatchRequest.Overrides)", async () => {
    const { key } = await bootstrap();
    const f = await newFactory(key);
    const r = await call("POST", `/api/v1/factories/${f.id}/runs`, {
      key,
      body: { prompt: "Fast one", overrides: { model: "claude-sonnet-4-5", idle_timeout_minutes: 5 } },
    });
    expect(r.status, JSON.stringify(r.body)).toBe(201);
    expect(r.body.config?.harness?.model ?? r.body.config?.model).toBe("claude-sonnet-4-5");
    const bad = await call("POST", `/api/v1/factories/${f.id}/runs`, {
      key,
      body: { prompt: "x", overrides: { machine: "nope" } },
    });
    expect(bad.status).toBe(400);
    expect(JSON.stringify(bad.body)).toContain("unknown machine");
  });
});
