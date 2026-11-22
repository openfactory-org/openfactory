// Frozen contract (C0). The Durable Object message catalogue: typed RPC between
// the Worker and the DOs, outbox kinds between DOs, and Queue messages.
// Sources: appendix 05 §5.4 (catalogue) and 03 §6 (payloads, keys, lanes).
//
// Rules (plan "Rules for the port"):
// - Every outbox row has a msgId; the receiver dedupes on it inside its own
//   transactionSync (inbox_dedupe). Idempotency keys are listed per kind.
// - Rows with the same non-empty lane are delivered in order; "" = unordered.
// - Payloads are JSON. A payload above 512 KiB goes to R2 by hash (payloadR2).
// - Authorization runs inside the owner DO: the Worker passes the Principal.

import type { FailureCode } from "@openfactory/core";
import type { SandboxTask, TaskStatus } from "@openfactory/sandbox";

// ---------- Shared ----------

export type Role = "user" | "editor" | "admin";

/** Who calls. The Worker authenticates; the owner DO authorizes (rule 6). */
export interface Principal {
  kind: "user" | "service_account" | "factory_mcp" | "run" | "worker";
  id: string;
  teamId: string;
  role: Role;
  /** Factory role overrides: factory ID -> role. */
  factoryRoles: Record<string, Role>;
  userId?: string;
  /** API key or session key ID, for "end sessions created from this key". */
  keyId?: string;
  /** Set for kind "run": the run the session token belongs to. */
  runId?: string;
}

/** Context passed with every Worker -> DO call. */
export interface CallMeta {
  requestId: string;
  principal?: Principal;
}

/** core.Event as JSON (snake_case, same as the Go json tags). */
export interface CoreEvent {
  id: string;
  team_id: string;
  factory_id?: string;
  provider: string;
  type: string;
  delivery_id?: string;
  action_key?: string;
  actor: {
    provider: string;
    external_id: string;
    handle: string;
    is_bot: boolean;
    user_id?: string;
    is_self?: boolean;
    trust?: string;
  };
  source: Record<string, unknown>;
  attributes: Record<string, unknown>;
  payload?: unknown;
  text?: string;
  received_at: string;
}

/** core.OutboundMessage as JSON. */
export interface CoreOutboundMessage {
  work_item_id: string;
  source: Record<string, unknown>;
  text: string;
  kind: string;
  options?: string[];
  inbox_id?: string;
  factory_id?: string;
  thread_ref?: string;
  issue_state?: string;
  pull_request?: { forge: string; repo: string; number: number; url?: string; branch?: string; state?: string };
}

/** A sandbox target: a provider, a worker or a pool (workerHost resolved). */
export type DispatchTarget =
  { kind: "provider"; providerId: string } | { kind: "worker"; workerId: string } | { kind: "pool"; label: string };

/** Reply of a receiver that could not apply the message now. The row stays pending. */
export interface RetryLater {
  retry: true;
  afterMs?: number;
}

export type Ack = { ok: true } | RetryLater;

// ---------- Outbox ----------

export interface OutboxEnvelope<K extends OutboxKind = OutboxKind> {
  /** Idempotency key seen by the receiver (see each kind). */
  msgId: string;
  kind: K;
  /** "" = no order; otherwise head-of-line per lane. */
  lane: string;
  /** Sender DO name, for dead letters and redrive. */
  origin: string;
  payload: OutboxPayloads[K];
  /** R2 key when the payload is larger than 512 KiB; payload is then {} . */
  payloadR2?: string;
}

export interface OutboxPayloads {
  // WorkDO -> DispatchDO (lane "dispatch")
  /** key (runId, session). 03: dispatch.slotRequest. */
  "dispatch.request": {
    runId: string;
    session: number;
    factoryId: string;
    target: DispatchTarget;
    queuedMs: number;
    runCreatedMs: number;
    restricted: boolean;
    workDO: string;
  };
  /** key (runId, session, "cancel"). */
  "dispatch.cancel": { runId: string; session: number };
  /** key (runId, session, "release"). */
  "dispatch.release": { runId: string; session: number; computeSeconds: number; endReason: string };
  /** key (runId, session, "started"). */
  "dispatch.taskStarted": { runId: string; session: number; taskId: string; startedMs: number };
  /** key (runId, session); upsert where seq is greater. costUsd is cumulative. */
  "dispatch.usage": {
    runId: string;
    session: number;
    seq: number;
    factoryId: string;
    runCreatedMs: number;
    costUsd: number;
    rootRunId?: string;
    workDO?: string;
  };
  // TeamDO -> DispatchDO
  /** key (factoryId, versionId). */
  "dispatch.limits": { factoryId: string; versionId: string; maxConcurrentRuns: number | null; spendLimit: unknown };
  // WorkerHubDO -> DispatchDO
  /** key (workerId, generation). 03: dispatch.workerCapacity. */
  "capacity.report": {
    workerId: string;
    kind: "microvm" | "process";
    online: boolean;
    draining: boolean;
    slots: number;
    used: number;
    labels: Record<string, string>;
    factories: string[];
    generation: number;
  };

