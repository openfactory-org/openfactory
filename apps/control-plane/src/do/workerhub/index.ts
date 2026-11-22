// WorkerHubDO (key team:<id>|worker:<workerId>): worker protocol v2 over a
// hibernatable WebSocket. Port of internal/sandbox/direct/hubv2.go for one
// worker, with the extensions of schemas/worker/v2 (auth.challenge,
// facts/slots in hello, task.list, drain).
//
// - The Worker routes GET /api/v1/workers/connect here by the OF-Worker-ID
//   header (src/api/workers.ts connectWorker).
// - The handshake (challenge -> hello -> ready) and the Ed25519 check are in
//   protocol.ts; the SQLite lease cache and hub state are in store.ts.
// - Liveness: the worker sends the text {"type":"ping"}; the runtime answers
//   {"type":"pong"} (setWebSocketAutoResponse) without waking the DO. A timer
//   every 45 s reads the auto-response timestamp.
// - hello reconciles the task cache with the tasks the worker holds. Tasks
//   that were never leased to this worker are ignored.
// - capacity.report goes to DispatchDO on every change (lane per worker).
import { doName, newId, partitionOf } from "@openfactory/core";
import { ProviderError, type SandboxTask, type TaskStatus } from "@openfactory/sandbox";
import {
  WORKER_SUBPROTOCOL,
  fromWireStatus,
  mergeStatus,
  providerErrorOf,
  toWireTask,
  type WireError,
  type WireFrame,
  type WireHello,
} from "@openfactory/sandbox/microvm";
import type { Ack, OutboxEnvelope, TeamRpc, WorkerHubRpc } from "../../contracts/do-messages";
import { serverConfig } from "../../config";
import type { Env } from "../../contracts/env";
import { BaseDO, type Migration } from "../../lib/base";
import { dest } from "../../lib/outbox/dest";
import type { OutboxRow } from "../../lib/outbox/outbox";
import { isoOf } from "../../lib/page";
import { isOpError } from "../../lib/rpc";
import type { WorkerRecord } from "../team/workers";
import m0001 from "./migrations/0001_init";
import {
  HANDSHAKE_MS,
  LIVENESS_MS,
  PING_TEXT,
  PONG_TEXT,
  challengeFrame,
  closeQuietly,
  frameRefusal,
  handshakeTimeout,
  heldTasks,
  helloRefusal,
  helloSummary,
  openAttachment,
  verifyAuthResponse,
  welcomeResult,
  type Attachment,
  type Mode,
} from "./protocol";
import { HubStore, statusOf } from "./store";
import { mintWorkerToken } from "./token";

/** Timeout of a request to the worker without a caller deadline (hubv2.go). */
export const CALL_TIMEOUT_MS = 60_000;
/** Custom outbox destination: WorkDO.sandboxState of a run. */
const SANDBOX_STATE_DEST = "sandbox-state:";

/** Settings of the hub. They are constants: nothing sets them at run time. */
export const hubSettings: Readonly<{
  /** Lowest runtime version (hello.version) the hub accepts; "" = any. */
  minRuntimeVersion: string;
  recommendedRuntimeVersion: string;
  callTimeoutMs: number;
}> = Object.freeze({
  minRuntimeVersion: "",
  recommendedRuntimeVersion: "",
  callTimeoutMs: CALL_TIMEOUT_MS,
});

