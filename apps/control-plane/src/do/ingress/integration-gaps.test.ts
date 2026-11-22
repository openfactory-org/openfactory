// Ports of internal/engine integration_gaps_test.go (auto follow-up off,
// starting repository refs, managed MCP servers) and inbox_test.go
// TestHandoffClosesOnReviewAndMerge, against real RouterDO and WorkDO. The
// outbound notice of an approval is in src/do/work/inbox.test.ts.
import { env } from "cloudflare:workers";
import { doName, newToken, notFound } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CoreEvent } from "../../contracts/do-messages";
import type { AgentConfig, InboxItem, Run } from "../../engine/types";
import { Planner, type PlannerDeps } from "../../runner-api/planner";
import type { WorkDO } from "../work/index";
import { handleEvent, platformIntakeDeps, type IntakeDeps } from "./intake";

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

function setup(team: string, factoryId: string): IntakeDeps {
  return {
    ...platformIntakeDeps(env, team),
    listFactoryIds: async () => [factoryId],
    snapshot: async (fid) => ({
      factory: { id: fid, alias: "payments" },
      runtime: { repositories: [{ codeForge: "GITHUB", owner: "acme", name: "api" }] },
      agents: [{ ...foreman, factory_id: fid }],
      automations: [
        {
          id: "aut_issues",
          factory_id: fid,
          name: "issues",
          enabled: true,
          agent: "foreman",
          prompt: "Handle the issue.",
          triggers: [{ provider: "github", event: "issue_labeled" }],
        },
      ],
      integrations: {},
    }),
    factoryByAlias: async () => factoryId,
    user: async (id) => ({ id, team_id: team, email: "a@example.test" }),
    prFacts: async () => {},
    bundle: async () => undefined,
  };
}

const ev = (team: string, p: Partial<CoreEvent>): CoreEvent => ({
  id: "ev_" + newToken("w"),
  team_id: team,
  provider: "github",
  type: "issue_labeled",
  delivery_id: "d_" + newToken("w"),
  actor: { provider: "github", external_id: "1", handle: "alice", is_bot: false, user_id: "usr_a" },
  source: { provider: "github", kind: "issue", repo: "acme/api", number: 7, title: "Invoices omit the PO number" },
  attributes: {},
  received_at: new Date().toISOString(),
  ...p,
});

async function workOf(team: string, fid: string): Promise<{ w: WorkDO; root: string; wiId: string }> {
  const route = await env.ROUTER.getByName(doName.router(team)).resolve(fid, "github:acme/api#7");
  const w = env.WORK.getByName(doName.work(team, route!.token)) as unknown as WorkDO;
  const st = await w.intakeState();
  return { w, root: st.rootRunId!, wiId: st.workItemId! };
}

const followUps = async (w: WorkDO, runId: string) =>
  ((await w.api({ requestId: "t" }, "listRunInputs", { runId }, null)) as { inputs: { content: string }[] }).inputs
    .map((i) => i.content)
    .filter((c) => c.startsWith("[follow-up]"));

describe("integration gaps", () => {
  it("auto_follow_up false (bool or string) does not deliver a comment; true does", async () => {
    const team = "ga" + newToken("w").slice(0, 8);
    const d = setup(team, "fac_ga");
    await handleEvent(d, team, ev(team, {}));
    const { w, root } = await workOf(team, "fac_ga");
    for (const v of [false, "false", " FALSE "]) {
      const c = ev(team, { type: "issue_commented", text: "just chatting", attributes: { auto_follow_up: v } });
      c.actor = { ...c.actor, handle: "bob" };
      expect(await handleEvent(d, team, c)).toEqual({ done: true });
    }
    expect(await followUps(w, root)).toHaveLength(0);
    await handleEvent(
      d,
      team,
      ev(team, { type: "issue_commented", text: "now deliver", attributes: { auto_follow_up: true } }),
    );
    const got = await followUps(w, root);
    expect(got).toHaveLength(1);
    expect(got[0]).toContain("now deliver");
  });

  for (const typ of ["pull_request_review_submitted", "pull_request_merged"]) {
    it(`a ${typ} event closes the handoff items; the forge close cannot be undone`, async () => {
      const team = "gh" + newToken("w").slice(0, 8);
      const d = setup(team, "fac_gh");
      await handleEvent(d, team, ev(team, {}));
      const { w, root } = await workOf(team, "fac_gh");
      const h = (await w.api(
        { requestId: "t" },
        "createInboxItem",
        {},
        { kind: "handoff", title: "PR ready", body: "PR ready", run_id: root },
      )) as InboxItem;
      await handleEvent(
        d,
        team,
        ev(team, {
          type: typ,
          actor: { provider: "github", external_id: "2", handle: "rev", is_bot: false, user_id: "usr_r" },
          source: {
            provider: "github",
            kind: "pull_request",
            repo: "acme/api",
            number: 7,
            url: "https://github.com/acme/api/pull/7",
          },
        }),
      );
      const got = (await w.api({ requestId: "t" }, "getInbox", { id: h.id }, null)) as InboxItem;
      expect(got.resolved).toBe(true);
      expect(got.resolved_via).toBe("forge");
      expect(got.resolution).toContain("rev");
      expect(got.resolution).toContain(typ.endsWith("merged") ? "pull request merged" : "review submitted");
      const err = await w.api({ requestId: "t" }, "unresolveInbox", { id: h.id }, null).catch((e: unknown) => e);
      expect(String(err)).toMatch(/platform closed this item/);
    });
  }
});

