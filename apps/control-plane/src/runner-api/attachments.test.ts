// Ports of internal/dispatch/attachments_test.go (TestToItemAttachmentLookup)
// and internal/runnerapi/attachments_test.go (TestAttachmentDownload) with a
// fake runner in the Workers runtime. Also: the task image follows the Go
// precedence (internal/server/taskimage_test.go), and the runner conversation
// merges compacted messages.
import { env } from "cloudflare:workers";
import { doName, newRoutedId, newToken } from "@openfactory/core";
import type { ProviderCapabilities, SandboxProvider, SandboxTask, TaskStatus } from "@openfactory/sandbox";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { Env } from "../contracts/env";
import type { CallMeta, Principal } from "../contracts/do-messages";
import { registerProvider, unregisterProvider } from "../do/dispatch/providers";
import { DEFAULT_SANDBOX_IMAGE } from "../do/work/engine";
import { selectImage } from "../do/work/grant";
import type { WorkDO } from "../do/work/index";
import { toItem } from "../engine/deliver";
import type { AgentConfig, Run, RunInput } from "../engine/types";
import { expandAttachments, handleRunnerApi } from "./index";
import { platformDeps, setPlannerDeps } from "./planner";

const TEAM = "default";
const ADMIN: Principal = { kind: "user", id: "usr_admin", teamId: TEAM, role: "admin", factoryRoles: {} };
const meta: CallMeta = { requestId: "req_test", principal: ADMIN };

class FakeProvider implements SandboxProvider {
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
}

const agent = (extra: Partial<AgentConfig> = {}): AgentConfig => ({
  id: "agt_foreman",
  factory_id: "fac_test",
  name: "foreman",
  description: "",
  type: "FOREMAN",
  instructions: "You are foreman.",
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

const work = (token: string) => env.WORK.getByName(doName.work(TEAM, token)) as unknown as WorkDO;

async function pump(token: string): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await work(token).pump();
    await (env.DISPATCH.getByName(doName.dispatch(TEAM)) as unknown as { pump(): Promise<void> }).pump();
  }
}

/** env with R2 presign settings (test values), so downloads answer. */
const r2Env = { ...env, OF_R2_ACCOUNT_ID: "0123abcd", OF_R2_ACCESS_KEY_ID: "test-id", OF_R2_SECRET_ACCESS_KEY: "test" };

async function runner(path: string, token: string, init: RequestInit = {}, e: Env = env): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set("authorization", `Bearer ${token}`);
  if (init.body) headers.set("content-type", "application/json");
  return handleRunnerApi(new Request(`http://localhost${path}`, { ...init, headers }), e);
}

type Cmd = { seq: number; type: string; data?: { turn_id?: string; input?: Record<string, unknown>[] } };

class FakeRunner {
  seq = 0;
  ack = 0;
  token = "";
  constructor(readonly task: SandboxTask) {}
  async start(): Promise<void> {
    const res = await runner("/runner/v1/session/start", this.task.env.OF_BOOTSTRAP_TOKEN!, {
      method: "POST",
      body: JSON.stringify({
        instance_id: "inst-" + this.task.taskId,
        sandbox_id: "sbx",
        runner_version: "0.0.0-dev",
        platform: { os: "linux", arch: "amd64" },
      }),
    });
    expect(res.status, await res.clone().text()).toBe(200);
    this.token = ((await res.json()) as { session_token: string }).session_token;
  }
  async events(evs: { type: string; turn_id?: string; data?: unknown }[]): Promise<void> {
    const batch = evs.map((e) => ({ seq: ++this.seq, at: new Date().toISOString(), ...e }));
    const res = await runner("/runner/v1/session/events", this.token, {
      method: "POST",
      body: JSON.stringify({ events: batch }),
    });
    expect(res.status).toBe(200);
  }
  async commands(): Promise<Cmd[]> {
    const res = await runner(`/runner/v1/session/commands?after=${this.ack}&wait=0`, this.token);
    expect(res.status).toBe(200);
    const cmds = ((await res.json()) as { commands: Cmd[] }).commands;
    for (const c of cmds) this.ack = Math.max(this.ack, c.seq);
    return cmds;
  }
  async conversation(): Promise<unknown> {
    const res = await runner("/runner/v1/session/conversation", this.token);
    expect(res.status).toBe(200);
    return res.json();
  }
}

