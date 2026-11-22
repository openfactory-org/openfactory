// WorkerHubDO and worker records with a fake worker client in the Workers
// runtime: enrollment, the Ed25519 challenge, slots, drain, revoke (git
// credentials refused too), and the reconnect cases ported from Go:
// TestV2ReconnectRestoresStatuses (internal/sandbox/direct/v2_test.go) and
// TestWorkerRestartsDuringTurn (internal/dispatch/replica_test.go).
import { env, exports } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName } from "@openfactory/core";
import { parseProviderError, type SandboxTask, type TaskStatus } from "@openfactory/sandbox";
import { afterEach, describe, expect, it } from "vitest";
import { toWireStatus, workerAuthMessage, type WireFrame } from "../../../../../packages/sandbox/src/microvm/wire";
import type { CallMeta, Principal, TeamRpc } from "../../contracts/do-messages";
import { drainWorker, resumeWorker, revokeWorker } from "../../api/workers";
import { ROUTES } from "../../http/routes";
import type { RouteContext } from "../../http/handler";
import { preflightWorkerHosts, setFactoryAccess, setGitCredentialMinter } from "../team/workers";
import type { TeamKit } from "../team/index";
import type { CompiledDefinition } from "@openfactory/definition";
import { localProvider } from "./providers";

const TEAM = "default";
const ADMIN: Principal = { kind: "user", id: "usr_admin", teamId: TEAM, role: "admin", factoryRoles: {} };
const VIEWER: Principal = { kind: "user", id: "usr_view", teamId: TEAM, role: "user", factoryRoles: {} };

const team = (): TeamRpc => env.TEAM.getByName(doName.team(TEAM)) as unknown as TeamRpc;
const hub = (id: string) => env.WORKER_HUB.getByName(doName.workerHub(TEAM, id));
const meta = (principal?: Principal): CallMeta => ({ requestId: "req_test", ...(principal ? { principal } : {}) });

const b64url = (b: ArrayBuffer | Uint8Array): string =>
  btoa(String.fromCharCode(...new Uint8Array(b))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

async function op(name: string, params: Record<string, string> = {}, body: unknown = null, p: Principal = ADMIN): Promise<any> {
  return team().api(meta(p), name, params, body);
}

async function eventually<T>(what: string, fn: () => Promise<T | undefined | false>, ms = 3000): Promise<T> {
  const end = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v) return v as T;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

async function rejection(p: Promise<unknown>): Promise<string> {
  try {
    await p;
  } catch (e) {
    return parseProviderError(e)?.kind ?? `other:${String(e)}`;
  }
  return "resolved";
}

let seq = 0;

/** Enrolls a KVM host and returns its ID and key pair. */
async function enroll(opts: { slots?: number; name?: string } = {}): Promise<{ id: string; keys: CryptoKeyPair; publicKey: string }> {
  const e = await op("createWorkerEnrollment", {}, { labels: { gpu: "" } });
  expect(e.$status).toBe(201);
  const keys = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"])) as CryptoKeyPair;
  const publicKey = btoa(String.fromCharCode(...new Uint8Array((await crypto.subtle.exportKey("raw", keys.publicKey)) as ArrayBuffer)));
  const res = await exports.default.fetch("http://localhost/api/v1/workers/enroll", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      enrollment_token: e.body.token,
      name: opts.name ?? `host-${++seq}`,
      public_key: publicKey,
      facts: { arch: "amd64", vcpus: 8, memory_mib: 16384, kvm: true, runtime_version: "1.0.0" },
      labels: { linux: "" },
      slots: opts.slots ?? 2,
      slot_size: { vcpus: 2, memory_mib: 4096, scratch_gib: 20 },
    }),
  });
  expect(res.status).toBe(201);
  const out = (await res.json()) as { worker_id: string; control_url: string };
  expect(out.control_url).toBe("http://localhost");
  return { id: out.worker_id, keys, publicKey };
}

/** A fake worker: the Go worker's side of protocol v2 with a scripted backend. */
class FakeWorker {
  ws!: WebSocket;
  frames: WireFrame[] = [];
  closed: { code: number; reason: string } | null = null;
  tasks = new Map<string, TaskStatus>();
  requests: WireFrame[] = [];
  draining = false;
  waiters: (() => void)[] = [];

  constructor(
    readonly id: string,
    readonly keys: CryptoKeyPair | null,
  ) {}

