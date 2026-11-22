// Event intake against real RouterDO and WorkDO (ported from
// internal/engine intake_test.go TestHandleEventRetry* and events tests):
// an automation match creates one work item and root run; a retry of the
// same action creates nothing; a comment continues the work item; an
// approval reply resolves the open approval.
import { env } from "cloudflare:workers";
import { doName, newToken } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CoreEvent } from "../../contracts/do-messages";
import type { AgentConfig, Run } from "../../engine/types";
import type { WorkDO } from "../work/index";
import { handleEvent, platformIntakeDeps, storeEventBundle, type IntakeDeps } from "./intake";
import { expandEventFiles } from "../../runner-api/index";

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

function setup(team: string, factoryId: string, extra: Partial<IntakeDeps> = {}): IntakeDeps {
  const base = platformIntakeDeps(env, team);
  return {
    ...base,
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
          triggers: [{ provider: "github", event: "issue_created" }],
        },
      ],
      integrations: {},
    }),
    factoryByAlias: async () => factoryId,
    user: async (id) => ({ id, team_id: team, email: "a@example.test" }),
    prFacts: async () => {},
    ...extra,
  };
}

const ev = (team: string, p: Partial<CoreEvent>): CoreEvent => ({
  id: "ev_" + newToken("w"),
  team_id: team,
  provider: "github",
  type: "issue_created",
  delivery_id: "d_" + newToken("w"),
  actor: { provider: "github", external_id: "1", handle: "alice", is_bot: false, user_id: "usr_a" },
  source: { provider: "github", kind: "issue", repo: "acme/api", number: 7, title: "Crash on save" },
  attributes: {},
  text: "It crashes",
  received_at: new Date().toISOString(),
  ...p,
});

