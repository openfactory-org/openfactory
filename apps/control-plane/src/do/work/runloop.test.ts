// C2 gate scenarios in the Workers runtime: foreman -> child -> lifecycle ->
// parent turn with a fake runner; a DO reset mid-turn repeats no turn and no
// tool call; a runner version mismatch is refused.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, newToken, newRoutedId } from "@openfactory/core";
import type { ProviderCapabilities, SandboxProvider, SandboxTask, TaskStatus } from "@openfactory/sandbox";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { registerProvider, unregisterProvider } from "../dispatch/providers";
import { handleRunnerApi } from "../../runner-api/index";
import { toolIdempotencyKey } from "../../engine/canonical";
import type { AgentConfig, Run } from "../../engine/types";
import type { WorkDO } from "./index";
import { platformDeps, setPlannerDeps } from "../../runner-api/planner";

const TEAM = "default";

class FakeProvider implements SandboxProvider {
  readonly id = "local";
  started: SandboxTask[] = [];
  destroyed: string[] = [];
  async capabilities(): Promise<ProviderCapabilities> {
    return {
      egress: "none",
      image: "oci",
      isolation: "process",
      arch: ["amd64"],
      maxLifetimeSeconds: null,
      startRate: [],
      instanceTypes: [],
      loopbackDaemonPorts: [],
    };
  }
  async preflight() {
    return { ok: true } as const;
  }
  async start(task: SandboxTask): Promise<TaskStatus> {
    if (!this.started.some((t) => t.taskId === task.taskId)) this.started.push(task);
    return { taskId: task.taskId, state: "accepted" };
  }
  async status(taskId: string): Promise<TaskStatus> {
    return { taskId, state: "running" };
  }
  async cancel(): Promise<void> {}
  async destroy(taskId: string): Promise<void> {
    this.destroyed.push(taskId);
  }
  async list(): Promise<TaskStatus[]> {
    return [];
  }
}

