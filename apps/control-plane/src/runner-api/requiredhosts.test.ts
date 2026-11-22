// Required hosts of a restricted run, as engine.RequiredHosts of the Go
// server computes them for a task: the control plane, the inference host and
// the hosts of the MCP servers in the run configuration. A required host is
// always allowed and cannot be denied.
import { describe, expect, it } from "vitest";
import type { Env } from "../contracts/env";
import type { Run, RunSession } from "../engine/types";
import type { Fx } from "../do/work/engine";
import { taskPolicy } from "../do/work/grant";
import { startConfigOf, type RunnerPlan } from "./planner";
import { mcpServerUrlsOf } from "./startconfig";

const env = { OF_PUBLIC_URL: "https://cp.example.com", OF_R2_ACCOUNT_ID: "acct" } as unknown as Env;
const fx = { cfg: { controlUrl: "https://cp.example.com", r2AccountId: "acct" } } as unknown as Fx;

const run = (cfg: Record<string, unknown>): Run =>
  ({
    id: "run_wabcdefghijklmnoxyz",
    factory_id: "fac_1",
    config: {
      id: "agt_1",
      factory_id: "fac_1",
      name: "foreman",
      description: "",
      type: "FOREMAN",
      instructions: "",
      harness: { type: "claude" },
      spawnable_by: null,
      network_policy: { restricted: true, allowed_hosts: ["pypi.org"], denied_hosts: ["MCP.Example.com", "evil.test"] },
      ...cfg,
    },
  }) as unknown as Run;

const plan: RunnerPlan = {
  env: {},
  secretEnv: {},
  harnessAuth: {},
  credential: { source: "none" } as RunnerPlan["credential"],
  repos: [],
  workDir: "",
  skills: [],
  systemPrompt: "",
  mcpServers: {},
  sessionTokenServers: new Set(),
  setupCommands: [],
  envSetupCommands: [],
};

describe("required hosts of a restricted run", () => {
  it.each([
    { servers: undefined, want: [] },
    { servers: {}, want: [] },
    {
      servers: { a: { url: "https://mcp.example.com/sse" }, b: { command: "npx" }, c: null, d: { url: 3 } },
      want: ["https://mcp.example.com/sse"],
    },
  ])("mcpServerUrlsOf($servers)", ({ servers, want }) => {
    expect(mcpServerUrlsOf({ mcp_servers: servers as Run["config"]["mcp_servers"] })).toEqual(want);
  });

  it("the task policy and the start configuration allow MCP server hosts", async () => {
    const r = run({ mcp_servers: { tracker: { url: "https://mcp.example.com/sse" } } });
    const required = ["cp.example.com", "acct.r2.cloudflarestorage.com", "mcp.example.com", "api.anthropic.com"];

    const task = taskPolicy(fx, r)!;
    expect(task.alwaysAllow).toEqual(required);
    expect(task.deniedHosts).toEqual(["evil.test"]);
    expect(task.allowedHosts).toContain("mcp.example.com");

    const session = { session: 1 } as RunSession;
    const sc = startConfigOf(env, r, session, plan);
    expect(sc.network_policy?.always_allow).toEqual(required);
    expect(sc.network_policy?.denied_hosts).toEqual(["evil.test"]);
    expect(sc.network_policy?.allowed_hosts).toEqual([...required, "pypi.org"].sort());
  });

  it("an unrestricted run has no task policy", () => {
    expect(taskPolicy(fx, run({ network_policy: { restricted: false } }))).toBeUndefined();
  });
});