  // DispatchDO -> WorkDO (lane "work:<token>")
  /** key (runId, session). 03: work.slotGranted. */
  "dispatch.grant": {
    runId: string;
    session: number;
    target: DispatchTarget;
    providerId: string;
    workerId?: string;
    leaseId: string;
  };
  /** key (runId, session, "expired"). */
  "work.queueExpired": { runId: string; session: number };
  /** key (runId, session, period, periodStart). */
  "work.spendHold": { runId: string; session: number; limit: unknown };
  "work.spendRelease": { runId: string; session: number };
  /** key (rootRunId, scope, period, periodStart). */
  "work.spendStop": { rootRunId: string; limit: unknown };

  // WorkDO -> RouterDO (lane "router"); receiver ignores lower versions.
  "router.status": {
    wiId: string;
    factoryId: string;
    stage: string;
    completedMs: number | null;
    version: number;
    keys: string[];
  };

  // TeamDO / router / Worker -> IngressDO(partition); key = dedupe key (event.id)
  "ingress.deliver": { partition: string; event: CoreEvent; dedupeKey: string };

  // WorkDO / IngressDO -> integrations (ordered, lane "notice:<wiId>"); key notice:<wi>:<seq>
  "notice.forge": { message: CoreOutboundMessage; integrationId?: string };
  "notice.responder": { message: CoreOutboundMessage; integrationId?: string };
  "notice.tracker": { message: CoreOutboundMessage; integrationId?: string };

  // WorkDO / DispatchDO -> provider or WorkerHubDO (lane "task:<taskId>"); key taskId
  "provider.start": { target: DispatchTarget; providerId: string; workerId?: string; task: SandboxTask };
  "provider.cancel": { providerId: string; workerId?: string; taskId: string; graceSeconds: number };
  "provider.destroy": { providerId: string; workerId?: string; taskId: string };
  "provider.status": { providerId: string; workerId?: string; taskId: string };
  /** key (taskId, "ack"). Sent after session.started so the provider stops its start watchdog. */
  "provider.ack": { target: DispatchTarget; providerId: string; taskId: string };

  // Any DO -> IndexDO (unordered, versioned)
  /** key (entity, id, version); upsert if newer. */
  "index.project": { entity: string; id: string; version: number; summary: Record<string, unknown> | null };
  /** key eventId. */
  "index.workItemEvent": {
    wiId: string;
    factoryId: string;
    eventId: string;
    kind: string;
    atMs: number;
    data: Record<string, unknown>;
  };
  /** key event.id; lane prfacts:<prKey>. */
  "index.prFacts": { prKey: string; event: CoreEvent };
  /** key: spend notice key or proposal version ID. */
  "index.inboxCreate": { item: Record<string, unknown> };
  /** key (originDO, msgId, attempts). */
  "index.deadLetter": { originDO: string; msgId: string; kind: string; lane: string; error: string; attempts: number };
}

export type OutboxKind = keyof OutboxPayloads;

export const OUTBOX_KINDS = [
  "dispatch.request",
  "dispatch.cancel",
  "dispatch.release",
  "dispatch.taskStarted",
  "dispatch.usage",
  "dispatch.limits",
  "capacity.report",
  "dispatch.grant",
  "work.queueExpired",
  "work.spendHold",
  "work.spendRelease",
  "work.spendStop",
  "router.status",
  "ingress.deliver",
  "notice.forge",
  "notice.responder",
  "notice.tracker",
  "provider.start",
  "provider.cancel",
  "provider.destroy",
  "provider.status",
  "provider.ack",
  "index.project",
  "index.workItemEvent",
  "index.prFacts",
  "index.inboxCreate",
  "index.deadLetter",
] as const satisfies readonly OutboxKind[];

/** Every DO that receives outbox rows exposes this one RPC method. */
export interface OutboxReceiver {
  receive(msg: OutboxEnvelope): Promise<Ack>;
}

// ---------- Queues (unordered fan-out only, rule 11) ----------