  /** Connects, answers the challenge, sends hello and waits for welcome (or a close). */
  async connect(opts: { signAs?: string; protocolMinor?: number; extraTasks?: TaskStatus[] } = {}): Promise<number> {
    this.frames = [];
    this.closed = null;
    const res = await exports.default.fetch("http://localhost/api/v1/workers/connect", {
      headers: { upgrade: "websocket", "sec-websocket-protocol": "openfactory.worker.v2", "of-worker-id": this.id },
    });
    if (res.status !== 101 || !res.webSocket) return res.status;
    this.ws = res.webSocket;
    this.ws.accept();
    this.ws.addEventListener("message", (ev) => {
      const f = JSON.parse(String(ev.data)) as WireFrame;
      this.frames.push(f);
      if (f.type === "request") this.handle(f);
      this.wake();
    });
    this.ws.addEventListener("close", (ev) => {
      this.closed = { code: ev.code, reason: ev.reason };
      this.wake();
    });
    const ch = await this.next("auth.challenge");
    if (!ch) return 0;
    const p = ch.params as { nonce: string; connection_id: string };
    const sig = await crypto.subtle.sign(
      { name: "Ed25519" },
      this.keys!.privateKey,
      workerAuthMessage(opts.signAs ?? this.id, p.connection_id, p.nonce) as Uint8Array<ArrayBuffer>,
    );
    this.send({ type: "auth.response", params: { worker_id: this.id, alg: "ed25519", sig: b64url(sig) } });
    const tasks = [...this.tasks.values(), ...(opts.extraTasks ?? [])].map((s) => ({ task_id: s.taskId, status: toWireStatus(s) }));
    this.send({
      type: "hello",
      params: {
        worker_id: this.id,
        backend: "microvm",
        capacity: 0,
        version: "1.0.0",
        tasks,
        protocol_minor: opts.protocolMinor ?? 1,
        facts: { arch: "amd64", vcpus: 8, kvm: true },
        slots: 2,
        slot: { vcpus: 2, memory_mib: 4096 },
        draining: this.draining,
      },
    });
    const w = await this.next("welcome");
    return w ? 101 : ((this.closed as { code: number } | null)?.code ?? -1);
  }

  send(f: unknown): void {
    this.ws.send(JSON.stringify(f));
  }

  ping(): void {
    this.ws.send('{"type":"ping"}');
  }

  /** Reports a status (task.state event) and keeps it as the backend's. */
  report(s: TaskStatus): void {
    this.tasks.set(s.taskId, s);
    this.send({ type: "event", method: "task.state", params: { status: toWireStatus(s) } });
  }

  drop(): void {
    this.ws.close(1001, "restart");
  }

  private wake(): void {
    const ws = this.waiters;
    this.waiters = [];
    for (const w of ws) w();
  }

  /** The next frame of a type, or null when the socket closes first. */
  async next(type: string, timeoutMs = 3000): Promise<WireFrame | null> {
    const end = Date.now() + timeoutMs;
    for (;;) {
      const i = this.frames.findIndex((f) => f.type === type);
      if (i >= 0) return this.frames.splice(i, 1)[0]!;
      if (this.closed) return null;
      if (Date.now() > end) throw new Error(`no ${type} frame`);
      await new Promise<void>((r) => {
        this.waiters.push(r);
        setTimeout(r, 50);
      });
    }
  }

  private handle(f: WireFrame): void {
    this.requests.push(f);
    const reply = (result?: unknown, error?: { code: string; message: string }) =>
      this.send({ type: "response", id: f.id, ...(error ? { error } : { result }) });
    const p = (f.params ?? {}) as { task?: { task_id: string }; task_id?: string; draining?: boolean };
    switch (f.method) {
      case "task.assign": {
        if (this.draining) return reply(undefined, { code: "at_capacity", message: "draining" });
        const id = p.task!.task_id;
        const s = this.tasks.get(id) ?? { taskId: id, state: "accepted" as const };
        this.tasks.set(id, s);
        return reply({ status: toWireStatus(s) });
      }
      case "task.status": {
        const s = this.tasks.get(p.task_id!);
        return s ? reply({ status: toWireStatus(s) }) : reply(undefined, { code: "not_found", message: "no task" });
      }
      case "task.cancel":
        return reply({});
      case "task.destroy":
        if (!this.tasks.delete(p.task_id!)) return reply(undefined, { code: "not_found", message: "no task" });
        return reply({});
      case "task.list":
        return reply({ tasks: [...this.tasks.values()].map((s) => ({ task_id: s.taskId, status: toWireStatus(s) })) });
      case "drain":
        this.draining = !!p.draining;
        return reply({});
    }
    reply(undefined, { code: "unknown_method", message: String(f.method) });
  }

