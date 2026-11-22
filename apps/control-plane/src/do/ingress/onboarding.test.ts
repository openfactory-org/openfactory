// Port of internal/engine/onboarding_test.go TestGitHubOnboardingNotices: a
// GitHub mention from an unlinked or non-member account starts no work; with
// notices on (the default) the account gets one onboarding comment, with
// notices off none. The gate applies to mentions only: an automation match
// runs, and bot events are not gated.
import { env } from "cloudflare:workers";
import { doName, newToken } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CoreEvent } from "../../contracts/do-messages";
import type { AgentConfig, Run } from "../../engine/types";
import { FakeResolver } from "../../../test/fakes/forge";
import type { WorkDO } from "../work/index";
import { handleEvent, platformIntakeDeps, type IntakeDeps } from "./intake";
import { githubActorEligible, onboardingComment, postOnboardingNotice } from "./onboarding";

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

function setup(team: string, fid: string, notices: boolean | undefined, forges: FakeResolver): IntakeDeps {
  return {
    ...platformIntakeDeps(env, team),
    listFactoryIds: async () => [fid],
    snapshot: async (id) => ({
      factory: { id, alias: "payments" },
      runtime: {
        repositories: [{ codeForge: "GITHUB", owner: "acme", name: "api" }],
        ...(notices === undefined ? {} : { github_onboarding_notices: notices }),
      },
      agents: [{ ...foreman, factory_id: id }],
      automations: [
        {
          id: "aut_labels",
          factory_id: id,
          name: "labels",
          enabled: true,
          agent: "foreman",
          prompt: "Handle it.",
          triggers: [{ provider: "github", event: "issue_labeled" }],
        },
      ],
      integrations: {},
    }),
    factoryByAlias: async () => fid,
    user: async (id) => (id === "ext_user" ? { id, team_id: "other-team" } : { id, team_id: team }),
    prFacts: async () => {},
    bundle: async () => undefined,
    onboardingNotice: async (ev, reason, factoryId) => {
      await postOnboardingNotice(
        { DATA: env.DATA, OF_PUBLIC_URL: "https://cp.test" },
        forges,
        team,
        ev,
        reason,
        factoryId,
      );
    },
  };
}

function mention(team: string, handle: string, userId: string, extra: Partial<CoreEvent> = {}): CoreEvent {
  return {
    id: "ev_" + newToken("w"),
    team_id: team,
    provider: "github",
    type: "issue_mentioned",
    delivery_id: "d_" + newToken("w"),
    actor: { provider: "github", external_id: "id-" + handle, handle, is_bot: false, user_id: userId },
    source: { provider: "github", kind: "issue", repo: "acme/api", number: 7, title: "Payments" },
    attributes: { alias: "payments" },
    text: "@payments please look",
    received_at: new Date().toISOString(),
    ...extra,
  };
}

const comments = (f: FakeResolver) =>
  (f.forges.get("GITHUB")?.callsOf("comment") ?? []).map(
    (c) => `${String(c.args[0])}/${String(c.args[1])}#${String(c.args[2])}: ${String(c.args[3])}`,
  );

async function runsOf(team: string, fid: string, number: number): Promise<Run[]> {
  const route = await env.ROUTER.getByName(doName.router(team)).resolve(fid, `github:acme/api#${number}`);
  if (!route) return [];
  const w = env.WORK.getByName(doName.work(team, route.token)) as unknown as WorkDO;
  const st = await w.intakeState();
  if (!st.workItemId) return [];
  return ((await w.api({ requestId: "t" }, "listWorkItemRuns", { id: st.workItemId }, null)) as { runs: Run[] }).runs;
}

describe("GitHub onboarding notices", () => {
  const cases: { name: string; notices?: boolean; events: (t: string) => CoreEvent[]; n: number; want: string }[] = [
    {
      name: "unlinked, notices on (default), once",
      events: (t) => [mention(t, "stranger", ""), mention(t, "stranger", "")],
      n: 1,
      want: "Link your account",
    },
    {
      name: "non-member",
      notices: true,
      events: (t) => [mention(t, "outsider", "ext_user")],
      n: 1,
      want: "not an active member",
    },
    { name: "notices off", notices: false, events: (t) => [mention(t, "stranger", "")], n: 0, want: "" },
  ];
  for (const tc of cases) {
    it(tc.name, async () => {
      const team = "ob" + newToken("w").slice(0, 8);
      const forges = new FakeResolver();
      const d = setup(team, "fac_ob", tc.notices, forges);
      for (const e of tc.events(team)) expect(await handleEvent(d, team, e)).toEqual({ done: true });
      // Ineligible accounts start no work.
      expect(await runsOf(team, "fac_ob", 7)).toHaveLength(0);
      const got = comments(forges);
      expect(got).toHaveLength(tc.n);
      if (tc.want) {
        expect(got[0]!.startsWith("acme/api#7: ")).toBe(true);
        expect(got[0]).toContain(tc.want);
        // The comment is public: it names the dashboard and holds no link URL.
        expect(got[0]).not.toContain("/ui/link/");
      }
    });
  }

  it("a member mention starts work; an automation match from an unlinked account runs; bots are not gated", async () => {
    const team = "om" + newToken("w").slice(0, 8);
    const forges = new FakeResolver();
    const d = setup(team, "fac_om", true, forges);
    await handleEvent(d, team, mention(team, "maintainer", "usr_m"));
    const runs = await runsOf(team, "fac_om", 7);
    expect(runs).toHaveLength(1);
    expect(runs[0]!.triggered_by).toBe("mention");

    const out = mention(team, "contributor", "", {
      type: "issue_labeled",
      text: "",
      source: { provider: "github", kind: "issue", repo: "acme/api", number: 8, title: "Outside report" },
    });
    await handleEvent(d, team, out);
    const auto = await runsOf(team, "fac_om", 8);
    expect(auto).toHaveLength(1);
    expect(auto[0]!.triggered_by ?? "").toMatch(/^automation:/);
    expect(comments(forges)).toHaveLength(0);

    const bot = mention(team, "ci-bot", "", { type: "check_run_completed" });
    bot.actor.is_bot = true;
    expect(
      await githubActorEligible(d, team, bot, [
        { factory: { id: "fac_om" }, runtime: { github_onboarding_notices: true } },
      ]),
    ).toBe(true);
  });

  it("comment texts", () => {
    expect(onboardingComment("dana", "unlinked", "https://cp.test/")).toContain(
      "Sign in to https://cp.test/integrations and link your GitHub account",
    );
    expect(onboardingComment("dana", "unlinked", "")).toContain("Sign in to the Open Factory dashboard");
  });
});