// ---------- planner: starting refs and managed MCP servers ----------

const SHA = "0123456789abcdef0123456789abcdef01234567";

function plannerDeps(over: Partial<PlannerDeps> = {}): PlannerDeps {
  return {
    snapshot: async () => ({
      factory: { id: "fac_p" },
      runtime: { repositories: [{ codeForge: "GITHUB", owner: "acme", name: "api" }] },
      agents: [{ name: "foreman", type: "FOREMAN" }],
      integrations: {},
    }),
    state: async () => ({ repoRefs: [] }),
    recordStartCommits: async () => {},
    environment: async () => undefined,
    secret: async (fid, name) =>
      fid !== "" ? undefined : ({ SENTRY_TOKEN: "s3", CLAUDE_KEY: "sk-test" } as Record<string, string>)[name],
    listSecrets: async (fid) => (fid ? [] : [{ name: "CLAUDE_KEY", type: "anthropic_api_key" }]),
    resolveManagedMcp: async (_r, id) => {
      throw notFound(`mcp server ${id}`);
    },
    forges: {
      forge: async () =>
        ({ cloneURL: async (o: string, n: string) => `https://x:t@github.com/${o}/${n}.git` }) as never,
    },
    sandboxUrl: "http://cp",
    now: () => Date.now(),
    ...over,
  };
}

describe("planner gaps", () => {
  it("a run with starting repo refs clones each repository at its commit", async () => {
    const token = newToken("w");
    const w = env.WORK.getByName(doName.work("default", token)) as unknown as WorkDO;
    const refs = [
      { codeForge: "GITHUB", owner: "acme", repo: "api", ref: SHA },
      { codeForge: "GITHUB", owner: "acme", repo: "docs", ref: SHA },
    ];
    const { run } = await w.runsCreate(
      { requestId: "t" },
      {
        run: {
          factoryId: "fac_p",
          config: { ...foreman, factory_id: "fac_p" },
          prompt: "trial",
          actor: "bench",
          startingRepoRefs: refs,
        },
      },
    );
    const st = await w.plannerState(run.id);
    expect(st.repoRefs).toEqual(refs);
    const p = await new Planner(plannerDeps({ state: async () => st as never })).plan(run as Run);
    expect(p.repos.map((r) => [r.name, r.ref])).toEqual([
      ["api", SHA],
      ["docs", SHA],
    ]);
    expect(p.repos[1]!.cloneUrl).not.toBe("");
    await w.runsCancel({ requestId: "t" }, run.id, false);
  });

  it("a managed MCP server is resolved for the team; its headers expand secrets", async () => {
    const cfg = {
      ...foreman,
      factory_id: "fac_p",
      mcp_servers: { tracker: { managedId: "linear", headers: { "X-Extra": "{{SENTRY_TOKEN}}" } } },
    };
    const run = {
      id: "run_wabcdefghijklmnoxyz",
      version: 1,
      team_id: "team1",
      factory_id: "fac_p",
      state: "claimed",
      prompt: "go",
      consumed_seq: 0,
      usage: { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 },
      created_at: "2026-01-01T00:00:00Z",
      config: cfg,
    } as unknown as Run;
    // Without a registered server the plan fails clearly.
    const err = await new Planner(plannerDeps()).plan(run).catch((e: unknown) => e);
    expect(String(err)).toContain('managedId "linear"');

    let resolvedFor = "";
    const p = await new Planner(
      plannerDeps({
        resolveManagedMcp: async (r, id) => {
          resolvedFor = r.team_id;
          if (id !== "linear") throw notFound(id);
          return { url: "https://mcp.linear.test", headers: { Authorization: "Bearer x" } };
        },
      }),
    ).plan(run);
    const got = p.mcpServers.tracker!;
    expect(got.url).toBe("https://mcp.linear.test");
    expect(got.managedId).toBeUndefined();
    expect(got.headers).toEqual({ Authorization: "Bearer x", "X-Extra": "s3" });
    expect(resolvedFor).toBe("team1");
  });
});