interface Pending {
  resolve: (v: unknown) => void;
  reject: (e: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
  connId: string;
  method: string;
}

export class WorkerHubDO extends BaseDO implements WorkerHubRpc {
  private readonly store = new HubStore(this.sql);
  private readonly pending = new Map<string, Pending>();
  /** Per-connection message chains: frames of one socket are handled in order. */
  private readonly chains = new Map<string, Promise<void>>();
  /** Requests of reconcile() that wait for an answer ("drain", "destroy:<task>"). */
  private readonly inflight = new Set<string>();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair(PING_TEXT, PONG_TEXT));
    this.onTimer<{ connId: string }>("handshake", (t) => {
      const ws = this.socket(t.payload.connId);
      if (!ws) return;
      const att = this.att(ws);
      if (att.phase === "ready") return;
      const r = handshakeTimeout(att.phase);
      closeQuietly(ws, r.code, r.reason);
    });
    this.onTimerAsync("liveness", () => this.checkLiveness());
  }

  protected migrations(): readonly Migration[] {
    return [{ version: 1, name: "init", sql: m0001 }];
  }

  /** Liveness probe for tests and the readiness check. */
  async ping(): Promise<string> {
    return "WorkerHubDO";
  }

  workerId(): string {
    const n = this.doName();
    const i = n.indexOf("|worker:");
    return i < 0 ? n : n.slice(i + "|worker:".length);
  }

  private att(ws: WebSocket): Attachment {
    return ws.deserializeAttachment() as Attachment;
  }

  private socket(connId: string): WebSocket | undefined {
    return this.ctx.getWebSockets(connId)[0];
  }

  /** The live connection: the socket that finished its hello last. */
  private current(): WebSocket | undefined {
    const id = this.store.get("current_conn");
    if (!id) return undefined;
    const ws = this.socket(id);
    if (!ws || this.att(ws).phase !== "ready") return undefined;
    return ws;
  }

  /** capacity.report to DispatchDO. Call inside tx. */
  private reportCapacity(): void {
    const st = this.store;
    const rec = st.record();
    if (!rec) return;
    const gen = st.nextGeneration();
    const online = !st.revoked() && st.get("current_conn") !== undefined;
    this.enqueue({
      dest: dest.dispatch(this.teamId()),
      kind: "capacity.report",
      lane: `capacity:${rec.id}`,
      msgId: `${rec.id}:${gen}`,
      payload: {
        workerId: rec.id,
        kind: rec.kind,
        online,
        draining: rec.draining,
        slots: st.slotLimit(),
        used: st.slotsUsed(),
        labels: rec.kind === "process" ? { ...rec.labels, ...(st.hello()?.labels ?? {}) } : rec.labels,
        factories: rec.factories,
        generation: gen,
      },
    });
  }

  /** Outbox row that tells the run's WorkDO that a task exited (WorkDO.sandboxState). Call inside tx. */
  private notifyExit(s: TaskStatus): void {
    if (s.state !== "exited") return;
    let runId: string;
    let session: number;
    try {
      const i = s.taskId.lastIndexOf(".");
      runId = s.taskId.slice(0, i);
      session = Number(s.taskId.slice(i + 1));
      partitionOf(runId);
    } catch {
      return; // not a routed run ID: nothing to tell
    }
    this.enqueue({
      dest: `${SANDBOX_STATE_DEST}${runId}`,
      kind: "sandbox.state",
      lane: `task:${s.taskId}`,
      msgId: `${s.taskId}:exited`,
      payload: { runId, session, status: s },
    });
  }

  protected override async deliverCustom(envelope: OutboxEnvelope, row: OutboxRow): Promise<Ack | void> {
    if (!row.dest.startsWith(SANDBOX_STATE_DEST)) return super.deliverCustom(envelope, row);
    const p = envelope.payload as unknown as { runId: string; session: number; status: TaskStatus };
    const work = this.env.WORK.getByName(doName.work(this.teamId(), partitionOf(p.runId))) as unknown as {
      sandboxState(runId: string, session: number, status: TaskStatus): Promise<{ ok: true }>;
    };
    await work.sandboxState(p.runId, p.session, p.status);
    return { ok: true };
  }

  private team(): TeamRpc {
    return this.env.TEAM.getByName(doName.team(this.teamId())) as unknown as TeamRpc;
  }

  /** Pushes live state to the TeamDO record. Best effort; the next liveness tick repeats it. */
  private async pushTeam(body: Record<string, unknown>): Promise<void> {
    try {
      const r = await this.team().api({ requestId: newId("req") }, "workers.hubState", { id: this.workerId() }, body);
      if (isOpError(r)) console.warn("WorkerHubDO: hubState refused", r.$error.code, r.$error.detail);
    } catch (e) {
      console.warn("WorkerHubDO: hubState failed", e);
    }
  }

  private liveBody(online: boolean): Record<string, unknown> {
    const h = this.store.hello();
    const body: Record<string, unknown> = { online, slotsUsed: this.store.slotsUsed() };
    const ws = this.current();
    if (ws) body.lastSeenMs = this.lastSeen(ws);
    if (h) {
      body.version = h.version;
      if (h.facts) body.facts = h.facts;
      body.labels = h.labels;
      body.capacity = h.capacity;
    }
    return body;
  }

  private lastSeen(ws: WebSocket): number {
    const att = this.att(ws);
    const auto = this.ctx.getWebSocketAutoResponseTimestamp(ws)?.getTime() ?? 0;
    return Math.max(auto, att.lastSeenMs, att.openedMs);
  }

  // ---------- connect ----------

  override async fetch(request: Request): Promise<Response> {
    await this.ready;
    if ((request.headers.get("upgrade") ?? "").toLowerCase() !== "websocket") {
      return new Response("expected a WebSocket upgrade\n", { status: 426 });
    }
    const workerId = this.workerId();
    if (request.headers.get("x-of-worker-id") !== workerId)
      return new Response("worker ID mismatch\n", { status: 400 });
    const mode: Mode = request.headers.get("x-of-auth-mode") === "apikey" ? "apikey" : "key";
    if (this.store.get("revoked") === "1") return new Response("revoked\n", { status: 403 });

    let rec: WorkerRecord;
    try {
      const r = await this.team().api({ requestId: newId("req") }, "workers.hubConnect", { id: workerId }, null);
      if (isOpError(r)) {
        if (r.$error.code === "resource_not_found") return new Response("unknown worker\n", { status: 401 });
        return new Response(`${r.$error.detail}\n`, { status: 503 });
      }
      rec = r as WorkerRecord;
    } catch (e) {
      console.error("WorkerHubDO: hubConnect failed", e);
      return new Response("worker registry unavailable\n", { status: 503 });
    }
    if (rec.revoked) {
      this.tx(() => this.store.set("revoked", "1"));
      return new Response("revoked\n", { status: 403 });
    }
    if (mode === "key" && rec.publicKey === "")
      return new Response("the worker has no key; enroll it\n", { status: 401 });
    if (mode === "apikey" && rec.publicKey !== "") {
      return new Response("this worker is enrolled: connect with its key\n", { status: 401 });
    }
    if (rec.kind === "process" && !serverConfig(this.env).devProcessWorkers) {
      return new Response("development process workers are not allowed\n", { status: 403 });
    }

    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    const connId = newId("wconn");
    this.ctx.acceptWebSocket(server, [connId]);
    const now = Date.now();
    const att = openAttachment(connId, mode, now);
    if (att.phase === "challenge") server.send(challengeFrame(att));
    server.serializeAttachment(att);
    this.tx(() => {
      this.store.setRecord(rec);
      this.setTimer(`handshake:${connId}`, "handshake", now + HANDSHAKE_MS, { connId });
    });
    return new Response(null, {
      status: 101,
      webSocket: client,
      headers: { "Sec-WebSocket-Protocol": WORKER_SUBPROTOCOL },
    });
  }

  // ---------- frames ----------

  override async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const att = this.att(ws);
    const prev = this.chains.get(att.connId) ?? Promise.resolve();
    const next = prev
      .then(() => this.handleFrame(ws, message))
      .catch((e) => console.error("WorkerHubDO: frame failed", e));
    this.chains.set(att.connId, next);
    await next;
  }

  private async handleFrame(ws: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const att = this.att(ws);
    if (typeof message !== "string") return;
    if (message === PING_TEXT) {
      ws.send(PONG_TEXT);
      return;
    }
    let f: WireFrame;
    try {
      f = JSON.parse(message) as WireFrame;
    } catch {
      return;
    }
    if (typeof f !== "object" || f === null) return;
    att.lastSeenMs = Date.now();
    ws.serializeAttachment(att);
    const refused = frameRefusal(att.phase, f);
    if (refused) {
      closeQuietly(ws, refused.code, refused.reason);
      return;
    }
    switch (att.phase) {
      case "challenge":
        await this.onAuthResponse(ws, att, f.params);
        return;
      case "hello":
        await this.onHello(ws, att, f.params as WireHello);
        return;
      case "ready":
        await this.onReadyFrame(ws, att, f);
    }
  }

  private async onAuthResponse(ws: WebSocket, att: Attachment, params: unknown): Promise<void> {
    if (!(await verifyAuthResponse(this.store.record(), att, params))) {
      closeQuietly(ws, 1008, "auth_failed");
      return;
    }
    att.phase = "hello";
    delete att.nonce;
    ws.serializeAttachment(att);
  }

  private async onHello(ws: WebSocket, att: Attachment, hello: WireHello): Promise<void> {
    const st = this.store;
    const rec = st.record();
    const refused = helloRefusal(rec, att, hello, hubSettings.minRuntimeVersion);
    if (refused || !rec) {
      closeQuietly(ws, refused?.code ?? 1008, refused?.reason ?? "worker_id does not match");
      return;
    }
    const held = heldTasks(hello);
    const replaced: WebSocket[] = [];
    this.tx(() => {
      for (const other of this.ctx.getWebSockets()) {
        if (other !== ws && this.att(other).phase === "ready") replaced.push(other);
      }
      att.phase = "ready";
      ws.serializeAttachment(att);
      st.set("current_conn", att.connId);
      this.clearTimer(`handshake:${att.connId}`);
      const now = Date.now();
      // A row with a destroy still owed keeps it: reconcile() sends it below.
      const { exited, ignored } = st.attach(held, now);
      for (const s of exited) this.notifyExit(s);
      for (const id of ignored) console.warn(`WorkerHubDO ${rec.id}: ignoring task ${id}, not leased to this worker`);
      st.setHello(helloSummary(hello));
      // The drain flag the worker holds now; reconcile() sends the record's flag when it differs.
      st.set("worker_draining", String(!!hello.draining));
      this.reportCapacity();
      this.setTimer("liveness", "liveness", now + LIVENESS_MS);
    });
    for (const old of replaced) {
      this.rejectPending(this.att(old).connId, "the connection was replaced");
      closeQuietly(old, 1001, "replaced by a new connection");
    }
    const welcome = welcomeResult(att.connId, rec.draining, hubSettings);
    if ((hello.features ?? []).includes("access_token")) {
      // Additive: only for workers that ask for it.
      const t = await mintWorkerToken(this.env, this.teamId(), rec.id);
      welcome.access_token = t.token;
      welcome.access_token_expires_at = isoOf(t.expiresAt);
    }
    ws.send(JSON.stringify({ type: "welcome", result: welcome }));
    this.reconcile();
    await this.pushTeam(this.liveBody(true));
  }

  private async onReadyFrame(ws: WebSocket, att: Attachment, f: WireFrame): Promise<void> {
    if (f.type === "response" && typeof f.id === "string") {
      const p = this.pending.get(f.id);
      if (!p || p.connId !== att.connId) return;
      this.pending.delete(f.id);
      clearTimeout(p.timer);
      if (f.error) p.reject(providerErrorOf(f.error as WireError));
      else p.resolve(f.result ?? {});
      return;
    }
    if (f.type === "event" && f.method === "task.state") {
      if (this.store.get("current_conn") !== att.connId) return;
      const s = fromWireStatus((f.params as { status?: unknown } | undefined)?.status);
      if (!s) return;
      const exited = this.tx(() => {
        const r = this.store.task(s.taskId);
        if (!r || r.destroyed === 1) return false;
        const old = statusOf(r);
        const merged = mergeStatus(old, s);
        if (merged === old) return false;
        this.store.setStatus(s.taskId, merged, Date.now());
        if (merged.state === "exited" && old.state !== "exited") {
          this.notifyExit(merged);
          this.reportCapacity();
          return true;
        }
        return false;
      });
      if (exited) await this.pushTeam({ slotsUsed: this.store.slotsUsed() });
      return;
    }
    if (f.type === "request" && typeof f.id === "string") {
      ws.send(
        JSON.stringify({
          type: "response",
          id: f.id,
          error: { code: "unknown_method", message: `unknown method ${String(f.method)}` },
        }),
      );
    }
    // heartbeat and unknown events: lastSeenMs is already updated.
  }

  override async webSocketClose(ws: WebSocket, code: number, reason: string): Promise<void> {
    closeQuietly(ws, code === 1005 || code === 1006 ? 1000 : code, reason || "bye");
    await this.onGone(ws);
  }

  override async webSocketError(ws: WebSocket): Promise<void> {
    await this.onGone(ws);
  }

  private async onGone(ws: WebSocket): Promise<void> {
    const att = this.att(ws);
    this.chains.delete(att.connId);
    this.rejectPending(att.connId, "the worker disconnected");
    const wasCurrent = this.tx(() => {
      this.clearTimer(`handshake:${att.connId}`);
      if (this.store.get("current_conn") !== att.connId) return false;
      this.store.del("current_conn");
      this.clearTimer("liveness");
      this.reportCapacity();
      return true;
    });
    if (wasCurrent) await this.pushTeam({ online: false, slotsUsed: this.store.slotsUsed() });
  }

  private rejectPending(connId: string, why: string): void {
    for (const [id, p] of this.pending) {
      if (p.connId !== connId) continue;
      this.pending.delete(id);
      clearTimeout(p.timer);
      p.reject(new ProviderError("unavailable", `${why} during ${p.method}`));
    }
  }

  private async checkLiveness(): Promise<void> {
    const ws = this.current();
    if (!ws) return;
    const last = this.lastSeen(ws);
    if (Date.now() - last > LIVENESS_MS) {
      closeQuietly(ws, 1001, "heartbeat timeout");
      await this.onGone(ws);
      return;
    }
    this.tx(() => this.setTimer("liveness", "liveness", Date.now() + LIVENESS_MS));
    this.reconcile();
    await this.pushTeam(this.liveBody(true));
  }

  // ---------- requests to the worker ----------

  private request(method: string, params: unknown): Promise<unknown> {
    const ws = this.current();
    if (!ws) return Promise.reject(new ProviderError("unavailable", "the worker is not connected"));
    const connId = this.att(ws).connId;
    const id = newId("wreq");
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new ProviderError("unavailable", `the worker did not answer ${method} in time`));
      }, hubSettings.callTimeoutMs);
      this.pending.set(id, { resolve, reject, timer, connId, method });
      try {
        ws.send(JSON.stringify({ type: "request", id, method, params }));
      } catch (e) {
        this.pending.delete(id);
        clearTimeout(timer);
        reject(new ProviderError("unavailable", `cannot send ${method}: ${String(e)}`));
      }
    });
  }

  /**
   * Sends what the worker still owes the hub, from SQLite state: the drain flag
   * of the record when the worker holds another one, and the destroys kept
   * while it was away (rows with destroyed = 1). Runs at hello, on every
   * liveness tick and after a drain change, so a request that failed, timed
   * out or was lost with the in-memory state (eviction) is sent again. Each
   * request is idempotent on the worker.
   */
  private reconcile(): void {
    if (!this.current()) return;
    const st = this.store;
    const rec = st.record();
    if (rec && st.get("worker_draining") !== String(rec.draining)) {
      const draining = rec.draining;
      this.background("drain", "drain", { draining }, () => {
        this.tx(() => st.set("worker_draining", String(draining)));
        this.reconcile(); // the record may have changed while the request was out
      });
    }
    for (const taskId of st.destroyedIds()) {
      this.background(`destroy:${taskId}`, "task.destroy", { task_id: taskId }, () =>
        this.tx(() => st.removeDestroyed(taskId)),
      );
    }
  }

  /** One request of reconcile(); at most one per key is out. not_found counts as done. */
  private background(key: string, method: string, params: unknown, done: () => void): void {
    if (this.inflight.has(key)) return;
    this.inflight.add(key);
    const p = this.request(method, params)
      .catch((e: unknown) => {
        if ((e as ProviderError).kind !== "not_found") throw e;
      })
      .then(
        () => {
          this.inflight.delete(key);
          done();
        },
        (e: unknown) => {
          this.inflight.delete(key);
          console.warn(`WorkerHubDO: ${method} failed; the next liveness check sends it again`, e);
        },
      )
      .catch((e: unknown) => console.warn(`WorkerHubDO: ${method} answered, but its state update failed`, e));
    this.ctx.waitUntil(p);
  }

  // ---------- WorkerHubRpc ----------
  // Errors are ProviderError: the message survives RPC and parseProviderError rebuilds it.

  async taskAssign(task: SandboxTask, extras?: Record<string, unknown>): Promise<TaskStatus> {
    await this.ready;
    const st = this.store;
    const reserved = this.tx(() => {
      const r = st.task(task.taskId);
      if (r && r.destroyed === 0) return { existing: statusOf(r) };
      if (st.revoked()) throw new ProviderError("unavailable", "the worker is revoked");
      const rec = st.record();
      if (!rec) throw new ProviderError("unavailable", "the worker has never connected");
      if (rec.draining) throw new ProviderError("at_capacity", "the worker is draining");
      if (!this.current()) throw new ProviderError("unavailable", "the worker is not connected");
      const limit = st.slotLimit();
      if (limit > 0 && st.slotsUsed() >= limit)
        throw new ProviderError("at_capacity", `all ${limit} slots of the worker are in use`);
      st.lease(task, { taskId: task.taskId, state: "accepted" }, Date.now());
      this.reportCapacity();
      return { existing: undefined };
    });
    if (reserved.existing) return reserved.existing;
    let result: unknown;
    try {
      result = await this.request("task.assign", { task: toWireTask(task) });
    } catch (e) {
      // Free the slot (hubv2.go frees the reservation on error).
      this.tx(() => {
        const r = st.task(task.taskId);
        if (r && statusOf(r).state === "accepted") {
          st.remove(task.taskId);
          this.reportCapacity();
        }
      });
      throw e;
    }
    const reported = fromWireStatus((result as { status?: unknown }).status) ?? {
      taskId: task.taskId,
      state: "accepted" as const,
    };
    const merged = this.tx(() => {
      const r = st.task(task.taskId);
      const m = mergeStatus(r ? statusOf(r) : undefined, reported);
      st.setStatus(task.taskId, m, Date.now());
      return m;
    });
    const cache = (extras?.git_cache ?? []) as { clone_url?: string }[];
    const repos = Array.isArray(cache)
      ? cache.map((c) => c?.clone_url).filter((u): u is string => typeof u === "string")
      : [];
    if (repos.length > 0) {
      await this.team()
        .api({ requestId: newId("req") }, "workers.noteRepos", { id: this.workerId() }, { repos })
        .catch((e: unknown) => console.warn("WorkerHubDO: noteRepos failed", e));
    }
    await this.pushTeam({ slotsUsed: st.slotsUsed() });
    return merged;
  }

  async taskStatus(taskId: string): Promise<TaskStatus> {
    await this.ready;
    const r = this.store.task(taskId);
    if (!r || r.destroyed === 1)
      throw new ProviderError("not_found", `task ${taskId} is not known to worker ${this.workerId()}`);
    return statusOf(r);
  }

  async taskCancel(taskId: string, graceSeconds: number): Promise<void> {
    await this.ready;
    const r = this.store.task(taskId);
    if (!r || r.destroyed === 1)
      throw new ProviderError("not_found", `task ${taskId} is not known to worker ${this.workerId()}`);
    await this.request("task.cancel", { task_id: taskId, grace_seconds: Math.max(0, Math.round(graceSeconds)) });
  }

  /** Safe to call twice. While the worker is away the destroy is kept and sent at its next hello. */
  async taskDestroy(taskId: string): Promise<void> {
    await this.ready;
    if (this.current()) {
      try {
        await this.request("task.destroy", { task_id: taskId });
      } catch (e) {
        if ((e as ProviderError).kind !== "not_found") throw e;
      }
      this.tx(() => {
        if (this.store.remove(taskId)) this.reportCapacity();
      });
    } else {
      this.tx(() => {
        if (this.store.markDestroyed(taskId, Date.now())) this.reportCapacity();
      });
    }
    await this.pushTeam({ slotsUsed: this.store.slotsUsed() });
  }

  /** Orphan sweep: every task the worker holds when connected, else the leased cache. */
  async taskList(): Promise<TaskStatus[]> {
    await this.ready;
    if (this.current()) {
      const res = (await this.request("task.list", {})) as { tasks?: { task_id: string; status: unknown }[] | null };
      return (res.tasks ?? []).map((t) => fromWireStatus(t.status)).filter((s): s is TaskStatus => s !== null);
    }
    return this.store.liveStatuses();
  }

  async live(): Promise<{ online: boolean; slotsUsed: number; lastSeen: string | null; version: string | null }> {
    await this.ready;
    const ws = this.current();
    return {
      online: !!ws,
      slotsUsed: this.store.slotsUsed(),
      lastSeen: ws ? isoOf(this.lastSeen(ws)) : null,
      version: this.store.hello()?.version || null,
    };
  }

  /** Closes the socket and refuses new connections (the TeamDO record says revoked too). */
  async revoke(): Promise<void> {
    await this.ready;
    this.tx(() => {
      this.store.set("revoked", "1");
      const rec = this.store.record();
      if (rec) this.store.setRecord({ ...rec, revoked: true });
      this.store.del("current_conn");
      this.clearTimer("liveness");
      this.reportCapacity();
    });
    for (const ws of this.ctx.getWebSockets()) {
      this.rejectPending(this.att(ws).connId, "the worker was revoked");
      closeQuietly(ws, 1008, "revoked");
    }
  }

  async drain(draining: boolean): Promise<void> {
    await this.ready;
    this.tx(() => {
      const rec = this.store.record();
      if (!rec) return;
      this.store.setRecord({ ...rec, draining });
      this.reportCapacity();
    });
    this.reconcile();
  }

  /** A changed record (PATCH, drain, resume) from the REST handler. */
  async configure(rec: WorkerRecord): Promise<void> {
    await this.ready;
    this.tx(() => {
      this.store.setRecord(rec);
      if (rec.revoked) this.store.set("revoked", "1");
      this.reportCapacity();
    });
    if (rec.revoked) {
      await this.revoke();
      return;
    }
    this.reconcile();
  }
}
