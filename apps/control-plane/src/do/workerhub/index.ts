// WorkerHubDO (key team:<id>|worker:<workerId>): worker protocol v2 over a
// hibernatable WebSocket. Port of internal/sandbox/direct/hubv2.go for one
// worker, with the extensions of schemas/worker/v2 (auth.challenge,
// facts/slots in hello, task.list, drain).
//
// - The Worker routes GET /api/v1/workers/connect here by the OF-Worker-ID
//   header (src/api/workers.ts connectWorker).
// - An enrolled worker (mode "key") gets auth.challenge first and proves its
//   Ed25519 key; a development process worker (mode "apikey") was checked by
//   the Worker with a team API key and speaks first.
// - Liveness: the worker sends the text {"type":"ping"}; the runtime answers
//   {"type":"pong"} (setWebSocketAutoResponse) without waking the DO. A timer
//   every 45 s reads the auto-response timestamp.
// - hello reconciles the task cache with the tasks the worker holds. Tasks
//   that were never leased to this worker are ignored (06 correction 20).
// - capacity.report goes to DispatchDO on every change (lane per worker).
import { doName, newId, partitionOf } from "@openfactory/core";
import { ProviderError, type SandboxTask, type TaskStatus } from "@openfactory/sandbox";
import {
  WORKER_SUBPROTOCOL,
  fromWireStatus,
  mergeStatus,
  providerErrorOf,
  toWireTask,
  workerAuthMessage,
  type WireError,
  type WireFrame,
  type WireHello,
} from "@openfactory/sandbox/microvm";
import type { Ack, OutboxEnvelope, TeamRpc, WorkerHubRpc } from "../../contracts/do-messages";
import { serverConfig } from "../../config";
import type { Env } from "../../contracts/env";
import { BaseDO, type Migration } from "../../lib/base";
import { b64url, fromAnyB64, fromB64url, randomBytes } from "../../lib/bytes";
import { dest } from "../../lib/outbox/dest";
import type { OutboxRow } from "../../lib/outbox/outbox";
import { isoOf } from "../../lib/page";
import { isOpError } from "../../lib/rpc";
import { all, first } from "../../lib/sql/query";
import type { WorkerRecord } from "../team/workers";
import m0001 from "./migrations/0001_init";
import { mintWorkerToken } from "./token";

/** Heartbeat interval announced in welcome. */
export const HEARTBEAT_MS = 15_000;
/** No ping or frame for this long closes the connection (3 intervals). */
export const LIVENESS_MS = 45_000;
/** The handshake (challenge answer and hello) must finish in this time. */
export const HANDSHAKE_MS = 10_000;
/** Timeout of a request to the worker without a caller deadline (hubv2.go). */
export const CALL_TIMEOUT_MS = 60_000;
export const PING_TEXT = '{"type":"ping"}';
export const PONG_TEXT = '{"type":"pong"}';
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

type Mode = "key" | "apikey";
type Phase = "challenge" | "hello" | "ready";

interface Attachment {
  connId: string;
  mode: Mode;
  phase: Phase;
  nonce?: string;
  openedMs: number;
  lastSeenMs: number;
}

interface Pending {
  resolve: (v: unknown) => void;
  reject: (e: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
  connId: string;
  method: string;
}

interface TaskRow extends Record<string, SqlStorageValue> {
  task_id: string;
  run_id: string;
  session: number;
  status: string;
  destroyed: number;
  leased_ms: number;
  updated_ms: number;
}

/** Hello facts kept by the hub for live() and the TeamDO push. */
interface HelloSummary {
  version: string;
  capacity: number;
  labels: Record<string, string>;
  facts: Record<string, unknown> | null;
  features: string[];
  slots: number;
}

function closeQuietly(ws: WebSocket, code: number, reason: string): void {
  try {
    ws.close(code, reason);
  } catch {
    /* already closed */
  }
}

/** a < b for dotted numeric versions ("v1.4.0"); non-numeric versions never compare lower. */
export function versionLess(a: string, b: string): boolean {
  const parse = (v: string): number[] | null => {
    const m = /^v?(\d+(?:\.\d+)*)/.exec(v.trim());
    return m ? m[1]!.split(".").map(Number) : null;
  };
  const x = parse(a);
  const y = parse(b);
  if (!x || !y) return false;
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] ?? 0) - (y[i] ?? 0);
    if (d !== 0) return d < 0;
  }
  return false;
}

