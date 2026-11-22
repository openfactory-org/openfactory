// A fake worker that speaks worker protocol v2 (WebSocket subprotocol
// openfactory.worker.v2 at /api/v1/workers/connect). Every frame in both
// directions is validated against schemas/worker/v2/protocol.json.
//
// Auth: an enrolled worker answers auth.challenge with an Ed25519 signature;
// a development process worker sends a team API key as a bearer header
// (OF_DEV_PROCESS_WORKERS=true on the control plane).
import type { Loose } from "./loose.ts";
import { generateKeyPairSync, sign, type KeyObject } from "node:crypto";
import { assertWorkerFrame } from "./schemas.ts";
import { target } from "./target.ts";
import type { TaskEnv } from "./runner.ts";

export const WORKER_SUBPROTOCOL = "openfactory.worker.v2";

export type WorkerAuth =
  { kind: "ed25519"; workerId: string; privateKey: KeyObject } | { kind: "bearer"; token: string };

export interface WorkerKeyPair {
  privateKey: KeyObject;
  /** Base64 (standard) of the raw 32-byte public key, for POST /workers/enroll. */
  publicKeyB64: string;
}

export function newWorkerKey(): WorkerKeyPair {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const jwk = publicKey.export({ format: "jwk" }) as { x: string };
  const raw = Buffer.from(jwk.x, "base64url");
  return { privateKey, publicKeyB64: raw.toString("base64") };
}

export function signChallenge(privateKey: KeyObject, workerId: string, connectionId: string, nonce: string): string {
  const msg = `openfactory-worker-auth-v1\n${workerId}\n${connectionId}\n${nonce}\n`;
  return sign(null, Buffer.from(msg, "utf8"), privateKey).toString("base64url");
}

export interface Frame {
  type: string;
  id?: string;
  method?: string;
  params?: Loose;
  result?: Loose;
  error?: { code: string; message: string };
}

export interface AssignedTask {
  task_id: string;
  run_id: string;
  session: number;
  env: TaskEnv;
  [k: string]: unknown;
}

type RequestHandler = (frame: Frame) => { result?: unknown; error?: { code: string; message: string } } | undefined;

export interface HelloOptions {
  workerId: string;
  name?: string;
  backend?: string;
  capacity?: number;
  labels?: Record<string, string>;
  slots?: number;
  draining?: boolean;
}

export class FakeWorker {
  readonly received: Frame[] = [];
  /** Schema violations of received frames (the test asserts that it stays empty). */
  readonly invalid: string[] = [];
  welcome: Loose;
  closed = false;
  closeCode: number | undefined;
  private waiters: { pred: (f: Frame) => boolean; resolve: (f: Frame) => void }[] = [];
  private handler: RequestHandler | undefined;
  readonly tasks = new Map<string, AssignedTask>();
  private taskWaiters: { runId: string; session: number; resolve: (t: AssignedTask) => void }[] = [];

  private constructor(private readonly ws: WebSocket) {}