  assigns(): number {
    return this.requests.filter((r) => r.method === "task.assign").length;
  }
}

function task(n: number, run = "run_wabcdefghijklmnoabcdefghijkl"): SandboxTask {
  return {
    taskId: `${run}.${n}`,
    runId: run,
    session: n,
    teamId: TEAM,
    factoryId: "fac_1",
    image: "ghcr.io/example/sandbox:1",
    platform: { os: "linux", arch: "amd64" },
    resources: { vcpus: 1, memoryMiB: 2048 },
    env: { OF_CONTROL_URL: "http://localhost", OF_RUN_ID: run, OF_SESSION: String(n), OF_BOOTSTRAP_TOKEN: "ofb_default.fake.fake" },
    maxSessionSeconds: 3600,
    failureRetentionSeconds: 0,
  };
}

function routeCtx(operationId: string, params: Record<string, string>, principal: Principal = ADMIN): RouteContext {
  const route = ROUTES.find((r) => r.operationId === operationId)!;
  return {
    params,
    url: new URL(`http://localhost/api/v1${route.path}`),
    requestId: "req_test",
    route,
    executionCtx: { waitUntil() {}, passThroughOnException() {}, props: {} } as unknown as ExecutionContext,
    principal,
  };
}

async function workerView(id: string): Promise<any> {
  return op("getWorker", { id }, null, VIEWER);
}

afterEach(() => {
  setGitCredentialMinter(null);
  setFactoryAccess(null);
});

describe("enrollment", () => {
  it("enrolls a KVM host with a single-use token and lists it as pending", async () => {
    const e = await op("createWorkerEnrollment", {}, { name: "build-01", factories: ["fac_a"] });
    expect(e.body.token).toMatch(/^ofe_/);
    expect(e.body.command).toContain("openfactory-runtime worker enroll --url http://localhost --token ofe_");
    const keys = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"])) as CryptoKeyPair;
    const pk = btoa(String.fromCharCode(...new Uint8Array((await crypto.subtle.exportKey("raw", keys.publicKey)) as ArrayBuffer)));
    const body = {
      enrollment_token: e.body.token,
      name: "ignored-host-name",
      public_key: pk,
      facts: { arch: "amd64", vcpus: 4, kvm: true },
      slots: 1,
      slot_size: { vcpus: 2, memory_mib: 2048 },
    };
    const post = (b: unknown) =>
      exports.default.fetch("http://localhost/api/v1/workers/enroll", { method: "POST", body: JSON.stringify(b) });
    const ok = await post(body);
    expect(ok.status).toBe(201);
    const { worker_id } = (await ok.json()) as { worker_id: string };
    const again = await post(body);
    expect(again.status).toBe(401);
    const v = await workerView(worker_id);
    expect(v).toMatchObject({ name: "build-01", status: "pending", kind: "microvm", slots: 1, factories: ["fac_a"] });
    const list = await op("listWorkers", {}, null, VIEWER);
    expect(list.items.some((w: { id: string }) => w.id === worker_id)).toBe(true);
  });

  it("refuses a host without KVM and consumes the token", async () => {
    const e = await op("createWorkerEnrollment");
    const body = {
      enrollment_token: e.body.token,
      name: "nokvm",
      public_key: btoa("x".repeat(32)),
      facts: { arch: "amd64", vcpus: 2, kvm: false },
      slots: 1,
      slot_size: { vcpus: 1, memory_mib: 1024 },
    };
    const res = await exports.default.fetch("http://localhost/api/v1/workers/enroll", { method: "POST", body: JSON.stringify(body) });
    expect(res.status).toBe(400);
    expect(((await res.json()) as { detail: string }).detail).toContain("/dev/kvm");
    const again = await exports.default.fetch("http://localhost/api/v1/workers/enroll", {
      method: "POST",
      body: JSON.stringify({ ...body, facts: { ...body.facts, kvm: true } }),
    });
    expect(again.status).toBe(401);
  });

  it("refuses a development process worker unless OF_DEV_PROCESS_WORKERS is true", async () => {
    const r = await op("workers.devRegister", { id: "dev-1" });
    expect(r.$error.code).toBe("not_authorized");
  });
});