export class WorkerHubDO extends BaseDO implements WorkerHubRpc {
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
      closeQuietly(
        ws,
        att.phase === "challenge" ? 1008 : 1002,
        att.phase === "challenge" ? "auth_failed" : "expected hello",
      );
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

  // ---------- state helpers ----------

  private state(k: string): string | undefined {
    return first<{ v: string }>(this.sql, "SELECT v FROM hub_state WHERE k = ?", k)?.v;
  }

  private setState(k: string, v: string): void {
    this.sql.exec("INSERT INTO hub_state (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v", k, v);
  }

  private delState(k: string): void {
    this.sql.exec("DELETE FROM hub_state WHERE k = ?", k);
  }

  private record(): WorkerRecord | undefined {
    const v = this.state("record");
    return v ? (JSON.parse(v) as WorkerRecord) : undefined;
  }

  private hello(): HelloSummary | undefined {
    const v = this.state("hello");
    return v ? (JSON.parse(v) as HelloSummary) : undefined;
  }

  private revoked(): boolean {
    return this.state("revoked") === "1" || this.record()?.revoked === true;
  }

  private rows(): TaskRow[] {
    return all<TaskRow>(this.sql, "SELECT * FROM hub_tasks ORDER BY leased_ms, task_id");
  }

  private row(taskId: string): TaskRow | undefined {
    return first<TaskRow>(this.sql, "SELECT * FROM hub_tasks WHERE task_id = ?", taskId);
  }

  private statusOf(r: TaskRow): TaskStatus {
    return JSON.parse(r.status) as TaskStatus;
  }

  /** Leased tasks that hold a slot: not exited and not destroyed. */
  private slotsUsed(): number {
    return this.rows().filter((r) => r.destroyed === 0 && this.statusOf(r).state !== "exited").length;
  }

  private slotLimit(): number {
    const rec = this.record();
    if (rec && rec.slots > 0) return rec.slots;
    return this.hello()?.capacity ?? 0;
  }

  private att(ws: WebSocket): Attachment {
    return ws.deserializeAttachment() as Attachment;
  }

  private socket(connId: string): WebSocket | undefined {
    return this.ctx.getWebSockets(connId)[0];
  }

  /** The live connection: the socket that finished its hello last. */
  private current(): WebSocket | undefined {
    const id = this.state("current_conn");
    if (!id) return undefined;
    const ws = this.socket(id);
    if (!ws || this.att(ws).phase !== "ready") return undefined;
    return ws;
  }