async function newRun(token: string, config: AgentConfig = agent()): Promise<Run> {
  const out = await work(token).runsCreate(
    { requestId: "req-1" },
    {
      dispatch: {
        factoryId: "fac_test",
        config,
        prompt: "Fix the bug",
        actor: "usr_1",
        title: "Fix the bug",
        workItemId: newRoutedId("wi", token),
      },
    },
  );
  return out.run as Run;
}

const taskFor = (runId: string): SandboxTask => {
  const t = provider.started.filter((x) => x.runId === runId).at(-1);
  if (!t) throw new Error(`no task for ${runId}`);
  return t;
};

/** A run with a live session whose first turn has ended (the run is succeeded). */
async function liveRun(): Promise<{ token: string; run: Run; fr: FakeRunner }> {
  const token = newToken("w");
  const run = await newRun(token);
  await pump(token);
  const fr = new FakeRunner(taskFor(run.id));
  await fr.start();
  await fr.events([{ type: "session.ready", data: { capabilities: [] } }]);
  const [ts] = await fr.commands();
  const turnId = ts!.data!.turn_id!;
  await fr.events([
    { type: "turn.started", turn_id: turnId },
    { type: "message", turn_id: turnId, data: { role: "assistant", content: "looked at it" } },
    { type: "turn.ended", turn_id: turnId, data: { outcome: "done", result: "ok" } },
  ]);
  return { token, run, fr };
}

describe("attachment delivery (dispatch attachments_test)", () => {
  it("toItem fills attachment metadata from the lookup and keeps the ID of a failed one", () => {
    const m = {
      message_id: "msg_1",
      run_id: "run_1",
      kind: "user",
      sender: { type: "user" },
      content: "x",
      attachments: ["att_1", "att_gone"],
    } as unknown as RunInput;
    const lookup = (id: string) =>
      id === "att_gone" ? undefined : { name: "a.txt", content_type: "text/plain", bytes: 7 };
    const item = toItem(m, lookup);
    expect(item.attachments).toEqual([
      { id: "att_1", name: "a.txt", content_type: "text/plain", bytes: 7 },
      { id: "att_gone", name: "", content_type: "", bytes: 0 },
    ]);
    expect(toItem(m).attachments![0]).toEqual({ id: "att_1", name: "", content_type: "", bytes: 0 });
  });

  it("expandAttachments looks up each attachment once in the team", async () => {
    await env.DATA.put("attachments/default/att_exp1/notes.md", "# notes", {
      httpMetadata: { contentType: "text/markdown" },
    });
    const cmds = [
      {
        data: {
          input: [
            {
              attachments: [
                { id: "att_exp1", name: "", content_type: "", bytes: 0 },
                { id: "att_expgone", name: "", content_type: "", bytes: 0 },
              ],
            },
          ],
        },
      },
    ];
    await expandAttachments(env, TEAM, cmds);
    expect(cmds[0]!.data.input[0]!.attachments).toEqual([
      { id: "att_exp1", name: "notes.md", content_type: "text/markdown", bytes: 7 },
      { id: "att_expgone", name: "", content_type: "", bytes: 0 },
    ]);
  });

  it("delivers a message with attachments and serves only the run's own attachments", async () => {
    const { token, run, fr } = await liveRun();
    await env.DATA.put("attachments/default/att_dl1/a.txt", "content", { httpMetadata: { contentType: "text/plain" } });
    await env.DATA.put("attachments/default/att_dl2/b.txt", "b");
    await env.DATA.put("attachments/other-team/att_dl3/c.txt", "c");
    await work(token).runsMessage(meta, run.id, {
      message: "see files",
      actor: "usr_1",
      attachments: ["att_dl1", "att_dl3"],
    });
    const turn = (await fr.commands()).find((c) => c.type === "turn.start");
    const item = turn!.data!.input!.find((i) => i.content === "see files")!;
    expect(item.attachments).toEqual([
      { id: "att_dl1", name: "a.txt", content_type: "text/plain", bytes: 7 },
      { id: "att_dl3", name: "", content_type: "", bytes: 0 },
    ]);

    const get = (q: string) => runner(`/runner/v1/session/downloads?kind=attachment${q}`, fr.token, {}, r2Env);
    const ok = await get("&id=att_dl1");
    expect(ok.status, await ok.clone().text()).toBe(200);
    const files = ((await ok.json()) as { files: { part: string; name: string; bytes: number; url: string }[] }).files;
    expect(files).toHaveLength(1);
    expect(files[0]).toMatchObject({ part: "attachment", name: "a.txt", bytes: 7 });
    expect(files[0]!.url).toContain("/openfactory-data/attachments/default/att_dl1/a.txt?");
    expect((await get("&id=att_dl2")).status).toBe(404); // not in the inputs of the run
    expect((await get("&id=att_dl3")).status).toBe(404); // another team
    expect((await get("&id=att_nope")).status).toBe(404);
    expect((await get("")).status).toBe(400);
  });
});