describe("connection", () => {
  it("authenticates with the Ed25519 challenge and goes online", async () => {
    const { id, keys } = await enroll();
    const w = new FakeWorker(id, keys);
    expect(await w.connect()).toBe(101);
    w.ping();
    expect(await w.next("pong")).toBeTruthy();
    await eventually("online", async () => (await workerView(id)).status === "online");
    const live = await hub(id).live();
    expect(live).toMatchObject({ online: true, slotsUsed: 0, version: "1.0.0" });
    await runInDurableObject(hub(id), async (_i, state) => {
      // capacity.report rows go to DispatchDO and are deleted once delivered.
      const gen = state.storage.sql.exec("SELECT v FROM hub_state WHERE k = 'generation'").toArray();
      expect(Number(gen[0]?.v ?? 0)).toBeGreaterThan(0);
      const dead = state.storage.sql.exec("SELECT COUNT(*) AS n FROM outbox WHERE state = 'dead'").one();
      expect(dead.n).toBe(0);
    });
  });

  it("closes a connection with a bad signature (1008 auth_failed)", async () => {
    const { id, keys } = await enroll();
    const w = new FakeWorker(id, keys);
    expect(await w.connect({ signAs: "someone-else" })).toBe(1008);
    expect(w.closed?.reason).toBe("auth_failed");
  });

  it("refuses an unknown worker and a runtime below the minimum protocol", async () => {
    const stranger = new FakeWorker("wkr_unknownunknownunkno", null);
    expect(await stranger.connect()).toBe(401);
    const { id, keys } = await enroll();
    const old = new FakeWorker(id, keys);
    expect(await old.connect({ protocolMinor: 0 })).toBe(1008);
    expect(old.closed?.reason).toBe("runtime_too_old");
  });

  it("ignores tasks in hello that were never leased to the worker", async () => {
    const { id, keys } = await enroll();
    const w = new FakeWorker(id, keys);
    await w.connect({ extraTasks: [{ taskId: "run_other.1", state: "running" }] });
    expect(await rejection(hub(id).taskStatus("run_other.1"))).toBe("not_found");
  });
});

describe("slots", () => {
  it("fills the slots, refuses at capacity and frees a slot when a task exits", async () => {
    const { id, keys } = await enroll({ slots: 2 });
    const w = new FakeWorker(id, keys);
    await w.connect();
    expect((await hub(id).taskAssign(task(1))).state).toBe("accepted");
    expect((await hub(id).taskAssign(task(2))).state).toBe("accepted");
    // Idempotent on task ID.
    await hub(id).taskAssign(task(1));
    expect(w.assigns()).toBe(2);
    expect(await rejection(hub(id).taskAssign(task(3)))).toBe("at_capacity");
    w.report({ taskId: task(1).taskId, state: "exited", reason: "completed", exitCode: 0 });
    await eventually("exited", async () => (await hub(id).taskStatus(task(1).taskId)).state === "exited");
    // The exit goes to the run's WorkDO (sandboxState) through the outbox and is delivered.
    await eventually("exit delivered to WorkDO", () =>
      runInDurableObject(hub(id), async (_i, state) => {
        const n = state.storage.sql.exec("SELECT COUNT(*) AS n FROM outbox WHERE kind = 'sandbox.state'").one().n;
        return n === 0;
      }),
    );
    expect((await hub(id).taskAssign(task(3))).state).toBe("accepted");
    await eventually("slots used", async () => (await workerView(id)).slots_used === 2);
  });

  it("refuses an assign while the worker is not connected", async () => {
    const { id } = await enroll();
    expect(await rejection(hub(id).taskAssign(task(1)))).toBe("unavailable");
  });
});