export type JobMessage =
  | { kind: "judge.result"; msgId: string; runId: string; scorerId: string; result: Record<string, unknown> }
  | { kind: "scoring.sample"; msgId: string; runId: string; factoryId: string }
  | {
      kind: "scoring.runFinished";
      msgId: string;
      runId: string;
      state: string;
      turnMarker: string;
      workDO: string;
      metadata?: Record<string, unknown>;
    }
  | {
      kind: "bench.trialUpdate";
      msgId: string;
      benchId: string;
      trialId: string;
      runId: string;
      state: string;
      turnMarker: string;
    }
  | { kind: "retention.sweep"; msgId: string; doName: string }
  | { kind: "compaction.run"; msgId: string; doName: string; runId: string };

export type JobKind = JobMessage["kind"];

// ---------- Synchronous RPC (Worker -> DO, DO -> DO) ----------
// Method names are the 05 §5.4 names in camelCase (auth.lookupApiKey -> authLookupApiKey).
// Inputs and results that are REST bodies use the openapi schema types from
// @openfactory/api-client; they are `unknown` here so this file stays small.

export interface ApiKeyLookup {
  keyId: string;
  principal: Principal;
}

/** TeamDO (key team:<id>). Request ID is the idempotency key of writes. */
export interface TeamRpc extends OutboxReceiver {
  authLookupApiKey(keyHash: string): Promise<ApiKeyLookup | null>;
  authSession(sessionId: string): Promise<Principal | null>;
  /** One transaction: definition version, compiled runtime, schedules, limits outbox. Network fetches happen before. */
  definitionApply(meta: CallMeta, factoryId: string, input: unknown): Promise<unknown>;
  /** Decrypts named secrets for a run (planner) and writes the audit row. */
  secretsResolve(
    meta: CallMeta,
    input: { factoryId: string; runId?: string; names: string[] },
  ): Promise<Record<string, string>>;
  /** Read model of the active version, cached by active_version_id. 03: team.configSnapshot. */
  configSnapshot(factoryId: string): Promise<unknown>;
  /** 03: team.proposeDefinition (tool idempotency key). */
  proposeDefinition(
    meta: CallMeta,
    input: {
      factoryId: string;
      files: Record<string, string>;
      message: string;
      authorRunId: string;
      idempotencyKey: string;
    },
  ): Promise<unknown>;
  workersEnroll(input: {
    enrollmentToken: string;
    name: string;
    publicKey: string;
    facts: Record<string, unknown>;
    labels: Record<string, string>;
    slots: number;
    slotSize: unknown;
  }): Promise<{ workerId: string }>;
  workersUpdate(meta: CallMeta, workerId: string, patch: unknown): Promise<unknown>;
  workersRevoke(meta: CallMeta, workerId: string): Promise<unknown>;
  /** REST handlers of TeamDO-owned routes (factories.*, settings.*, schedules.*, integrations.*, mcp.*). */
  api(meta: CallMeta, op: string, params: Record<string, string>, body: unknown): Promise<unknown>;
}

/** WorkDO (key team:<id>|work:<token>). Runner calls are keyed by (run, session, seq). */
export interface WorkRpc extends OutboxReceiver {
  runsCreate(meta: CallMeta, input: unknown): Promise<unknown>;
  runsMessage(meta: CallMeta, runId: string, input: unknown): Promise<unknown>;
  runsCancel(meta: CallMeta, runId: string, cascade: boolean): Promise<unknown>;
  runsInterrupt(meta: CallMeta, runId: string): Promise<unknown>;
  runnerStart(input: {
    runId: string;
    session: number;
    bootstrapSecret: string;
    runnerVersion: string;
    instanceId: string;
  }): Promise<unknown>;
  runnerSecrets(runId: string, session: number): Promise<unknown>;
  runnerEvents(runId: string, session: number, batch: unknown): Promise<unknown>;
  runnerCommands(runId: string, session: number, after: number): Promise<unknown>;
  runnerEnd(runId: string, session: number, input: unknown): Promise<unknown>;
  runnerUploads(runId: string, session: number, input: unknown): Promise<unknown>;
  runnerDownloads(runId: string, session: number, input: unknown): Promise<unknown>;
  runnerToken(runId: string, session: number, input: unknown): Promise<unknown>;
  runnerGitCredential(runId: string, session: number, input: unknown): Promise<unknown>;
  runnerCloudToken(runId: string, session: number, input: unknown): Promise<unknown>;
  inboxResolve(meta: CallMeta, inboxId: string, input: unknown): Promise<unknown>;
  refsLinkPullRequest(runId: string, input: unknown): Promise<unknown>;
  refsLinkIssue(runId: string, input: unknown): Promise<unknown>;
  /** IngressDO -> WorkDO, idempotent on (actionKey, target). */
  applyAction(input: {
    actionKey: string;
    target: string;
    kind: "continue" | "createRoot" | "sideRun" | "deliverToRoot" | "mention";
    event: CoreEvent;
    bundleRef?: string;
    dispatchRequest?: unknown;
  }): Promise<Ack>;
  allowBotAction(actionKey: string): Promise<boolean>;
  reopen(input: { actionKey: string; event: CoreEvent }): Promise<Ack>;
  /** DispatchDO reconcile only. */
  leaseCheck(runId: string, session: number): Promise<"open" | "ended">;
  /** REST handlers of WorkDO-owned routes. */
  api(meta: CallMeta, op: string, params: Record<string, string>, body: unknown): Promise<unknown>;
}