describe("intake (HandleEvent)", () => {
  it("creates one work item and root run per action; a retry creates nothing; a comment continues", async () => {
    const team = "it" + newToken("w").slice(0, 8);
    const d = setup(team, "fac_i1");
    const e1 = ev(team, {});
    expect(await handleEvent(d, team, structuredClone(e1))).toEqual({ done: true });
    const route = await env.ROUTER.getByName(doName.router(team)).resolve("fac_i1", "github:acme/api#7");
    expect(["reserved", "active"]).toContain(route?.status);
    const work = env.WORK.getByName(doName.work(team, route!.token)) as unknown as WorkDO;
    const st = await work.intakeState();
    expect(st.rootRunId).toBeTruthy();
    expect(st.rootAgent).toBe("foreman");
    const runs = (await work.api({ requestId: "t" }, "listWorkItemRuns", { id: st.workItemId! }, null)) as {
      runs: Run[];
    };
    expect(runs.runs).toHaveLength(1);
    expect(runs.runs[0]!.prompt).toContain("Handle the issue.");
    expect(runs.runs[0]!.prompt).toContain("## Triggering event");
    expect(runs.runs[0]!.triggered_by).toBe("automation:aut_issues");

    // Retry of the same action: no second run.
    expect(await handleEvent(d, team, structuredClone(e1))).toEqual({ done: true });
    expect(
      ((await work.api({ requestId: "t" }, "listWorkItemRuns", { id: st.workItemId! }, null)) as { runs: Run[] }).runs,
    ).toHaveLength(1);

    // A comment from a linked member continues the work item as one message.
    const c = ev(team, { type: "issue_commented", text: "Also on load" });
    await handleEvent(d, team, structuredClone(c));
    await handleEvent(d, team, structuredClone(c));
    const inputs = (await work.api({ requestId: "t" }, "listRunInputs", { runId: st.rootRunId! }, null)) as {
      inputs: { content: string }[];
    };
    const follow = inputs.inputs.filter((i) => i.content.startsWith("[follow-up]"));
    expect(follow).toHaveLength(1);
    expect(follow[0]!.content).toContain("- Sender: linked team member");
  });

  it("attaches the event context: the prompt names the event files; the runner API expands them", async () => {
    const team = "ie" + newToken("w").slice(0, 8);
    const d = setup(team, "fac_ie");
    await handleEvent(d, team, ev(team, { payload: { issue: { number: 7 } } }));
    const route = await env.ROUTER.getByName(doName.router(team)).resolve("fac_ie", "github:acme/api#7");
    const work = env.WORK.getByName(doName.work(team, route!.token)) as unknown as WorkDO;
    const st = await work.intakeState();
    const run = (await work.getRun(st.rootRunId!)) as Run;
    expect(run.prompt).toContain("## Event files (untrusted data)");
    expect(run.prompt).toMatch(/\.openfactory\/events\/msg_[a-z0-9]+\//);
    // A follow-up gets its own event files (envelope and payload, no provider context).
    await handleEvent(d, team, ev(team, { type: "issue_commented", text: "More" }));
    const inputs = (await work.api({ requestId: "t" }, "listRunInputs", { runId: st.rootRunId! }, null)) as {
      inputs: { content: string }[];
    };
    expect(inputs.inputs.some((i) => i.content.startsWith("[follow-up]") && i.content.includes("## Event files"))).toBe(
      true,
    );

    const b = await storeEventBundle(env, ev(team, { payload: { a: 1 } }), false);
    expect(b?.ref).toMatch(/^eventctx\//);
    const cmds = [{ data: { turn_id: "t", input: [{ message_id: "m", content: "x", event_files_ref: b!.ref }] } }];
    await expandEventFiles(env.DATA, cmds);
    const item = cmds[0]!.data.input[0] as Record<string, unknown>;
    expect(item.event_files_ref).toBeUndefined();
    const files = item.files as { path: string; data: string }[];
    expect(files.map((f) => f.path.split("/").pop())).toEqual(["envelope.json", "payload.json"]);
    expect(atob(files[1]!.data)).toBe('{"a":1}');
  });

  it("a self event is dropped and a non-matching event creates nothing", async () => {
    const team = "is" + newToken("w").slice(0, 8);
    const d = setup(team, "fac_i2");
    await handleEvent(
      d,
      team,
      ev(team, { actor: { provider: "github", external_id: "b", handle: "bot", is_bot: true, is_self: true } }),
    );
    await handleEvent(
      d,
      team,
      ev(team, { type: "issue_labeled", source: { provider: "github", kind: "issue", repo: "acme/api", number: 9 } }),
    );
    expect(await env.ROUTER.getByName(doName.router(team)).resolve("fac_i2", "github:acme/api#7")).toBeNull();
    expect(await env.ROUTER.getByName(doName.router(team)).resolve("fac_i2", "github:acme/api#9")).toBeNull();
  });

  it("a transient failure asks for a retry; the retry creates the run once", async () => {
    const team = "ir" + newToken("w").slice(0, 8);
    let fail = true;
    const base = setup(team, "fac_i3");
    const d: IntakeDeps = {
      ...base,
      work: (token) => {
        const w = base.work(token);
        return {
          ...w,
          applyAction: async (i: unknown) => {
            if (fail) {
              fail = false;
              await w.applyAction(i);
              throw new Error("connection reset after commit");
            }
            return w.applyAction(i);
          },
          intakeState: () => w.intakeState(),
          allowBotAction: (k: string) => w.allowBotAction(k),
          reopen: (x: { actionKey: string; event: CoreEvent }) => w.reopen(x),
        };
      },
    };
    const e = ev(team, {});
    const first = await handleEvent(d, team, structuredClone(e));
    expect(first).toMatchObject({ retry: true });
    expect(await handleEvent(d, team, structuredClone(e))).toEqual({ done: true });
    const route = await env.ROUTER.getByName(doName.router(team)).resolve("fac_i3", "github:acme/api#7");
    const work = env.WORK.getByName(doName.work(team, route!.token)) as unknown as WorkDO;
    const st = await work.intakeState();
    expect(
      ((await work.api({ requestId: "t" }, "listWorkItemRuns", { id: st.workItemId! }, null)) as { runs: Run[] }).runs,
    ).toHaveLength(1);
  });

  it("inbox: resolve, then unresolve within the window withdraws the answer", async () => {
    const team = "iu" + newToken("w").slice(0, 8);
    const token = newToken("w");
    const work = env.WORK.getByName(doName.work(team, token)) as unknown as WorkDO;
    const meta = {
      requestId: "t",
      principal: {
        kind: "user" as const,
        id: "usr_a",
        userId: "usr_a",
        teamId: team,
        role: "admin" as const,
        factoryRoles: {},
      },
    };
    const { run } = await work.runsCreate(meta, {
      dispatch: { factoryId: "fac_u", config: foreman, prompt: "go", actor: "usr_a", title: "t" },
    });
    const it0 = (await work.api(
      meta,
      "createInboxItem",
      {},
      { kind: "question", title: "Which DB?", run_id: run.id, track_key: "q1" },
    )) as { id: string };
    const again = (await work.api(
      meta,
      "createInboxItem",
      {},
      { kind: "question", title: "Which DB?", run_id: run.id, track_key: "q1" },
    )) as { id: string };
    expect(again.id).toBe(it0.id);
    await work.api(meta, "resolveInbox", { id: it0.id }, { resolution: "Postgres" });
    const un = (await work.api(meta, "unresolveInbox", { id: it0.id }, null)) as { resolved: boolean };
    expect(un.resolved).toBe(false);
    const inputs = (await work.api(meta, "listRunInputs", { runId: run.id }, null)) as {
      inputs: { content: string }[];
    };
    expect(inputs.inputs.some((i) => i.content.startsWith("[human correction] usr_a withdrew the answer"))).toBe(true);
  });
});