describe("drain and revoke", () => {
  it("drain stops new tasks and tells the worker; resume undoes it", async () => {
    const { id, keys } = await enroll();
    const w = new FakeWorker(id, keys);
    await w.connect();
    const req = () => new Request("http://localhost/x", { method: "POST" });
    const res = await drainWorker(req(), env, routeCtx("drainWorker", { id }));
    expect(res.status).toBe(200);
    expect(((await res.json()) as { status: string }).status).toBe("draining");
    await eventually("drain request", async () => w.draining);
    expect(await rejection(hub(id).taskAssign(task(1)))).toBe("at_capacity");
    await resumeWorker(req(), env, routeCtx("resumeWorker", { id }));
    await eventually("resume request", async () => !w.draining);
    expect((await hub(id).taskAssign(task(1))).state).toBe("accepted");
  });

  it("revoke closes the socket, refuses connections and git credentials", async () => {
    const { id, keys } = await enroll();
    setGitCredentialMinter(async (_kit, input) => ({ username: "x-access-token", password: `fake-${input.repo}`, expiresAt: "2026-11-22T01:00:00Z" }));
    const w = new FakeWorker(id, keys);
    await w.connect();
    const workerP: Principal = { kind: "worker", id, teamId: TEAM, role: "user", factoryRoles: {} };
    const repo = "https://github.com/acme/app.git";
    // Not sent to this worker yet: refused.
    expect((await op("getWorkerGitCredential", { id, "?repo": repo }, null, workerP)).$error.code).toBe("not_authorized");
    await hub(id).taskAssign(task(1), { git_cache: [{ key: "k", clone_url: repo }] });
    const cred = await op("getWorkerGitCredential", { id, "?repo": repo }, null, workerP);
    expect(cred).toMatchObject({ username: "x-access-token", password: "fake-https://github.com/acme/app", expires_at: "2026-11-22T01:00:00Z" });
    // Another worker cannot ask for this worker's credentials.
    const other: Principal = { ...workerP, id: "wkr_other" };
    expect((await op("getWorkerGitCredential", { id, "?repo": repo }, null, other)).$error.code).toBe("not_authorized");

    const res = await revokeWorker(new Request("http://localhost/x", { method: "POST" }), env, routeCtx("revokeWorker", { id }));
    expect(res.status).toBe(200);
    await eventually("closed", async () => w.closed);
    expect(w.closed).toMatchObject({ code: 1008, reason: "revoked" });
    expect((await workerView(id)).status).toBe("revoked");
    expect(await w.connect()).toBe(403);
    expect((await op("getWorkerGitCredential", { id, "?repo": repo }, null, workerP)).$error.code).toBe("not_authorized");
    expect(await rejection(hub(id).taskAssign(task(2)))).toBe("unavailable");
  });
});