describe("runner conversation", () => {
  it("merges the compacted messages of the run", async () => {
    const { token, run, fr } = await liveRun();
    const before = await fr.conversation();
    expect(JSON.stringify(before)).toContain("looked at it");
    expect((await work(token).getRun(run.id)).state).toBe("succeeded");
    await work(token).compactRun(run.id);
    const rows = (await work(token).api(meta, "listRunMessages", { runId: run.id }, null)) as {
      messages: unknown[];
      compacted_upto?: number;
    };
    expect(rows.messages).toHaveLength(0);
    expect(rows.compacted_upto).toBeGreaterThan(0);
    expect(await fr.conversation()).toEqual(before);
  });
});

describe("task image (server taskimage_test)", () => {
  it("selects the environment image, then the machine image, then the default", () => {
    expect(selectImage(undefined, undefined, "def:1")).toBe("def:1");
    expect(selectImage(undefined, "machine:1", "def:1")).toBe("machine:1");
    expect(selectImage(" env:1 ", "machine:1", "def:1")).toBe("env:1");
    expect(selectImage("", "machine:1", "def:1")).toBe("machine:1");
  });

  it("starts the task with the image of the run's environment", async () => {
    const team = env.TEAM.getByName(doName.team(TEAM));
    const created = (await team.api(
      meta,
      "createEnvironment",
      {},
      {
        name: "img-" + crypto.randomUUID().slice(0, 6),
        image: "example.test/env:1",
      },
    )) as { body: { id: string } };
    const plain = (await team.api(
      meta,
      "createEnvironment",
      {},
      {
        name: "plain-" + crypto.randomUUID().slice(0, 6),
      },
    )) as { body: { id: string } };
    const machine = { name: "big", os: "linux", arch: "amd64", image: "example.test/machine:1" };
    const cases: { name: string; cfg: Partial<AgentConfig>; want: string }[] = [
      { name: "nothing set", cfg: {}, want: DEFAULT_SANDBOX_IMAGE },
      { name: "machine image", cfg: { machine }, want: "example.test/machine:1" },
      {
        name: "environment beats machine",
        cfg: { machine, environment_id: created.body.id },
        want: "example.test/env:1",
      },
      {
        name: "environment without image",
        cfg: { machine, environment_id: plain.body.id },
        want: "example.test/machine:1",
      },
      { name: "missing environment", cfg: { machine, environment_id: "env_gone" }, want: "example.test/machine:1" },
    ];
    for (const c of cases) {
      const token = newToken("w");
      const run = await newRun(token, agent(c.cfg as Partial<AgentConfig>));
      await pump(token);
      expect(taskFor(run.id).image, c.name).toBe(c.want);
    }
  });
});
