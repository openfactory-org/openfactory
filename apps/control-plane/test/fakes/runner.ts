// A fake sandbox provider and runner for WorkDO tests (the pattern of
// src/do/work/runloop.test.ts): install() registers the provider and a fixed
// planner snapshot; FakeRunner speaks /runner/v1 for one started task.
import { env } from "cloudflare:workers";
import { doName } from "@openfactory/core";
import type { ProviderCapabilities, SandboxProvider, SandboxTask, TaskStatus } from "@openfactory/sandbox";
import { registerProvider, unregisterProvider } from "../../src/do/dispatch/providers";
import { handleRunnerApi } from "../../src/runner-api/index";
import { platformDeps, setPlannerDeps } from "../../src/runner-api/planner";
import type { WorkDO } from "../../src/do/work/index";

export class FakeProvider implements SandboxProvider {
  readonly id = "local";
  started: SandboxTask[] = [];
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
  async destroy(): Promise<void> {}
  async list(): Promise<TaskStatus[]> {
    return [];
  }
  taskFor(runId: string): SandboxTask {
    const t = this.started.filter((x) => x.runId === runId).at(-1);
    if (!t) throw new Error(`no task for ${runId}`);
    return t;
  }
}

/** Registers a fake provider and planner deps; returns the provider and the teardown. */
export function installFakeSandbox(): { provider: FakeProvider; uninstall: () => void } {
  const provider = new FakeProvider();
  registerProvider("local", () => provider);
  registerProvider("cloudflare-sandbox", () => provider);
  setPlannerDeps((e, run) => ({
    ...platformDeps(e, run),
    snapshot: async (fid) => ({ factory: { id: fid }, runtime: { repositories: [] }, agents: [], integrations: {} }),
    environment: async () => undefined,
    secret: async () => undefined,
    listSecrets: async () => [],
  }));
  return {
    provider,
    uninstall: () => {
      setPlannerDeps(platformDeps);
      unregisterProvider("local");
      unregisterProvider("cloudflare-sandbox");
    },
  };
}

export function workStub(team: string, token: string): WorkDO {
  return env.WORK.getByName(doName.work(team, token)) as unknown as WorkDO;
}

/** Delivers outbox rows and due timers between WorkDO and DispatchDO until quiet. */
export async function pump(team: string, token: string, rounds = 6): Promise<void> {
  for (let i = 0; i < rounds; i++) {
    await workStub(team, token).pump();
    await (env.DISPATCH.getByName(doName.dispatch(team)) as unknown as { pump(): Promise<void> }).pump();
  }
}

async function runner(path: string, init: RequestInit & { token?: string } = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  if (init.token) headers.set("authorization", `Bearer ${init.token}`);
  if (init.body) headers.set("content-type", "application/json");
  return handleRunnerApi(new Request(`http://localhost${path}`, { ...init, headers }), env);
}

export interface InputItem {
  kind: string;
  content: string;
  message_id?: string;
  data?: unknown;
  files?: { path: string; data: string }[];
}

export interface Command {
  seq: number;
  type: string;
  data?: { turn_id?: string; input?: InputItem[] };
}

export class FakeRunner {
  seq = 0;
  cmdAck = 0;
  token = "";
  constructor(readonly task: SandboxTask) {}
  async start(): Promise<Response> {
    const res = await runner("/runner/v1/session/start", {
      method: "POST",
      token: this.task.env.OF_BOOTSTRAP_TOKEN,
      body: JSON.stringify({
        instance_id: "inst-" + this.task.taskId,
        sandbox_id: "sbx",
        runner_version: "0.0.0-dev",
        platform: { os: "linux", arch: "amd64" },
      }),
    });
    if (res.status === 200) this.token = ((await res.clone().json()) as { session_token: string }).session_token;
    return res;
  }
  async events(evs: { type: string; turn_id?: string; data?: unknown }[]): Promise<Response> {
    const batch = evs.map((e) => ({ seq: ++this.seq, at: new Date().toISOString(), ...e }));
    return runner("/runner/v1/session/events", {
      method: "POST",
      token: this.token,
      body: JSON.stringify({ events: batch }),
    });
  }
  async commands(): Promise<Command[]> {
    const res = await runner(`/runner/v1/session/commands?after=${this.cmdAck}&wait=0`, { token: this.token });
    if (res.status !== 200) throw new Error(`commands: ${res.status} ${await res.text()}`);
    const body = (await res.json()) as { commands: Command[] };
    for (const c of body.commands) this.cmdAck = Math.max(this.cmdAck, c.seq);
    return body.commands;
  }
  /** Starts the session and returns the first turn.start command. */
  async ready(): Promise<Command> {
    const s = await this.start();
    if (s.status !== 200) throw new Error(`start: ${s.status} ${await s.text()}`);
    await this.events([{ type: "session.ready", data: { capabilities: ["mid_turn_input"] } }]);
    const ts = (await this.commands()).find((c) => c.type === "turn.start");
    if (!ts) throw new Error("no turn.start");
    return ts;
  }
}