describe("reconnect", () => {
  // Port of TestV2ReconnectRestoresStatuses.
  it("restores statuses from hello after a reconnect", async () => {
    const { id, keys } = await enroll();
    const w = new FakeWorker(id, keys);
    await w.connect();
    const a = task(1);
    const b = task(2);
    await hub(id).taskAssign(a);
    await hub(id).taskAssign(b);
    w.report({ taskId: a.taskId, state: "running" });
    await eventually("running", async () => (await hub(id).taskStatus(a.taskId)).state === "running");

    // Drop the connection. The hub keeps the last known statuses.
    w.drop();
    await eventually("offline", async () => !(await hub(id).live()).online);
    expect((await hub(id).taskStatus(a.taskId)).state).toBe("running");
    expect(await rejection(hub(id).taskCancel(a.taskId, 0))).toBe("unavailable");

    // The task exits while the worker has no connection, and the other one disappears.
    w.tasks.set(a.taskId, { taskId: a.taskId, state: "exited", reason: "process_failed", exitCode: 3 });
    w.tasks.delete(b.taskId);
    expect(await w.connect()).toBe(101);
    await eventually("restored", async () => {
      const s = await hub(id).taskStatus(a.taskId);
      return s.state === "exited" && s.reason === "process_failed";
    });
    expect(await rejection(hub(id).taskStatus(b.taskId))).toBe("not_found");
    const list = await hub(id).taskList();
    expect(list.map((s) => s.taskId)).toEqual([a.taskId]);
  });

  // Port of TestWorkerRestartsDuringTurn (the hub's part): a worker restart
  // during a turn neither fails the session nor starts the task again.
  it("keeps a running task across a worker restart", async () => {
    const { id, keys } = await enroll();
    const w = new FakeWorker(id, keys);
    await w.connect();
    const t = task(1, "run_wzyxwvutsrqponmlkzyxwvutsrqp");
    await hub(id).taskAssign(t);
    w.report({ taskId: t.taskId, state: "running" });
    await eventually("running", async () => (await hub(id).taskStatus(t.taskId)).state === "running");

    // The worker restarts: no connection for a while. Status answers from the
    // cache, so the run's liveness check sees the task, not a loss.
    w.drop();
    await eventually("offline", async () => !(await hub(id).live()).online);
    expect((await hub(id).taskStatus(t.taskId)).state).toBe("running");

    // The worker is back and holds the task. The hub does not assign it again.
    expect(await w.connect()).toBe(101);
    expect((await hub(id).taskStatus(t.taskId)).state).toBe("running");
    expect(w.assigns()).toBe(1);
    w.report({ taskId: t.taskId, state: "exited", reason: "completed", exitCode: 0 });
    await eventually("exited", async () => (await hub(id).taskStatus(t.taskId)).state === "exited");
    await hub(id).taskDestroy(t.taskId);
    expect(await rejection(hub(id).taskStatus(t.taskId))).toBe("not_found");
    expect(w.tasks.has(t.taskId)).toBe(false);
  });

  it("keeps a destroy asked while the worker is away and sends it at the next hello", async () => {
    const { id, keys } = await enroll();
    const w = new FakeWorker(id, keys);
    await w.connect();
    await hub(id).taskAssign(task(1));
    w.drop();
    await eventually("offline", async () => !(await hub(id).live()).online);
    await hub(id).taskDestroy(task(1).taskId);
    expect(await rejection(hub(id).taskStatus(task(1).taskId))).toBe("not_found");
    await w.connect();
    await eventually("destroyed on the worker", async () => !w.tasks.has(task(1).taskId));
  });

  it("replaces an older connection of the same worker", async () => {
    const { id, keys } = await enroll();
    const first = new FakeWorker(id, keys);
    await first.connect();
    const second = new FakeWorker(id, keys);
    await second.connect();
    await eventually("replaced", async () => first.closed);
    expect(first.closed).toMatchObject({ code: 1001 });
    expect((await hub(id).live()).online).toBe(true);
  });
});

describe("providers and machines", () => {
  it("the local provider finds no development worker and asks to requeue", async () => {
    const p = localProvider(env, TEAM);
    expect((await p.capabilities()).egress).toBe("none");
    expect(await rejection(p.start(task(9, "run_wlocallocallocallocallocal")))).toBe("at_capacity");
  });

  it("lists the machines of active definitions, sorted, with names filled", async () => {
    setFactoryAccess({
      listFactories: () => [{ id: "fac_b", name: "beta" }],
      activeMachines: () => ({ large: { vcpus: 8, memoryGb: 16 }, small: { name: "tiny", vcpus: 1 } }),
    });
    const r = await op("listMachines", {}, null, VIEWER);
    expect(r.items).toEqual([
      { factory_id: "fac_b", factory_name: "beta", name: "large", vcpus: 8, memoryGb: 16 },
      { factory_id: "fac_b", factory_name: "beta", name: "tiny", vcpus: 1 },
    ]);
    expect(r.page_info).toEqual({ has_next_page: false });
  });

  it("preflight refuses an unknown workerHost and accepts workers, pools and providers", async () => {
    const { id } = await enroll({ name: "pre-host" });
    const def = {
      agents: [
        { name: "a", worker_host: "pre-host" },
        { name: "b", worker_host: id },
        { name: "c", worker_host: "pool:linux" },
        { name: "d", worker_host: "daytona" },
        { name: "e", worker_host: "server" },
        { name: "f", worker_host: "nowhere" },
        { name: "g", worker_host: "pool:gpu-none" },
      ],
      automations: [{ name: "x", overrides: { worker_host: "nowhere" } }],
    } as unknown as CompiledDefinition;
    const chk = { definition: null, errors: [] as { file: string; field: string; message: string }[], warnings: [], skipped: [] };
    await runInDurableObject(env.TEAM.getByName(doName.team(TEAM)), async (_i, state) => {
      preflightWorkerHosts({ sql: state.storage.sql } as unknown as TeamKit, def, chk as never);
    });
    expect(chk.errors.map((e) => `${e.file}|${e.field}`)).toEqual([
      "agents/f/agent.md|workerHost",
      "agents/g/agent.md|workerHost",
      "automations/x/automation.md|workerHost",
    ]);
  });
});