/** DispatchDO (key team:<id>|dispatch): admission only. */
export interface DispatchRpc extends OutboxReceiver {
  /** Read, cached 10 s by the caller. */
  spendGate(factoryId: string): Promise<{ allowed: boolean; limit?: unknown }>;
  spend(factoryId: string): Promise<unknown>;
}

/** RouterDO (key team:<id>|router). */
export interface RouterRpc extends OutboxReceiver {
  resolve(factoryId: string, sourceKey: string): Promise<{ wiId: string; token: string; status: string } | null>;
  /** Atomic. Returns the same token while reserved or active. */
  resolveOrReserve(input: {
    factoryId: string;
    sourceKeys: string[];
    actionKey: string;
    target: string;
  }): Promise<{ wiId: string; token: string; created: boolean }>;
  addReference(input: {
    factoryId: string;
    key: string;
    kind: string;
    wiId: string;
    token: string;
  }): Promise<{ ok: true } | { ok: false; ownerWiId: string }>;
  reopen(input: { factoryId: string; sourceKey: string; wiId: string }): Promise<boolean>;
}

/** IndexDO (key team:<id>|index): read model, team SSE source. */
export interface IndexRpc extends OutboxReceiver {
  /** lists.* : paged reads of the read model. */
  list(meta: CallMeta, op: string, params: Record<string, string>): Promise<unknown>;
  markInboxRead(meta: CallMeta, inboxId: string): Promise<void>;
}

/** IngressDO (key team:<id>|ingress:<partition>). */
export interface IngressRpc extends OutboxReceiver {
  enqueue(
    partition: string,
    event: CoreEvent,
    dedupeKey: string,
  ): Promise<{ accepted: boolean; deduplicated: boolean }>;
  redrive(msgId: string): Promise<boolean>;
  deliveries(meta: CallMeta, op: string, params: Record<string, string>): Promise<unknown>;
}

/** WorkerHubDO (key team:<id>|worker:<workerId>). The WebSocket upgrade goes through fetch(). */
export interface WorkerHubRpc extends OutboxReceiver {
  taskAssign(task: SandboxTask, extras?: Record<string, unknown>): Promise<TaskStatus>;
  taskCancel(taskId: string, graceSeconds: number): Promise<void>;
  taskStatus(taskId: string): Promise<TaskStatus>;
  taskDestroy(taskId: string): Promise<void>;
  taskList(): Promise<TaskStatus[]>;
  /** Live state for GET /workers. */
  live(): Promise<{ online: boolean; slotsUsed: number; lastSeen: string | null; version: string | null }>;
  /** Closes the socket with 4001 and refuses new connections. */
  revoke(): Promise<void>;
  drain(draining: boolean): Promise<void>;
}

/** BenchmarkDO (key team:<id>|bench:<token>). */
export interface BenchmarkRpc extends OutboxReceiver {
  start(meta: CallMeta, input: unknown): Promise<unknown>;
  get(meta: CallMeta): Promise<unknown>;
  cancel(meta: CallMeta): Promise<unknown>;
}

// ---------- Live updates (DO -> Worker hibernatable WebSocket) ----------

/** Frame a DO sends on a subscriber socket to wake a held long-poll or SSE stream. */
export type LiveFrame =
  | { type: "wake"; channel: string }
  | { type: "commands"; commands: unknown[] }
  | { type: "event"; id: string; event: string; data: unknown };

/** Failure code carried in session end, lease end and timeline rows. */
export type EndFailure = { code: FailureCode; message?: string };