  /** capacity.report to DispatchDO. Call inside tx. */
  private reportCapacity(): void {
    const rec = this.record();
    if (!rec) return;
    const gen = Number(this.state("generation") ?? "0") + 1;
    this.setState("generation", String(gen));
    const online = !this.revoked() && this.state("current_conn") !== undefined;
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
        slots: this.slotLimit(),
        used: this.slotsUsed(),
        labels: rec.kind === "process" ? { ...rec.labels, ...(this.hello()?.labels ?? {}) } : rec.labels,
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
    const h = this.hello();
    const body: Record<string, unknown> = { online, slotsUsed: this.slotsUsed() };
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
    if (this.state("revoked") === "1") return new Response("revoked\n", { status: 403 });

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
      this.tx(() => this.setState("revoked", "1"));
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
    const att: Attachment = {
      connId,
      mode,
      phase: mode === "key" ? "challenge" : "hello",
      openedMs: now,
      lastSeenMs: now,
    };
    if (mode === "key") {
      att.nonce = b64url(randomBytes(32));
      server.send(
        JSON.stringify({ type: "auth.challenge", params: { nonce: att.nonce, connection_id: connId, protocols: [1] } }),
      );
    }
    server.serializeAttachment(att);
    this.tx(() => {
      this.setState("record", JSON.stringify(rec));
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
    switch (att.phase) {
      case "challenge":
        if (f.type !== "auth.response") {
          closeQuietly(ws, 1008, "auth_failed");
          return;
        }
        await this.onAuthResponse(ws, att, f.params);
        return;
      case "hello":
        if (f.type !== "hello") {
          closeQuietly(ws, 1002, "expected hello");
          return;
        }
        await this.onHello(ws, att, f.params as WireHello);
        return;
      case "ready":
        await this.onReadyFrame(ws, att, f);
    }
  }

  private async onAuthResponse(ws: WebSocket, att: Attachment, params: unknown): Promise<void> {
    const p = (params ?? {}) as { worker_id?: unknown; alg?: unknown; sig?: unknown };
    const rec = this.record();
    const ok =
      !!rec &&
      p.worker_id === rec.id &&
      p.alg === "ed25519" &&
      typeof p.sig === "string" &&
      (await this.verify(rec.publicKey, p.worker_id, att.connId, att.nonce ?? "", p.sig));
    if (!ok) {
      closeQuietly(ws, 1008, "auth_failed");
      return;
    }
    att.phase = "hello";
    delete att.nonce;
    ws.serializeAttachment(att);
  }

  private async verify(
    publicKey: string,
    workerId: string,
    connId: string,
    nonce: string,
    sig: string,
  ): Promise<boolean> {
    try {
      const raw = fromAnyB64(publicKey);
      if (!raw || raw.length !== 32) return false;
      const key = await crypto.subtle.importKey("raw", raw, { name: "Ed25519" }, false, ["verify"]);
      return await crypto.subtle.verify(
        { name: "Ed25519" },
        key,
        fromB64url(sig),
        workerAuthMessage(workerId, connId, nonce) as Uint8Array<ArrayBuffer>,
      );
    } catch {
      return false;
    }
  }

  private async onHello(ws: WebSocket, att: Attachment, hello: WireHello): Promise<void> {
    const rec = this.record();
    if (!rec || !hello || hello.worker_id !== rec.id) {
      closeQuietly(ws, 1008, "worker_id does not match");
      return;
    }
    if (att.mode === "key" && (hello.protocol_minor ?? 0) < 1) {
      closeQuietly(ws, 1008, "runtime_too_old");
      return;
    }
    if (hubSettings.minRuntimeVersion && versionLess(hello.version ?? "", hubSettings.minRuntimeVersion)) {
      closeQuietly(ws, 1008, "runtime_too_old");
      return;
    }
    const held = new Map<string, TaskStatus>();
    for (const t of hello.tasks ?? []) {
      const s = fromWireStatus(t?.status);
      if (s && s.taskId === t.task_id) held.set(s.taskId, s);
    }
    const replaced: WebSocket[] = [];
    this.tx(() => {
      for (const other of this.ctx.getWebSockets()) {
        if (other !== ws && this.att(other).phase === "ready") replaced.push(other);
      }
      att.phase = "ready";
      ws.serializeAttachment(att);
      this.setState("current_conn", att.connId);
      this.clearTimer(`handshake:${att.connId}`);
      const now = Date.now();
      // attach: the hello's list replaces the cache. A leased task the worker
      // does not hold is gone; a held task that was never leased is ignored.
      for (const r of this.rows()) {
        const s = held.get(r.task_id);
        if (!s) {
          this.sql.exec("DELETE FROM hub_tasks WHERE task_id = ?", r.task_id);
          continue;
        }
        if (r.destroyed === 1) continue; // reconcile() sends the destroy kept while the worker was away
        const old = this.statusOf(r);
        this.sql.exec(
          "UPDATE hub_tasks SET status = ?, updated_ms = ? WHERE task_id = ?",
          JSON.stringify(s),
          now,
          r.task_id,
        );
        if (s.state === "exited" && old.state !== "exited") this.notifyExit(s);
      }
      for (const id of held.keys()) {
        if (!this.row(id)) console.warn(`WorkerHubDO ${rec.id}: ignoring task ${id}, not leased to this worker`);
      }
      const summary: HelloSummary = {
        version: hello.version ?? "",
        capacity: Math.max(0, hello.capacity ?? 0),
        labels: hello.labels ?? {},
        facts: (hello.facts as Record<string, unknown> | undefined) ?? null,
        features: hello.features ?? [],
        slots: hello.slots ?? 0,
      };
      this.setState("hello", JSON.stringify(summary));
      // The drain flag the worker holds now; reconcile() sends the record's flag when it differs.
      this.setState("worker_draining", String(!!hello.draining));
      this.reportCapacity();
      this.setTimer("liveness", "liveness", now + LIVENESS_MS);
    });
    for (const old of replaced) {
      this.rejectPending(this.att(old).connId, "the connection was replaced");
      closeQuietly(old, 1001, "replaced by a new connection");
    }
    const welcome: Record<string, unknown> = {
      connection_id: att.connId,
      heartbeat_ms: HEARTBEAT_MS,
      draining: rec.draining,
    };
    if (hubSettings.minRuntimeVersion) welcome.min_runtime_version = hubSettings.minRuntimeVersion;
    if (hubSettings.recommendedRuntimeVersion)
      welcome.recommended_runtime_version = hubSettings.recommendedRuntimeVersion;
    if ((hello.features ?? []).includes("access_token")) {
      // Additive (needs the schema change requested by WS6): only for workers that ask.
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
      if (this.state("current_conn") !== att.connId) return;
      const s = fromWireStatus((f.params as { status?: unknown } | undefined)?.status);
      if (!s) return;
      const exited = this.tx(() => {
        const r = this.row(s.taskId);
        if (!r || r.destroyed === 1) return false;
        const old = this.statusOf(r);
        const merged = mergeStatus(old, s);
        if (merged === old) return false;
        this.sql.exec(
          "UPDATE hub_tasks SET status = ?, updated_ms = ? WHERE task_id = ?",
          JSON.stringify(merged),
          Date.now(),
          s.taskId,
        );
        if (merged.state === "exited" && old.state !== "exited") {
          this.notifyExit(merged);
          this.reportCapacity();
          return true;
        }
        return false;
      });
      if (exited) await this.pushTeam({ slotsUsed: this.slotsUsed() });
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
      if (this.state("current_conn") !== att.connId) return false;
      this.delState("current_conn");
      this.clearTimer("liveness");
      this.reportCapacity();
      return true;
    });
    if (wasCurrent) await this.pushTeam({ online: false, slotsUsed: this.slotsUsed() });
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
    const rec = this.record();
    if (rec && this.state("worker_draining") !== String(rec.draining)) {
      const draining = rec.draining;
      this.background("drain", "drain", { draining }, () => {
        this.tx(() => this.setState("worker_draining", String(draining)));
        this.reconcile(); // the record may have changed while the request was out
      });
    }
    for (const r of this.rows()) {
      if (r.destroyed !== 1) continue;
      const taskId = r.task_id;
      this.background(`destroy:${taskId}`, "task.destroy", { task_id: taskId }, () =>
        this.tx(() => this.sql.exec("DELETE FROM hub_tasks WHERE task_id = ? AND destroyed = 1", taskId)),
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
    const reserved = this.tx(() => {
      const r = this.row(task.taskId);
      if (r && r.destroyed === 0) return { existing: this.statusOf(r) };
      if (this.revoked()) throw new ProviderError("unavailable", "the worker is revoked");
      const rec = this.record();
      if (!rec) throw new ProviderError("unavailable", "the worker has never connected");
      if (rec.draining) throw new ProviderError("at_capacity", "the worker is draining");
      if (!this.current()) throw new ProviderError("unavailable", "the worker is not connected");
      const limit = this.slotLimit();
      if (limit > 0 && this.slotsUsed() >= limit)
        throw new ProviderError("at_capacity", `all ${limit} slots of the worker are in use`);
      const now = Date.now();
      const s: TaskStatus = { taskId: task.taskId, state: "accepted" };
      this.sql.exec(
        "INSERT OR REPLACE INTO hub_tasks (task_id, run_id, session, status, destroyed, leased_ms, updated_ms) VALUES (?, ?, ?, ?, 0, ?, ?)",
        task.taskId,
        task.runId,
        task.session,
        JSON.stringify(s),
        now,
        now,
      );
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
        const r = this.row(task.taskId);
        if (r && this.statusOf(r).state === "accepted") {
          this.sql.exec("DELETE FROM hub_tasks WHERE task_id = ?", task.taskId);
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
      const r = this.row(task.taskId);
      const m = mergeStatus(r ? this.statusOf(r) : undefined, reported);
      this.sql.exec(
        "UPDATE hub_tasks SET status = ?, updated_ms = ? WHERE task_id = ?",
        JSON.stringify(m),
        Date.now(),
        task.taskId,
      );
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
    await this.pushTeam({ slotsUsed: this.slotsUsed() });
    return merged;
  }

  async taskStatus(taskId: string): Promise<TaskStatus> {
    await this.ready;
    const r = this.row(taskId);
    if (!r || r.destroyed === 1)
      throw new ProviderError("not_found", `task ${taskId} is not known to worker ${this.workerId()}`);
    return this.statusOf(r);
  }

  async taskCancel(taskId: string, graceSeconds: number): Promise<void> {
    await this.ready;
    const r = this.row(taskId);
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
        if (this.row(taskId)) {
          this.sql.exec("DELETE FROM hub_tasks WHERE task_id = ?", taskId);
          this.reportCapacity();
        }
      });
    } else {
      this.tx(() => {
        if (this.row(taskId)) {
          this.sql.exec("UPDATE hub_tasks SET destroyed = 1, updated_ms = ? WHERE task_id = ?", Date.now(), taskId);
          this.reportCapacity();
        }
      });
    }
    await this.pushTeam({ slotsUsed: this.slotsUsed() });
  }

  /** Orphan sweep: every task the worker holds when connected, else the leased cache. */
  async taskList(): Promise<TaskStatus[]> {
    await this.ready;
    if (this.current()) {
      const res = (await this.request("task.list", {})) as { tasks?: { task_id: string; status: unknown }[] | null };
      return (res.tasks ?? []).map((t) => fromWireStatus(t.status)).filter((s): s is TaskStatus => s !== null);
    }
    return this.rows()
      .filter((r) => r.destroyed === 0)
      .map((r) => this.statusOf(r));
  }

  async live(): Promise<{ online: boolean; slotsUsed: number; lastSeen: string | null; version: string | null }> {
    await this.ready;
    const ws = this.current();
    return {
      online: !!ws,
      slotsUsed: this.slotsUsed(),
      lastSeen: ws ? isoOf(this.lastSeen(ws)) : null,
      version: this.hello()?.version || null,
    };
  }

  /** Closes the socket and refuses new connections (the TeamDO record says revoked too). */
  async revoke(): Promise<void> {
    await this.ready;
    this.tx(() => {
      this.setState("revoked", "1");
      const rec = this.record();
      if (rec) this.setState("record", JSON.stringify({ ...rec, revoked: true }));
      this.delState("current_conn");
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
      const rec = this.record();
      if (!rec) return;
      this.setState("record", JSON.stringify({ ...rec, draining }));
      this.reportCapacity();
    });
    this.reconcile();
  }

  /** A changed record (PATCH, drain, resume) from the REST handler. */
  async configure(rec: WorkerRecord): Promise<void> {
    await this.ready;
    this.tx(() => {
      this.setState("record", JSON.stringify(rec));
      if (rec.revoked) this.setState("revoked", "1");
      this.reportCapacity();
    });
    if (rec.revoked) {
      await this.revoke();
      return;
    }
    this.reconcile();
  }
}
