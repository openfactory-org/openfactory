// run_agents in WorkDO: the orchestration depth limit
// (internal/engine/depth_test.go), an environment override of another team
// (environment_team_test.go) and a cancel while the call is in progress
// (runagents_cancel_test.go).
import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it } from "vitest";
import type { Env } from "../../contracts/env";
import type { AgentConfig } from "../../engine/types";
import { agent, buildRuns, resultText, settle, withFx, workStub } from "../../orchestration/fixture.test-util";
import { callTool, setAgentSource, teamAgents } from "../../orchestration/index";
import { runAgents } from "./engine";
import { engineConfig } from "./index";

afterEach(async () => {
  await settle();
  setAgentSource(teamAgents);
});

/** implement can start review, so a child is allowed by spawnable_by to start a grandchild. */
const AGENTS: AgentConfig[] = [
  agent("foreman", "FOREMAN"),
  agent("implement", "IMPLEMENT"),
  agent("review", "REVIEW", { spawnable_by: ["foreman", "implement"] }),
];

async function rootWithChild() {
  setAgentSource(async () => AGENTS);
  const f = await buildRuns(
    { wi: { stage: "building", root: "root" } },
    { root: { agent: "foreman", type: "FOREMAN", workItem: "wi", state: "running" } },
  );
  const res = await callTool(env, "default", f.ids.root!, "run_agents", {
    agent_run_configs: [{ agent_identity_uid: "agt_implement", prompt: "x" }],
  });
  expect(res.isError, resultText(res)).toBeFalsy();
  const child = (JSON.parse(resultText(res)) as { runs: { run_id: string }[] }).runs[0]!.run_id;
  await withFx(f.token, (fx) => fx.repo.putRun(Object.assign(fx.repo.mustRun(child), { state: "running" })));
  return { ...f, child };
}

const children = (token: string, parent: string) => withFx(token, (fx) => fx.repo.listRuns({ parentRunId: parent }));

describe("orchestration depth", () => {
  it("defaults to 1: a child cannot start a grandchild", async () => {
    const f = await rootWithChild();
    const res = await callTool(env, "default", f.child, "run_agents", {
      agent_run_configs: [{ agent_identity_uid: "agt_review", prompt: "x" }],
    });
    expect(res.isError).toBe(true);
    expect(resultText(res)).toContain("OF_MAX_ORCHESTRATION_DEPTH is 1");
    expect(await children(f.token, f.child)).toHaveLength(0);
  });

  it("with 2, a child starts a grandchild and the grandchild is at the limit", async () => {
    const f = await rootWithChild();
    const review = AGENTS[2]!;
    const grand = await withFx(f.token, (fx) => {
      fx.cfg = { ...fx.cfg, maxDepth: 2 };
      return runAgents(fx, f.child, [{ config: review, name: "review", prompt: "x" }]);
    });
    expect(grand).toHaveLength(1);
    await withFx(f.token, (fx) => fx.repo.putRun(Object.assign(fx.repo.mustRun(grand[0]!.id), { state: "running" })));
    await expect(
      withFx(f.token, (fx) => {
        fx.cfg = { ...fx.cfg, maxDepth: 2 };
        // review may start review: spawnable_by does not stop the depth check.
        return runAgents(fx, grand[0]!.id, [
          { config: { ...review, spawnable_by: ["review"] }, name: "review", prompt: "x" },
        ]);
      }),
    ).rejects.toThrow(/depth limit/);
  });

  it("reads OF_MAX_ORCHESTRATION_DEPTH with a default of 1", () => {
    const base = env as unknown as Record<string, unknown>;
    expect(engineConfig({ ...base, OF_MAX_ORCHESTRATION_DEPTH: undefined } as unknown as Env).maxDepth).toBe(1);
    expect(engineConfig({ ...base, OF_MAX_ORCHESTRATION_DEPTH: "3" } as unknown as Env).maxDepth).toBe(3);
  });
});

describe("run_agents overrides", () => {
  // Go: a run names the environment of another team as an override; the child
  // must not get it. The TS tool has no per-child overrides (one team, D3), so
  // the field is ignored and the child runs with the configuration of its agent.
  it("ignores an environment named by the run", async () => {
    setAgentSource(async () => AGENTS);
    const f = await buildRuns(
      { wi: { stage: "building", root: "root" } },
      { root: { agent: "foreman", type: "FOREMAN", workItem: "wi", state: "running" } },
    );
    const res = await callTool(env, "default", f.ids.root!, "run_agents", {
      agent_run_configs: [
        { agent_identity_uid: "agt_implement", prompt: "x", environment_id: "env_foreign", overrides: { x: 1 } },
      ],
    });
    expect(res.isError, resultText(res)).toBeFalsy();
    const [kid] = await children(f.token, f.ids.root!);
    expect(kid!.config).toEqual(AGENTS[1]);
    expect(JSON.stringify(kid!.config)).not.toContain("env_foreign");
  });
});

describe("cancel during run_agents", () => {
  it("leaves no live child", async () => {
    const f = await buildRuns(
      { wi: { stage: "building", root: "root" } },
      { root: { agent: "foreman", type: "FOREMAN", workItem: "wi", state: "running" } },
    );
    let fired = false;
    // The cancel arrives while the Worker resolves the agents, before WorkDO creates the children.
    setAgentSource(async () => {
      if (!fired) {
        fired = true;
        await workStub(f.token).runsCancel({ requestId: "t" }, f.ids.root!, true);
      }
      return AGENTS;
    });
    const res = await callTool(env, "default", f.ids.root!, "run_agents", {
      agent_run_configs: [{ agent_identity_uid: "agt_implement", prompt: "x" }],
    });
    expect(fired).toBe(true);
    expect(res.isError).toBe(true);
    const kids = await children(f.token, f.ids.root!);
    expect(kids).toHaveLength(0);
  });
});
