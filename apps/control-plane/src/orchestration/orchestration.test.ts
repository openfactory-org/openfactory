// The per-run MCP endpoint: auth, tools/list, run_agents through the agent
// source, and ask_human creating an inbox item (orchestration server_test.go).
import { env } from "cloudflare:workers";
import { doName, newRoutedId, newToken } from "@openfactory/core";
import { afterEach, describe, expect, it } from "vitest";
import type { WorkDO } from "../do/work/index";
import type { AgentConfig, Run } from "../engine/types";
import { mintSessionToken, runnerKeys } from "../runner-api/token";
import { handleRunMcp, setAgentSource, teamAgents } from "./index";

const agent = (name: string, type: string): AgentConfig => ({
  id: `agt_${name}`,
  factory_id: "fac_o",
  name,
  description: `${name} agent`,
  type,
  instructions: "",
  harness: { type: "claude" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
});

afterEach(() => setAgentSource(teamAgents));

async function setup(): Promise<{ run: Run; token: string; work: WorkDO }> {
  const tok = newToken("w");
  const work = env.WORK.getByName(doName.work("default", tok)) as unknown as WorkDO;
  const out = await work.runsCreate(
    { requestId: "r" },
    {
      dispatch: {
        factoryId: "fac_o",
        config: agent("foreman", "FOREMAN"),
        prompt: "go",
        actor: "usr_1",
        title: "go",
        workItemId: newRoutedId("wi", tok),
      },
    },
  );
  const { token } = await mintSessionToken(await runnerKeys(env), {
    team: "default",
    run: out.run.id,
    ses: 1,
    sub: "runner",
  });
  return { run: out.run, token, work };
}

async function rpc(runId: string, token: string, method: string, params: unknown = {}) {
  const res = await handleRunMcp(
    new Request(`http://localhost/mcp/run/${runId}`, {
      method: "POST",
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    }),
    env,
  );
  return res!;
}

describe("run MCP", () => {
  it("refuses a token of another run", async () => {
    const a = await setup();
    const b = await setup();
    expect((await rpc(a.run.id, b.token, "tools/list")).status).toBe(401);
  });

  it("lists tools and starts children with run_agents", async () => {
    const { run, token, work } = await setup();
    setAgentSource(async () => [agent("foreman", "FOREMAN"), agent("coder", "IMPLEMENT")]);
    const list = (await (await rpc(run.id, token, "tools/list")).json()) as { result: { tools: { name: string }[] } };
    expect(list.result.tools.map((t) => t.name)).toContain("run_agents");
    const agents = (await (await rpc(run.id, token, "tools/call", { name: "list_agents", arguments: {} })).json()) as {
      result: { content: { text: string }[] };
    };
    expect(JSON.parse(agents.result.content[0]!.text).agents.map((a: { name: string }) => a.name)).toEqual(["coder"]);
    const res = (await (
      await rpc(run.id, token, "tools/call", {
        name: "run_agents",
        arguments: { agent_run_configs: [{ agent_identity_uid: "coder", prompt: "build" }] },
      })
    ).json()) as { result: { isError?: boolean } };
    expect(res.result.isError).toBeFalsy();
    const kids = (await work.api({ requestId: "r" }, "listRunChildren", { runId: run.id }, null)) as { runs: Run[] };
    expect(kids.runs.map((k) => k.agent_name)).toEqual(["coder"]);
    // The foreman cannot be spawned.
    const bad = (await (
      await rpc(run.id, token, "tools/call", {
        name: "run_agents",
        arguments: { agent_run_configs: [{ agent_identity_uid: "foreman", prompt: "x" }] },
      })
    ).json()) as { result: { isError?: boolean } };
    expect(bad.result.isError).toBe(true);
  });

  it("ask_human creates an inbox item of the work item", async () => {
    const { run, token, work } = await setup();
    const res = (await (
      await rpc(run.id, token, "tools/call", {
        name: "ask_human",
        arguments: { question: "blue or green?", options: ["blue", "green"] },
      })
    ).json()) as {
      result: { content: { text: string }[] };
    };
    const { inbox_id } = JSON.parse(res.result.content[0]!.text) as { inbox_id: string };
    const it = (await work.api({ requestId: "r" }, "getInbox", { id: inbox_id }, null)) as {
      kind: string;
      resolved: boolean;
      work_item_id: string;
    };
    expect(it).toMatchObject({ kind: "question", resolved: false, work_item_id: run.work_item_id });
  });
});