  /** Connects, authenticates, sends hello and waits for welcome. */
  static async connect(auth: WorkerAuth, hello: HelloOptions, timeoutMs = 15_000): Promise<FakeWorker> {
    const url = target.baseUrl.replace(/^http/, "ws") + "/api/v1/workers/connect";
    // The Go runtime sends OF-Worker-ID on the upgrade so the hub can route
    // the connection before any frame arrives (internal/sandbox/direct).
    const headers: Record<string, string> = { "OF-Worker-ID": hello.workerId };
    if (auth.kind === "bearer") headers.authorization = `Bearer ${auth.token}`;
    // Node's WebSocket (undici) accepts {protocols, headers}.
    const Ctor = WebSocket as unknown as new (
      u: string,
      init: { protocols: string[]; headers: Record<string, string> },
    ) => WebSocket;
    const ws = new Ctor(url, { protocols: [WORKER_SUBPROTOCOL], headers });
    const w = new FakeWorker(ws);
    // Listen before "open": the control plane sends auth.challenge at once,
    // and undici can deliver it in the same task as the open event.
    w.attach();
    await new Promise<void>((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`worker connect to ${url}: timeout`)), timeoutMs);
      ws.addEventListener("open", () => {
        clearTimeout(t);
        resolve();
      });
      ws.addEventListener("error", (ev) => {
        clearTimeout(t);
        reject(new Error(`worker connect to ${url}: ${(ev as unknown as { message?: string }).message ?? "error"}`));
      });
      ws.addEventListener("close", (ev) => {
        clearTimeout(t);
        reject(new Error(`worker connect to ${url}: closed ${ev.code} ${ev.reason}`));
      });
    });
    if (ws.protocol !== WORKER_SUBPROTOCOL) {
      w.close();
      throw new Error(`worker connect: server selected subprotocol "${ws.protocol}", want ${WORKER_SUBPROTOCOL}`);
    }
    if (auth.kind === "ed25519") {
      const ch = await w.waitFrame((f) => f.type === "auth.challenge", timeoutMs);
      w.send({
        type: "auth.response",
        params: {
          worker_id: auth.workerId,
          alg: "ed25519",
          sig: signChallenge(auth.privateKey, auth.workerId, ch.params.connection_id, ch.params.nonce),
        },
      });
    }
    const params: Record<string, unknown> = {
      worker_id: hello.workerId,
      backend: hello.backend ?? "process",
      capacity: hello.capacity ?? 4,
      tasks: [],
      version: "1.0.0-contract",
    };
    // An enrolled worker speaks protocol v2.1 (auth.challenge), as the Go
    // worker agent does; the bearer (API key) worker stays at v2.0.
    if (auth.kind === "ed25519") params.protocol_minor = 1;
    if (hello.name) params.name = hello.name;
    if (hello.labels) params.labels = hello.labels;
    if (hello.slots !== undefined) params.slots = hello.slots;
    if (hello.draining !== undefined) params.draining = hello.draining;
    w.send({ type: "hello", params });
    const welcome = await w.waitFrame((f) => f.type === "welcome", timeoutMs);
    w.welcome = welcome.result;
    return w;
  }

  private attach(): void {
    this.ws.addEventListener("message", (ev) => {
      const data = typeof ev.data === "string" ? ev.data : Buffer.from(ev.data as ArrayBuffer).toString("utf8");
      let frame: Frame;
      try {
        frame = JSON.parse(data) as Frame;
      } catch {
        this.invalid.push(`not JSON: ${data.slice(0, 200)}`);
        return;
      }
      try {
        assertWorkerFrame(frame);
      } catch (e) {
        this.invalid.push(String(e));
      }
      this.received.push(frame);
      this.dispatch(frame);
      this.waiters = this.waiters.filter((w) => {
        if (w.pred(frame)) {
          w.resolve(frame);
          return false;
        }
        return true;
      });
    });
    this.ws.addEventListener("close", (ev) => {
      this.closed = true;
      this.closeCode = ev.code;
    });
  }

  private dispatch(f: Frame): void {
    if (f.type === "ping") {
      this.send({ type: "pong" });
      return;
    }
    if (f.type !== "request" || !f.id) return;
    const custom = this.handler?.(f);
    if (custom) {
      this.respond(f.id, custom);
      return;
    }
    switch (f.method) {
      case "task.assign": {
        const task = f.params.task as AssignedTask;
        this.tasks.set(task.task_id, task);
        const status = {
          task_id: task.task_id,
          state: "running",
          sandbox_id: `fake-${task.task_id}`,
          started_at: new Date().toISOString(),
        };
        this.respond(f.id, { result: { status } });
        this.taskWaiters = this.taskWaiters.filter((w) => {
          if (w.runId === task.run_id && w.session === task.session) {
            w.resolve(task);
            return false;
          }
          return true;
        });
        return;
      }
      case "task.status":
      case "task.cancel":
      case "task.destroy": {
        const id = f.params.task_id as string;
        if (!this.tasks.has(id)) {
          this.respond(f.id, { error: { code: "not_found", message: `no task ${id}` } });
          return;
        }
        const exited = f.method !== "task.status";
        const status = exited
          ? {
              task_id: id,
              state: "exited",
              reason: f.method === "task.cancel" ? "cancelled" : "completed",
              exit_code: 0,
              ended_at: new Date().toISOString(),
            }
          : { task_id: id, state: "running" };
        if (f.method === "task.destroy") this.tasks.delete(id);
        this.respond(f.id, { result: { status } });
        return;
      }
      case "task.list":
        this.respond(f.id, {
          result: {
            tasks: [...this.tasks.keys()].map((id) => ({ task_id: id, status: { task_id: id, state: "running" } })),
          },
        });
        return;
      case "drain":
        this.respond(f.id, { result: {} });
        return;
      default:
        this.respond(f.id, { error: { code: "unknown_method", message: `unknown method ${f.method}` } });
    }
  }

  /** Overrides the answer to requests; return undefined to keep the default. */
  onRequest(h: RequestHandler): void {
    this.handler = h;
  }

  respond(id: string, r: { result?: unknown; error?: { code: string; message: string } }): void {
    const frame: Frame = { type: "response", id };
    if (r.error) frame.error = r.error;
    else frame.result = r.result ?? {};
    this.send(frame);
  }

  send(frame: Frame): void {
    assertWorkerFrame(frame);
    this.ws.send(JSON.stringify(frame));
  }

  heartbeat(active = 0, capacity = 4): void {
    this.send({ type: "heartbeat", params: { active, capacity } });
  }

  /** Reports a task state change (event task.state). */
  taskState(status: Record<string, unknown>): void {
    this.send({ type: "event", method: "task.state", params: { status } });
  }

  waitFrame(pred: (f: Frame) => boolean, timeoutMs = 15_000): Promise<Frame> {
    const hit = this.received.find(pred);
    if (hit) return Promise.resolve(hit);
    return new Promise((resolve, reject) => {
      const t = setTimeout(
        () =>
          reject(
            new Error(
              `worker: no matching frame within ${timeoutMs} ms (received: ${this.received.map((f) => f.type + (f.error ? `:${f.error.code}` : "")).join(", ") || "none"}${this.closed ? `; closed ${this.closeCode}` : ""})`,
            ),
          ),
        timeoutMs,
      );
      this.waiters.push({
        pred,
        resolve: (f) => {
          clearTimeout(t);
          resolve(f);
        },
      });
    });
  }

  /** Waits for task.assign of a run session. */
  awaitTask(runId: string, session = 1, timeoutMs = 30_000): Promise<AssignedTask> {
    for (const t of this.tasks.values()) if (t.run_id === runId && t.session === session) return Promise.resolve(t);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(
        () => reject(new Error(`worker: no task.assign for ${runId}.${session} within ${timeoutMs} ms`)),
        timeoutMs,
      );
      this.taskWaiters.push({
        runId,
        session,
        resolve: (t) => {
          clearTimeout(timer);
          resolve(t);
        },
      });
    });
  }

  close(): void {
    try {
      this.ws.close(1000, "contract test done");
    } catch {
      // already closed
    }
  }

  /** Waits until the server closes the connection. Returns the close code. */
  waitClosed(timeoutMs = 15_000): Promise<number | undefined> {
    if (this.closed) return Promise.resolve(this.closeCode);
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`worker: connection still open after ${timeoutMs} ms`)), timeoutMs);
      this.ws.addEventListener("close", (ev) => {
        clearTimeout(t);
        resolve(ev.code);
      });
    });
  }
}