const agent = (name: string, type: string, extra: Partial<AgentConfig> = {}): AgentConfig => ({
  id: `agt_${name}`,
  factory_id: "fac_test",
  name,
  description: "",
  type,
  instructions: `You are ${name}.`,
  harness: { type: "claude", model: "test" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
  ...extra,
});

let provider: FakeProvider;

beforeEach(() => {
  provider = new FakeProvider();
  registerProvider("local", () => provider);
  registerProvider("cloudflare-sandbox", () => provider);
  // The planner reads the run's WorkDO; the factory comes from a fixed snapshot.
  setPlannerDeps((e, run) => ({
    ...platformDeps(e, run),
    snapshot: async (fid) => ({ factory: { id: fid }, runtime: { repositories: [] }, agents: [], integrations: {} }),
    environment: async () => undefined,
    secret: async () => undefined,
    listSecrets: async () => [],
  }));
});

afterEach(() => {
  setPlannerDeps(platformDeps);
  unregisterProvider("local");
  unregisterProvider("cloudflare-sandbox");
});

function workStub(token: string): WorkDO {
  return env.WORK.getByName(doName.work(TEAM, token)) as unknown as WorkDO;
}

/** Delivers outbox rows and due timers between WorkDO and DispatchDO until quiet. */
async function pump(token: string, rounds = 6): Promise<void> {
  for (let i = 0; i < rounds; i++) {
    await workStub(token).pump();
    await (env.DISPATCH.getByName(doName.dispatch(TEAM)) as unknown as { pump(): Promise<void> }).pump();
  }
}

async function runner(path: string, init: RequestInit & { token?: string } = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  if (init.token) headers.set("authorization", `Bearer ${init.token}`);
  if (init.body) headers.set("content-type", "application/json");
  return handleRunnerApi(new Request(`http://localhost${path}`, { ...init, headers }), env);
}

class FakeRunner {
  seq = 0;
  cmdAck = 0;
  token = "";
  constructor(readonly task: SandboxTask) {}
  get runId() {
    return this.task.runId;
  }
  async start(version = "0.0.0-dev"): Promise<Response> {
    const res = await runner("/runner/v1/session/start", {
      method: "POST",
      token: this.task.env.OF_BOOTSTRAP_TOKEN,
      body: JSON.stringify({
        instance_id: "inst-" + this.task.taskId,
        sandbox_id: "sbx",
        runner_version: version,
        platform: { os: "linux", arch: "amd64" },
      }),
    });
    if (res.status === 200) this.token = ((await res.clone().json()) as { session_token: string }).session_token;
    return res;
  }
  async events(evs: { type: string; turn_id?: string; data?: unknown }[], seqFrom?: number): Promise<Response> {
    let s = seqFrom ?? this.seq;
    const batch = evs.map((e) => ({ seq: ++s, at: new Date().toISOString(), ...e }));
    if (seqFrom === undefined) this.seq = s;
    return runner("/runner/v1/session/events", { method: "POST", token: this.token, body: JSON.stringify({ events: batch }) });
  }
  async commands(): Promise<{ seq: number; type: string; data?: { turn_id?: string; input?: { kind: string; content: string; data?: unknown }[] } }[]> {
    const res = await runner(`/runner/v1/session/commands?after=${this.cmdAck}&wait=0`, { token: this.token });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { commands: { seq: number; type: string; data?: never }[] };
    for (const c of body.commands) this.cmdAck = Math.max(this.cmdAck, c.seq);
    return body.commands;
  }
}

async function newRootRun(token: string): Promise<Run> {
  const out = await workStub(token).runsCreate(
    { requestId: "req-1" },
    {
      dispatch: {
        factoryId: "fac_test",
        config: agent("foreman", "FOREMAN"),
        prompt: "Fix the bug",
        actor: "usr_1",
        title: "Fix the bug",
        workItemId: newRoutedId("wi", token),
      },
    },
  );
  return out.run as Run;
}

function taskFor(runId: string): SandboxTask {
  const t = provider.started.filter((x) => x.runId === runId).at(-1);
  if (!t) throw new Error(`no task for ${runId}`);
  return t;
}

describe("run loop (C2)", () => {
  it("foreman -> child -> lifecycle message -> parent turn", async () => {
    const token = newToken("w");
    const root = await newRootRun(token);
    expect(root.state).toBe("queued");
    await pump(token);
    const fr = new FakeRunner(taskFor(root.id));
    expect(fr.task.env.OF_CONTROL_URL).toBe("http://localhost");
    expect(fr.task.env.OF_RUNNER_URL).toContain("/runner/v1/bin/linux-{arch}");
    const start = await fr.start();
    expect(start.status).toBe(200);
    const startBody = (await start.json()) as { run_id: string; session: number; egress_lock: string };
    expect(startBody.run_id).toBe(root.id);
    expect(startBody.egress_lock).toBe("off");
    expect((await fr.events([{ type: "session.ready", data: { capabilities: ["mid_turn_input"] } }])).status).toBe(200);
    const [turnStart] = await fr.commands();
    expect(turnStart?.type).toBe("turn.start");
    const turnId = turnStart!.data!.turn_id!;
    expect(turnStart!.data!.input![0]!.content).toBe("Fix the bug");
    await fr.events([{ type: "turn.started", turn_id: turnId }]);

    // run_agents from the foreman's turn.
    const stub = workStub(token);
    const input = { agent_run_configs: [{ agent_identity_uid: "agt_coder", prompt: "Implement it" }] };
    const call = {
      runId: root.id,
      tool: "run_agents",
      input,
      turnId,
      key: await toolIdempotencyKey(root.id, turnId, "run_agents", input),
      plans: [{ config: agent("coder", "IMPLEMENT"), name: "coder", prompt: "Implement it" }],
    };
    const res = await stub.toolCall(call);
    expect(res.isError).toBeFalsy();
    const childId = (JSON.parse(res.content[0]!.text) as { runs: { run_id: string }[] }).runs[0]!.run_id;

    await fr.events([{ type: "turn.ended", turn_id: turnId, data: { outcome: "done", result: "started a child" } }]);
    expect((await stub.getRun(root.id)).state).toBe("succeeded");

    await pump(token);
    const cr = new FakeRunner(taskFor(childId));
    expect((await cr.start()).status).toBe(200);
    await cr.events([{ type: "session.ready", data: { capabilities: [] } }]);
    const [childTurn] = await cr.commands();
    const childTurnId = childTurn!.data!.turn_id!;
    await cr.events([
      { type: "turn.started", turn_id: childTurnId },
      { type: "message", turn_id: childTurnId, data: { role: "assistant", content: "done" } },
      { type: "turn.ended", turn_id: childTurnId, data: { outcome: "done", result: "PR opened" } },
    ]);
    expect((await stub.getRun(childId)).state).toBe("succeeded");

    // The lifecycle message reached the parent in the child's transaction and
    // started a parent turn on its live session.
    const parentCmds = await fr.commands();
    const lifecycle = parentCmds.find((c) => c.type === "turn.start");
    expect(lifecycle).toBeDefined();
    const item = lifecycle!.data!.input!.find((i) => i.kind === "lifecycle" && i.content.endsWith("succeeded."));
    expect(item?.content).toBe('Child run "coder" succeeded.');
    expect((item?.data as { result: string }).result).toBe("PR opened");
  });

  it("a DO reset mid-turn repeats no turn and no tool call", async () => {
    const token = newToken("w");
    const root = await newRootRun(token);
    await pump(token);
    const fr = new FakeRunner(taskFor(root.id));
    await fr.start();
    await fr.events([{ type: "session.ready", data: { capabilities: [] } }]);
    const [ts] = await fr.commands();
    const turnId = ts!.data!.turn_id!;
    const startedSeq = fr.seq;
    await fr.events([{ type: "turn.started", turn_id: turnId }]);
    const input = { agent_run_configs: [{ agent_identity_uid: "agt_coder", prompt: "x" }] };
    const call = {
      runId: root.id,
      tool: "run_agents",
      input,
      turnId,
      key: await toolIdempotencyKey(root.id, turnId, "run_agents", input),
      plans: [{ config: agent("coder", "IMPLEMENT"), name: "coder", prompt: "x" }],
    };
    const first = await workStub(token).toolCall(call);

    // Reset the DO (deploy, runtime restart).
    await runInDurableObject(env.WORK.getByName(doName.work(TEAM, token)), (_i, state) => {
      state.abort("reset");
    }).catch(() => undefined);

    // The runner retries the same batch and the same tool call.
    const again = await fr.events([{ type: "turn.started", turn_id: turnId }], startedSeq);
    expect(again.status).toBe(200);
    expect(((await again.json()) as { ack: number }).ack).toBe(startedSeq + 1);
    const dup = await workStub(token).toolCall(call);
    expect(dup._meta?.duplicate).toBe(true);
    expect(dup.content[0]!.text).toBe(first.content[0]!.text);
    const children = (await workStub(token).api({ requestId: "r" }, "listRunChildren", { runId: root.id }, null)) as { runs: Run[] };
    expect(children.runs).toHaveLength(1);
    // No second turn.start.
    expect(await fr.commands()).toHaveLength(0);
    const all = await runner(`/runner/v1/session/commands?after=0&wait=0`, { token: fr.token });
    const cmds = ((await all.json()) as { commands: { type: string }[] }).commands;
    expect(cmds.filter((c) => c.type === "turn.start")).toHaveLength(1);
  });

  it("refuses a mismatched runner_version", async () => {
    const token = newToken("w");
    const root = await newRootRun(token);
    await pump(token);
    const fr = new FakeRunner(taskFor(root.id));
    const res = await fr.start("9.9.9");
    expect(res.status).toBe(426);
    expect(((await res.json()) as { code: string }).code).toBe("runner_version_mismatch");
    expect((await workStub(token).getRun(root.id)).state).toBe("pending");
  });

  it("answers 426 to an unsupported major version header and 401 to a bad token", async () => {
    const r1 = await runner("/runner/v1/session/events", { method: "POST", headers: { "OF-Runner-Version": "2.0.0" } });
    expect(r1.status).toBe(426);
    const r2 = await runner("/runner/v1/session/events", { method: "POST", token: "ofs_bad.sig", body: "{}" });
    expect(r2.status).toBe(401);
  });

  it("a session token outlives no session: end refuses later calls", async () => {
    const token = newToken("w");
    const root = await newRootRun(token);
    await pump(token);
    const fr = new FakeRunner(taskFor(root.id));
    await fr.start();
    await fr.events([{ type: "session.ready", data: { capabilities: [] } }]);
    const end = await runner("/runner/v1/session/end", {
      method: "POST",
      token: fr.token,
      body: JSON.stringify({ reason: "idle", last_event_seq: fr.seq }),
    });
    expect(end.status).toBe(200);
    const after = await fr.commands().catch((e: unknown) => e);
    expect(after).toBeInstanceOf(Error);
    // A run that ended its session before any turn is lost with runner_lost and retried.
    const run = await workStub(token).getRun(root.id);
    expect(run.state).toBe("queued");
  });
});
