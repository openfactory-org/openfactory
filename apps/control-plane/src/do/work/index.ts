// WorkDO (key team:<t>|work:<token>): one work item (or a run tree without
// one), its runs, sessions, runner protocol, inbox items, dispatch ledger and
// tool idempotency rows. Every request or alarm step is one transactionSync.
import {
  usageCoverage,
  conflict,
  doName,
  invalid,
  newRoutedId,
  notFound,
  OfError,
  partitionOf,
  pullRequestSource,
  sourceKey,
  type FailureCode,
} from "@openfactory/core";
import { failureForSandboxReason, gateNetworkPolicy, parseProviderError, type TaskStatus } from "@openfactory/sandbox";
import type { EndRequest, StartRequest } from "@openfactory/runner-proto";
import type { Ack, CallMeta, CoreEvent, OutboxEnvelope, OutboxPayloads } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { BaseDO, type Migration } from "../../lib/base";
import { LiveHub, isUpgrade } from "../../lib/live/index";
import type { OutboxRow } from "../../lib/outbox/outbox";
import type { TimerRow } from "../../lib/timers/timers";
import { decideDeadline, decideSilent, DEFAULT_LOST_AFTER_MS } from "../../engine/liveness";
import { parseDuration } from "../../engine/sessions";
import {
  isTerminal,
  STAGES,
  type InboxItem,
  type PullRequestRef,
  type Run,
  type RunSession,
  type Stage,
} from "../../engine/types";
import { defaultProviderId, providerFor, targetOf } from "../dispatch/providers";
import {
  createRun as engineCreateRun,
  resolveInboxItem,
  resumesRun,
  actorLabel,
  resolutionMessage,
  workItemEvent,
  cancel,
  cancelOne,
  setStage,
  dispatchRoot,
  loseCode,
  loseSession,
  requestSlot,
  DEFAULT_SANDBOX_IMAGE,
  runAgents,
  sendMessage,
  stopSession,
  transition,
  projectInbox,
  projectWorkItem,
  type ChildPlan,
  type DispatchPlan,
  type EngineConfig,
  type Fx,
} from "./engine";
import {
  buildTask,
  commitGrant,
  mintBootstrap,
  onGrant,
  startFailed,
  startSucceeded,
  taskPolicy,
  type Grant,
  type GrantPrep,
} from "./grant";
import { m0001 } from "./migrations/0001_init";
import { compactRunMessages, type CompactionRpc } from "../../compaction/compact";
import { pruneRunData, retentionMs, type RetentionRpc } from "../../retention/sweep";
import { Repo } from "./repo";
import {
  appendEvents,
  authSession,
  endSessionByRunner,
  enqueueCommand,
  failStart,
  pollCommands,
  rerr,
  sessionStart,
  type RunnerError,
} from "./runner";
import { runTool, type ToolCall, type ToolResult } from "./tools";
import { applyAction as applyIntake, completeWorkItem, type ApplyActionInput, type ApplyActionResult } from "./intake";

/** Delivers notices (notify service). The integrations workstream sets it; the default logs and acks. */
export type NoticeDeliverer = (env: Env, envelope: OutboxEnvelope) => Promise<void>;
let noticeDeliverer: NoticeDeliverer = async (_env, e) => {
  console.info("work: notice without a deliverer", e.kind, e.msgId);
};
export function setNoticeDeliverer(fn: NoticeDeliverer): void {
  noticeDeliverer = fn;
}

/** OF_OBJECT_STORE_RETENTION in ms; an invalid value keeps data (0) and is logged. */
function safeRetentionMs(env: Env): number {
  try {
    return retentionMs(env);
  } catch (e) {
    console.warn(
      "work: OF_OBJECT_STORE_RETENTION is invalid; run data is kept",
      e instanceof Error ? e.message : String(e),
    );
    return 0;
  }
}

export function engineConfig(env: Env): EngineConfig {
  const pub = (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
  return {
    publicUrl: pub,
    controlUrl: pub,
    r2AccountId: env.OF_R2_ACCOUNT_ID ?? "",
    queueTimeoutMs: parseDuration(env.OF_QUEUE_TIMEOUT, 30 * 60_000),
    maxSessionMs: parseDuration(env.OF_MAX_SESSION_DURATION, 8 * 3_600_000),
    stopGraceMs: parseDuration(env.OF_SESSION_STOP_GRACE, 2 * 60_000),
    startTimeoutMs: parseDuration(env.OF_SANDBOX_START_TIMEOUT, 30 * 60_000),
    lostAfterMs: DEFAULT_LOST_AFTER_MS,
    maxDepth: Number((env as unknown as Record<string, string>).OF_MAX_ORCHESTRATION_DEPTH ?? "") || 1,
    runnerVersion: env.OF_RUNNER_VERSION ?? "",
    allowUnenforced: env.OF_ALLOW_UNENFORCED_NETWORK_POLICY ?? "",
    sandboxImage: (env.OF_SANDBOX_IMAGE ?? "").trim() || DEFAULT_SANDBOX_IMAGE,
    retentionMs: safeRetentionMs(env),
  };
}

export interface CreateRunInput {
  /** Root dispatch (work item + root run, or a follow-up to the root run). */
  dispatch?: DispatchPlan;
  /** A run without a work item (POST /runs of a single agent, judge runs). */
  run?: {
    factoryId: string;
    config: Run["config"];
    prompt: string;
    name?: string;
    triggeredBy?: string;
    metadata?: Record<string, string>;
    actor?: string;
    runId?: string;
    /** Repositories at the commits a judge or trial run starts from (RunRepoRefsKey). */
    startingRepoRefs?: unknown[];
  };
}

export class WorkDO extends BaseDO implements CompactionRpc, RetentionRpc {
  private readonly live = new LiveHub(this.ctx);

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.onMessage("dispatch.grant", (p) => this.withFx((fx) => onGrant(fx, p)));
    this.onMessage("work.queueExpired", (p) =>
      this.withFx((fx) => {
        const r = fx.repo.getRun(p.runId);
        const latest = fx.repo.latestSession(p.runId);
        if (!r || r.state !== "queued" || (latest && latest.session >= p.session)) return;
        transition(fx, p.runId, { event: "queue_timeout" });
      }),
    );
    this.onMessage("work.spendHold", (p) =>
      this.withFx((fx) => {
        const r = fx.repo.getRun(p.runId);
        if (r?.state === "queued" && r.queued_reason !== "spend_limit") {
          fx.repo.putRun(Object.assign(r, { queued_reason: "spend_limit" }));
        }
      }),
    );
    this.onMessage("work.spendRelease", (p) =>
      this.withFx((fx) => {
        const r = fx.repo.getRun(p.runId);
        if (r) fx.repo.setMeta(`spend_exceeded:${r.factory_id}`, "");
        if (r?.queued_reason) {
          delete r.queued_reason;
          fx.repo.putRun(r);
        }
      }),
    );
    this.onMessage("work.spendStop", (p) => this.withFx((fx) => this.spendStop(fx, p.rootRunId)));
    this.onTimerAsync<Grant>("grant", (t) => this.prepareAndCommitGrant(t.payload));
    this.onTimer<{ runId: string }>("requeue", (t) =>
      this.fxSync((fx) => {
        const r = fx.repo.getRun(t.payload.runId);
        if (r?.state === "queued") requestSlot(fx, r);
      }),
    );
    this.onTimer<{ runId: string; session: number }>("start_timeout", (t) =>
      this.fxSync((fx) => {
        const s = fx.repo.getSession(t.payload.runId, t.payload.session);
        if (s?.state === "assigned")
          loseCode(fx, s, "deadline_exceeded", "sandbox_create_failed", true, { cancel: true });
      }),
    );
    this.onTimer<{ runId: string; session: number }>("deadline", (t) => this.fxSync((fx) => this.onDeadline(fx, t)));
    // Data upkeep of finished runs: each timer sends a queue job that calls back compactRun / retentionSweep.
    this.onTimerAsync<{ runId: string }>("compact", async (t) => {
      await this.env.JOBS.send({
        kind: "compaction.run",
        msgId: `compact:${t.payload.runId}`,
        doName: this.doName(),
        runId: t.payload.runId,
      });
    });
    this.onTimerAsync("retention", async () => {
      await this.env.JOBS.send({
        kind: "retention.sweep",
        msgId: `retention:${this.doName()}:${Date.now()}`,
        doName: this.doName(),
      });
    });
    this.onTimerAsync<{ runId: string; session: number }>("liveness", (t) => this.onLiveness(t));
  }

  protected migrations(): readonly Migration[] {
    return [m0001];
  }

  async ping(): Promise<string> {
    return "WorkDO";
  }

  // ---------- fx plumbing ----------

  private token(): string {
    const name = this.doName();
    const i = name.lastIndexOf("|work:");
    return i >= 0 ? name.slice(i + 6) : name;
  }

  private makeFx(wakes: { cmd: Set<string>; run: Set<string> }): Fx {
    return {
      repo: new Repo(this.sql, () => Date.now()),
      now: () => Date.now(),
      teamId: this.teamId(),
      token: this.token(),
      doName: this.doName(),
      cfg: engineConfig(this.env),
      target: (h) => targetOf(this.env, h),
      enqueue: (input) => this.enqueue(input),
      setTimer: (n, k, d, p) => this.setTimer(n, k, d, p),
      setEarliestTimer: (n, k, d, p) => this.timers.setEarliest(n, k, d, p ?? null),
      clearTimer: (n) => void this.clearTimer(n),
      wakeCommands: (runId, s) => wakes.cmd.add(`cmd:${runId}:${s}`),
      wakeRun: (runId) => wakes.run.add(`run:${runId}`),
    };
  }

  /** Runs fn in the current transaction (message handlers, sync timers), then flushes wakes. */
  private withFx<T>(fn: (fx: Fx) => T): T {
    const wakes = { cmd: new Set<string>(), run: new Set<string>() };
    const out = fn(this.makeFx(wakes));
    queueMicrotask(() => this.afterCommit(wakes));
    return out;
  }

  private fxSync<T>(fn: (fx: Fx) => T): T {
    return this.withFx(fn);
  }

  /** One transaction with an Fx, then live pushes and an outbox kick. */
  private txFx<T>(fn: (fx: Fx) => T): T {
    const wakes = { cmd: new Set<string>(), run: new Set<string>() };
    const out = this.tx(() => fn(this.makeFx(wakes)));
    this.afterCommit(wakes);
    return out;
  }

  private afterCommit(wakes: { cmd: Set<string>; run: Set<string> }): void {
    for (const tag of wakes.cmd) this.live.broadcast(tag, { type: "wake", channel: tag });
    for (const tag of wakes.run) this.live.broadcast(tag, { type: "wake", channel: tag });
    try {
      this.ctx.waitUntil(this.drainNow().then(() => undefined));
    } catch {
      /* no execution context (tests) */
    }
  }

  // ---------- live subscriptions ----------

  override async fetch(request: Request): Promise<Response> {
    await this.ready;
    const url = new URL(request.url);
    if (isUpgrade(request) && url.pathname === "/subscribe/commands") {
      const run = url.searchParams.get("run") ?? "";
      const session = Number(url.searchParams.get("session") ?? "0");
      const after = Number(url.searchParams.get("after") ?? "0");
      const tag = `cmd:${run}:${session}`;
      return this.live.accept([tag], { after }, (ws) => {
        const repo = new Repo(this.sql, () => Date.now());
        const s = repo.getSession(run, session);
        if (!s || s.state === "ended" || repo.listCommands(run, session, after).length > 0) {
          ws.send(JSON.stringify({ type: "wake", channel: tag }));
        }
      });
    }
    if (isUpgrade(request) && url.pathname === "/subscribe/run") {
      const run = url.searchParams.get("run") ?? "";
      return this.live.accept([`run:${run}`], {});
    }
    return new Response("not found", { status: 404 });
  }

  override async webSocketMessage(): Promise<void> {}
  override async webSocketClose(ws: WebSocket, code: number): Promise<void> {
    try {
      ws.close(code === 1005 ? 1000 : code, "bye");
    } catch {
      /* closed */
    }
  }

  // ---------- runs (Worker -> WorkDO) ----------

  async runsCreate(
    meta: CallMeta,
    input: CreateRunInput,
  ): Promise<{ run: Run; workItemId?: string; followUp?: boolean }> {
    return this.rpc(() =>
      this.txFx((fx) => {
        if (input.dispatch) {
          const out = dispatchRoot(fx, input.dispatch);
          // saveWorkItemState: the pins of a trial apply to the children that run_agents starts.
          const pins = input.dispatch.agentPins;
          if (pins && Object.keys(pins).length > 0)
            fx.repo.setMeta(`agent_pins:${out.workItem.id}`, JSON.stringify(pins));
          const refs = input.dispatch.startingRepoRefs;
          if (refs?.length && !out.followUp) fx.repo.setRunState(out.run.id, "repo_refs", JSON.stringify(refs));
          return { run: out.run, workItemId: out.workItem.id, followUp: out.followUp };
        }
        const r = input.run;
        if (!r) throw invalid("dispatch or run is required");
        if (r.runId && fx.repo.getRun(r.runId)) return { run: fx.repo.mustRun(r.runId) };
        const run = createRunOnly(fx, r, meta);
        if (r.startingRepoRefs?.length) fx.repo.setRunState(run.id, "repo_refs", JSON.stringify(r.startingRepoRefs));
        return { run };
      }),
    );
  }

  async runsMessage(
    _meta: CallMeta,
    runId: string,
    input: { message: string; actor: string; attachments?: string[]; messageId?: string },
  ) {
    return this.rpc(() =>
      this.txFx((fx) => sendMessage(fx, runId, input.message, input.actor, input.attachments ?? [], input.messageId)),
    );
  }

  async runsCancel(meta: CallMeta, runId: string, cascade: boolean): Promise<{ cancelled: string[]; run: Run }> {
    return this.rpc(() =>
      this.txFx((fx) => {
        const actor = meta.principal?.userId ?? meta.principal?.id ?? "api";
        const done = cancel(fx, runId, actor, cascade);
        return { cancelled: [...done], run: fx.repo.mustRun(runId) };
      }),
    );
  }

  /** Dispatcher.Interrupt. Throws operation_not_supported with reason no_active_turn / unsupported_harness. */
  async runsInterrupt(_meta: CallMeta, runId: string): Promise<{ ok: true }> {
    return this.rpc(() =>
      this.txFx((fx) => {
        fx.repo.mustRun(runId);
        const s = fx.repo.latestSession(runId);
        if (!s || s.state !== "active" || !s.active_turn_id) {
          throw new OfError("conflict", "the run has no active turn", { reason: "no_active_turn" });
        }
        if (!(s.capabilities ?? []).includes("interrupt")) {
          throw new OfError("operation_not_supported", "the harness of the run cannot interrupt a turn", {
            reason: "unsupported_harness",
          });
        }
        enqueueCommand(fx, s, "turn.interrupt", { turn_id: s.active_turn_id });
        return { ok: true as const };
      }),
    );
  }

  /** run_agents: children in one transaction. Plans are resolved by the caller (TeamDO config). */
  async runAgents(parentRunId: string, plans: ChildPlan[], summary = ""): Promise<Run[]> {
    return this.rpc(() => this.txFx((fx) => runAgents(fx, parentRunId, plans, summary)));
  }

  /** Orchestration tool call with its idempotency row in the same transaction (C8). */
  async toolCall(call: ToolCall): Promise<ToolResult> {
    return this.rpc(() => this.txFx((fx) => runTool(fx, call)));
  }

  /** Active turn of the latest session (the orchestration turn source). */
  async activeTurn(runId: string): Promise<string> {
    return this.rpc(() => new Repo(this.sql, Date.now).latestSession(runId)?.active_turn_id ?? "");
  }

  // ---------- data upkeep (WS13: compaction and retention) ----------

  /** CompactionRpc: one step that moves the messages of a finished run to R2. */
  async compactRun(runId: string): Promise<{ more: boolean }> {
    return this.rpc(async () => {
      const repo = new Repo(this.sql, Date.now);
      const r = repo.getRun(runId);
      if (!r || !isTerminal(r.state)) return { more: false };
      const out = await compactRunMessages(
        {
          bucket: this.env.DATA,
          now: () => Date.now(),
          readRows: (id, after, limit) =>
            this.sql
              .exec<{ seq: number; data: string }>(
                "SELECT seq, data FROM run_messages WHERE run_id = ? AND seq > ? ORDER BY seq LIMIT ?",
                id,
                after,
                limit,
              )
              .toArray(),
          deleteRows: (id, upto) =>
            this.tx(() => {
              const n = this.sql.exec("DELETE FROM run_messages WHERE run_id = ? AND seq <= ?", id, upto).rowsWritten;
              const prev = Number(repo.getRunState(id, "compacted_upto") ?? "0");
              if (upto > prev) repo.setRunState(id, "compacted_upto", String(upto));
              return n;
            }),
        },
        runId,
      );
      return { more: out.more };
    });
  }

  /**
   * RetentionRpc: one step that deletes the R2 data (transcript, compacted
   * messages, snapshots) of finished runs older than OF_OBJECT_STORE_RETENTION.
   * The rows stay. The next timer is set for the next run that expires.
   */
  async retentionSweep(): Promise<{ more: boolean }> {
    return this.rpc(async () => {
      const keep = safeRetentionMs(this.env);
      if (keep <= 0) return { more: false };
      const now = Date.now();
      const rows = this.sql
        .exec<{ id: string; data: string }>(
          "SELECT id, data FROM runs WHERE state IN ('succeeded', 'failed', 'error', 'cancelled') AND id NOT IN (SELECT run_id FROM run_state WHERE k = 'data_pruned')",
        )
        .toArray();
      const runs: { id: string; lastActivityMs: number }[] = [];
      for (const row of rows) {
        const r = JSON.parse(row.data) as Run;
        runs.push({ id: row.id, lastActivityMs: Date.parse(r.finished_at ?? r.created_at) || now });
      }
      const out = await pruneRunData({
        bucket: this.env.DATA,
        teamId: this.teamId(),
        runs,
        nowMs: now,
        retentionMs: keep,
      });
      this.tx(() => {
        const repo = new Repo(this.sql, Date.now);
        for (const id of out.pruned) repo.setRunState(id, "data_pruned", String(now));
        if (!out.more) {
          const left = runs.filter((r) => !out.pruned.includes(r.id)).map((r) => r.lastActivityMs);
          if (left.length > 0)
            this.timers.setEarliest("retention", "retention", Math.min(...left) + keep + 60_000, null);
        }
      });
      return { more: out.more };
    });
  }

  // ---------- reads ----------

  async getRun(runId: string): Promise<Run> {
    return this.rpc(() => new Repo(this.sql, Date.now).mustRun(runId));
  }

  async api(meta: CallMeta, op: string, params: Record<string, string>, body: unknown): Promise<unknown> {
    return this.rpc(() => {
      const repo = new Repo(this.sql, Date.now);
      const runId = params.runId ?? params.id ?? "";
      switch (op) {
        case "getRun":
          return repo.mustRun(runId);
        case "listRunMessages": {
          repo.mustRun(runId);
          // compacted_upto: messages at or below it are in R2 (runs/<id>/messages.ndjson).
          const upto = Number(repo.getRunState(runId, "compacted_upto") ?? "0");
          return {
            messages: repo.listMessages(runId, Number(params.after ?? 0), Number(params.limit ?? 0)),
            ...(upto > 0 ? { compacted_upto: upto } : {}),
          };
        }
        case "getAgentPins": {
          const raw = repo.getMeta(`agent_pins:${params.id ?? ""}`);
          return { pins: raw ? (JSON.parse(raw) as Record<string, unknown>) : {} };
        }
        case "listRunInputs":
          repo.mustRun(runId);
          return { inputs: repo.listInputs(runId) };
        case "getRunTimeline":
          repo.mustRun(runId);
          return { events: repo.listTimeline(runId) };
        case "listRunSessions":
          repo.mustRun(runId);
          return { sessions: repo.listSessions(runId).map((x) => sessionView(this.sql, x)) };
        case "getRunDiagnostics": {
          repo.mustRun(runId);
          const out: unknown[] = [];
          for (const s of repo.listSessions(runId))
            out.push(...repo.listEvents(runId, s.session).filter((e) => e.type === "diagnostic"));
          return { diagnostics: out };
        }
        case "listRunChildren":
          return { runs: repo.listRuns({ parentRunId: runId }) };
        case "getWorkItem": {
          const wi = repo.getWorkItem(params.id);
          if (!wi) throw notFound(`work item ${params.id}`);
          return wi;
        }
        case "getWorkItemTimeline": {
          const wi = repo.getWorkItem(params.id);
          if (!wi) throw notFound(`work item ${params.id}`);
          return { events: repo.listWorkItemEvents(wi.id) };
        }
        case "listWorkItemRuns": {
          const wi = repo.getWorkItem(params.id);
          if (!wi) throw notFound(`work item ${params.id}`);
          return { runs: repo.listRuns({ workItemId: wi.id }) };
        }
        case "listWorkItemInbox": {
          const wi = repo.getWorkItem(params.id);
          if (!wi) throw notFound(`work item ${params.id}`);
          return { items: repo.listInbox({ resolved: false }).filter((i) => i.work_item_id === wi.id) };
        }
        case "getInbox": {
          const it = repo.getInbox(params.id ?? "");
          if (!it) throw notFound(`inbox item ${params.id}`);
          return it;
        }
        case "resolveInbox":
          return this.txFx((fx) => resolveInbox(fx, params.id ?? "", body, meta));
        case "getRunStartCommits": {
          repo.mustRun(runId);
          const raw = repo.getRunState(runId, "start_commits");
          return { start_commits: raw ? (JSON.parse(raw) as unknown[]) : [] };
        }
        case "createInboxItem":
          return this.txFx((fx) => {
            const b = (body ?? {}) as Partial<InboxItem> & { track_key?: string };
            if (!b.kind || !b.title) throw invalid("kind and title are required");
            const key = b.track_key ? `inbox_track:${b.track_key}` : "";
            if (key) {
              const prev = fx.repo.getMeta(key);
              if (prev) return fx.repo.getInbox(prev);
            }
            const wi = b.work_item_id ? fx.repo.getWorkItem(b.work_item_id) : fx.repo.getWorkItem();
            // A caller may choose the ID (propose_definition_change names it in the version first).
            let id = newRoutedId("inb", fx.token);
            if (b.id) {
              if (!b.id.startsWith("inb_") || partitionOf(b.id) !== fx.token)
                throw invalid(`inbox item ID ${b.id} does not belong to this work item`);
              if (fx.repo.getInbox(b.id)) return fx.repo.getInbox(b.id);
              id = b.id;
            }
            const it: InboxItem = {
              id,
              factory_id: b.factory_id ?? wi?.factory_id ?? "",
              ...(wi ? { work_item_id: wi.id } : {}),
              ...(b.run_id ? { run_id: b.run_id } : {}),
              kind: b.kind,
              title: b.title,
              body: b.body ?? "",
              ...(b.options ? { options: b.options } : {}),
              ...(b.questions ? { questions: b.questions } : {}),
              ...(b.assignee_id ? { assignee_id: b.assignee_id } : {}),
              ...(b.recipients ? { recipients: b.recipients } : wi?.creator_id ? { recipients: [wi.creator_id] } : {}),
              resolved: false,
              created_at: new Date(fx.now()).toISOString(),
              version: 0,
            };
            fx.repo.createInbox(it);
            projectInbox(fx, it);
            if (key) fx.repo.setMeta(key, it.id);
            return it;
          });
        case "unresolveInbox":
          return this.txFx((fx) =>
            unresolveInbox(
              fx,
              params.id ?? "",
              actorOfMeta(meta),
              (body as { actor_label?: string } | null)?.actor_label,
            ),
          );
        case "appendRunLog":
          return this.txFx((fx) => {
            const r = fx.repo.mustRun(runId);
            const b = (body ?? {}) as { content?: string; data?: unknown };
            const m = fx.repo.appendMessage({
              id: newRoutedId("msg", fx.token),
              run_id: r.id,
              role: "system",
              content: b.content ?? "",
              ...(b.data !== undefined ? { data: b.data } : {}),
              created_at: new Date(fx.now()).toISOString(),
            });
            fx.wakeRun(r.id);
            return { message_id: m.id };
          });
        case "mcpPatchWorkItem":
          return this.txFx((fx) => {
            const wi = fx.repo.getWorkItem(params.id);
            if (!wi) throw notFound(`work item ${params.id}`);
            const b = (body ?? {}) as {
              local_owner?: string;
              labels?: Record<string, string>;
              event?: { kind: string; data?: Record<string, unknown>; run_id?: string; actor?: string };
            };
            if (b.local_owner !== undefined) wi.local_owner = b.local_owner;
            if (b.labels) wi.labels = { ...(wi.labels ?? {}), ...b.labels };
            if (b.local_owner !== undefined || b.labels) {
              wi.updated_at = new Date(fx.now()).toISOString();
              fx.repo.putWorkItem(wi);
              projectWorkItem(fx, wi);
            }
            if (b.event?.kind)
              workItemEvent(
                fx,
                wi.id,
                b.event.kind,
                b.event.run_id ?? "",
                b.event.actor ?? actorOfMeta(meta),
                b.event.data ?? {},
              );
            return fx.repo.getWorkItem(wi.id);
          });
        case "mcpLinkPullRequest":
          return this.txFx((fx) => {
            const wi = fx.repo.getWorkItem(params.id);
            if (!wi) throw notFound(`work item ${params.id}`);
            const pr = (body ?? {}) as PullRequestRef;
            if (!pr.repo || !pr.number) throw invalid("pull request repo and number are required");
            const list = wi.pull_requests ?? [];
            const i = list.findIndex((x) => x.forge === pr.forge && x.repo === pr.repo && x.number === pr.number);
            if (i >= 0) list[i] = { ...list[i]!, ...pr };
            else list.push(pr);
            wi.pull_requests = list;
            const key = sourceKey(pullRequestSource(pr as never));
            if (key && !(wi.references ?? []).some((r) => r.key === key)) {
              wi.references = [
                ...(wi.references ?? []),
                { kind: "pull_request", key, added_at: new Date(fx.now()).toISOString() },
              ];
            }
            wi.updated_at = new Date(fx.now()).toISOString();
            fx.repo.putWorkItem(wi);
            projectWorkItem(fx, wi);
            if (i < 0)
              workItemEvent(fx, wi.id, "pr_linked", "", actorOfMeta(meta), {
                repo: pr.repo,
                number: pr.number,
                url: pr.url ?? "",
              });
            return fx.repo.getWorkItem(wi.id);
          });
        case "workItemMessage":
          return this.txFx((fx) => {
            const wi = fx.repo.getWorkItem(params.id);
            if (!wi) throw notFound(`work item ${params.id}`);
            if (!wi.root_run_id) throw new OfError("operation_not_supported", `work item ${wi.id} has no foreman run`);
            const b = (body ?? {}) as { message?: string };
            if (!(b.message ?? "").trim()) throw invalid("message is required");
            const rec = sendMessage(fx, wi.root_run_id, b.message ?? "", actorOfMeta(meta));
            return { work_item_id: wi.id, run_id: wi.root_run_id, message_id: rec.message_id, outcome: rec.outcome };
          });
        case "workItemCancel":
          return this.txFx((fx) => {
            const wi = fx.repo.getWorkItem(params.id);
            if (!wi) throw notFound(`work item ${params.id}`);
            if (wi.stage === "cancelled" || wi.stage === "complete") throw conflict(`work item is already ${wi.stage}`);
            const actor = actorOfMeta(meta);
            const done = new Set<string>();
            for (const r of fx.repo.listRuns({ workItemId: wi.id })) {
              if (!r.parent_run_id) for (const id of cancel(fx, r.id, actor, true)) done.add(id);
            }
            const note = ((body ?? {}) as { reason?: string }).reason || "Cancelled via API";
            const cur = fx.repo.getWorkItem(wi.id)!;
            if (cur.stage !== "cancelled" && cur.stage !== "complete") setStage(fx, wi.id, "cancelled", actor, note);
            return { work_item: fx.repo.getWorkItem(wi.id), cancelled: [...done] };
          });
        case "workItemComplete":
          return this.txFx((fx) => {
            const wi = fx.repo.getWorkItem(params.id);
            if (!wi) throw notFound(`work item ${params.id}`);
            if (wi.stage === "cancelled" || wi.stage === "complete") throw conflict(`work item is already ${wi.stage}`);
            const actor = actorOfMeta(meta);
            for (const r of fx.repo.listRuns({ workItemId: wi.id })) if (r.parent_run_id) cancelOne(fx, r.id, actor);
            completeWorkItem(fx, wi.id, actor, ((body ?? {}) as { note?: string }).note ?? "");
            return fx.repo.getWorkItem(wi.id);
          });
        case "workItemStage":
          return this.txFx((fx) => {
            const b = (body ?? {}) as { stage?: string; reason?: string };
            if (!b.stage || !STAGES.includes(b.stage as Stage))
              throw invalid(`invalid stage ${JSON.stringify(b.stage ?? "")}`);
            return setStage(fx, params.id ?? "", b.stage as Stage, actorOfMeta(meta), b.reason ?? "");
          });
        default:
          throw new OfError("feature_not_available", `WorkDO does not serve ${op}`);
      }
    });
  }

  // ---------- runner protocol ----------

  async runnerStart(input: {
    bootstrapHash: string;
    instanceHash: string;
    req: StartRequest;
  }): Promise<{ run: Run; session: RunSession } | RunnerError> {
    return this.rpc(() =>
      this.txFx((fx) => {
        if (
          fx.cfg.runnerVersion &&
          input.req.runner_version !== fx.cfg.runnerVersion &&
          !isDevVersion(input.req.runner_version)
        ) {
          return rerr(
            426,
            "runner_version_mismatch",
            `runner version ${JSON.stringify(input.req.runner_version)} is not the served version ${JSON.stringify(fx.cfg.runnerVersion)}`,
          );
        }
        const res = sessionStart(fx, input);
        if ("error" in res) return res;
        return { run: res.run, session: res.session };
      }),
    );
  }

  /** The start configuration of a session failed with a configuration fault. */
  async runnerFailStart(runId: string, session: number, message: string): Promise<RunnerError> {
    return this.rpc(() => this.txFx((fx) => failStart(fx, runId, session, message)));
  }

  /** authSession for the routes that only need a live session (secrets, token, git credential). */
  async runnerAuth(runId: string, session: number): Promise<{ run: Run; session: RunSession } | RunnerError> {
    return this.rpc(() =>
      this.txFx((fx) => {
        const s = authSession(fx, runId, session);
        if ("error" in s) return s;
        return { run: fx.repo.mustRun(runId), session: s };
      }),
    );
  }

  async runnerEvents(runId: string, session: number, batch: unknown): Promise<{ ack: number } | RunnerError> {
    return this.rpc(() =>
      this.txFx((fx) =>
        appendEvents(fx, runId, session, (batch ?? { events: [] }) as Parameters<typeof appendEvents>[3]),
      ),
    );
  }

  async runnerCommands(runId: string, session: number, after: number): Promise<unknown> {
    return this.rpc(() => this.txFx((fx) => pollCommands(fx, runId, session, after)));
  }

  async runnerEnd(runId: string, session: number, input: unknown): Promise<unknown> {
    return this.rpc(() => this.txFx((fx) => endSessionByRunner(fx, runId, session, input as EndRequest)));
  }

  async runnerConversation(runId: string, session: number, after = 0): Promise<unknown> {
    return this.rpc(() =>
      this.txFx((fx) => {
        const s = authSession(fx, runId, session);
        if ("error" in s) return s;
        void after;
        // The rows of all messages; the Worker merges the compacted prefix from
        // R2 and builds the conversation (handleConversation in runnerapi/conversation.go).
        const upto = Number(fx.repo.getRunState(runId, "compacted_upto") ?? "0");
        return {
          run: fx.repo.mustRun(runId),
          messages: fx.repo.listMessages(runId, 0, 0),
          ...(upto > 0 ? { compacted_upto: upto } : {}),
        };
      }),
    );
  }

  /**
   * A task state pushed by its provider or WorkerHubDO. An exited task ends
   * its session as lost (the liveness rule for "exited"); other states only
   * count as a sign of life.
   */
  async sandboxState(runId: string, session: number, status: TaskStatus): Promise<{ ok: true }> {
    return this.rpc(() =>
      this.txFx((fx) => {
        const s = fx.repo.getSession(runId, session);
        if (!s || s.state === "ended") return { ok: true as const };
        if (status.state === "exited") {
          const reason = status.reason ?? "unknown";
          loseCode(fx, s, reason, failureForSandboxReason(reason) ?? "", true);
        }
        return { ok: true as const };
      }),
    );
  }

  // ---------- planner and transfer reads (Worker -> WorkDO) ----------

  /** Planner inputs: the work item, the starting repository refs and the start commits. */
  async plannerState(runId: string): Promise<{ workItem?: unknown; repoRefs: unknown[]; startCommits?: unknown[] }> {
    return this.rpc(() => {
      const repo = new Repo(this.sql, Date.now);
      const r = repo.mustRun(runId);
      const wi = r.work_item_id ? repo.getWorkItem(r.work_item_id) : undefined;
      const wiRefs = this.sql
        .exec<{ v: string }>("SELECT v FROM work_item_state WHERE k = 'repo_refs'")
        .toArray()[0]?.v;
      const raw = repo.getRunState(runId, "repo_refs") ?? wiRefs;
      const start = repo.getRunState(runId, "start_commits");
      return {
        ...(wi ? { workItem: wi } : {}),
        repoRefs: raw ? (JSON.parse(raw) as unknown[]) : [],
        ...(start ? { startCommits: JSON.parse(start) as unknown[] } : {}),
      };
    });
  }

  /** recordStartCommits: the first record wins (later sessions keep it). */
  async recordStartCommits(runId: string, list: unknown[]): Promise<{ ok: true }> {
    return this.rpc(() =>
      this.tx(() => {
        const repo = new Repo(this.sql, Date.now);
        if (list.length > 0 && !repo.getRunState(runId, "start_commits"))
          repo.setRunState(runId, "start_commits", JSON.stringify(list));
        return { ok: true as const };
      }),
    );
  }

  /** The timeline event secrets.delivered (names only; one per task). */
  async runnerSecretsDelivered(
    runId: string,
    session: number,
    data: { names: string[]; harness_credential: unknown },
  ): Promise<{ ok: true }> {
    return this.rpc(() =>
      this.txFx((fx) => {
        const s = fx.repo.getSession(runId, session);
        if (s) {
          fx.repo.appendTimeline({
            event_id: "secrets.delivered:" + s.task_id,
            run_id: runId,
            session,
            type: "secrets.delivered",
            at: new Date(fx.now()).toISOString(),
            data,
          });
        }
        return { ok: true as const };
      }),
    );
  }

  /**
   * snapshotFiles (transfer.go): for each part, the upload IDs of the newest
   * session before `before` that saved it.
   */
  async snapshotParts(
    runId: string,
    before: number,
  ): Promise<{ session: number; part: string; uploadIds: string[] }[]> {
    return this.rpc(() => {
      const rows = this.sql
        .exec<{ session: number; part: string; upload_ids: string }>(
          "SELECT session, part, upload_ids FROM snapshot_parts WHERE run_id = ? AND session < ? ORDER BY session DESC",
          runId,
          before,
        )
        .toArray();
      const out: { session: number; part: string; uploadIds: string[] }[] = [];
      const seen = new Set<string>();
      for (const r of rows) {
        if (seen.has(r.part)) continue;
        seen.add(r.part);
        out.push({ session: Number(r.session), part: r.part, uploadIds: JSON.parse(r.upload_ids) as string[] });
      }
      return out;
    });
  }

  /** Whether an input of the run names the attachment (downloads of kind attachment). */
  async inputsNameAttachment(runId: string, attachmentId: string): Promise<boolean> {
    return this.rpc(() =>
      new Repo(this.sql, Date.now).listInputs(runId).some((i) => (i.attachments ?? []).includes(attachmentId)),
    );
  }

  /** DispatchDO reconcile only. */
  async leaseCheck(runId: string, session: number): Promise<"open" | "ended"> {
    return this.rpc(() => {
      const repo = new Repo(this.sql, Date.now);
      const s = repo.getSession(runId, session);
      if (s) return s.state === "ended" ? "ended" : "open";
      const g = this.sql
        .exec<{ state: string }>("SELECT state FROM grants WHERE run_id = ? AND session = ?", runId, session)
        .toArray()[0];
      return g?.state === "pending" ? "open" : "ended";
    });
  }

  // ---------- intake (IngressDO -> WorkDO) ----------

  /** IngressDO -> WorkDO: one intake action, idempotent on (actionKey, target). */
  async applyAction(input: ApplyActionInput): Promise<ApplyActionResult> {
    return this.rpc(() => this.txFx((fx) => applyIntake(fx, input)));
  }

  /** The work item and its root run, for the intake decisions (rootAgentName). */
  async intakeState(): Promise<{
    workItemId?: string;
    stage?: string;
    rootRunId?: string;
    rootAgent?: string;
    creatorId?: string;
  }> {
    return this.rpc(() => {
      const repo = new Repo(this.sql, Date.now);
      const wi = repo.getWorkItem();
      if (!wi) return {};
      const root = wi.root_run_id ? repo.getRun(wi.root_run_id) : undefined;
      return {
        workItemId: wi.id,
        stage: wi.stage,
        ...(wi.creator_id ? { creatorId: wi.creator_id } : {}),
        ...(root ? { rootRunId: root.id, rootAgent: root.agent_name ?? "" } : {}),
      };
    });
  }

  /** allowBotAction: at most 20 bot actions per work item in one hour; an action counts once. */
  async allowBotAction(actionKey: string): Promise<boolean> {
    return this.rpc(() =>
      this.tx(() => {
        const now = Date.now();
        if (actionKey && this.sql.exec("SELECT 1 FROM bot_actions WHERE action_key = ?", actionKey).toArray().length)
          return true;
        this.sql.exec("DELETE FROM bot_actions WHERE at_ms < ?", now - 3_600_000);
        const n = Number(this.sql.exec<{ n: number }>("SELECT COUNT(*) AS n FROM bot_actions").one().n);
        if (n >= 20) return false;
        this.sql.exec(
          "INSERT OR IGNORE INTO bot_actions (action_key, at_ms) VALUES (?, ?)",
          actionKey || `anon:${now}:${Math.random()}`,
          now,
        );
        return true;
      }),
    );
  }

  async reopen(input: { actionKey: string; event: CoreEvent }): Promise<Ack> {
    return this.rpc(() =>
      this.txFx((fx) => {
        const wi = fx.repo.getWorkItem();
        if (!wi || wi.stage !== "complete") return { ok: true } as const;
        if (fx.repo.hasDispatch(input.actionKey, "reopen") !== undefined) return { ok: true } as const;
        wi.stage = "triage";
        delete wi.completed_at;
        wi.updated_at = new Date().toISOString();
        fx.repo.putWorkItem(wi);
        fx.repo.recordDispatch(input.actionKey, "reopen", wi.id);
        return { ok: true } as const;
      }),
    );
  }

  // ---------- grants ----------

  private async prepareAndCommitGrant(g: Grant): Promise<void> {
    let prep: GrantPrep;
    try {
      prep = await this.prepareGrant(g);
    } catch (e) {
      prep = { kind: "retry_later", grant: g, message: e instanceof Error ? e.message : String(e) };
    }
    this.txFx((fx) => commitGrant(fx, prep));
  }

  private async prepareGrant(g: Grant): Promise<GrantPrep> {
    const repo = new Repo(this.sql, Date.now);
    const run = repo.getRun(g.runId);
    if (!run || run.state !== "queued") return { kind: "release", grant: g, reason: "not queued" };
    const provider = providerFor(this.env, this.teamId(), g.providerId || defaultProviderId(this.env), g.workerId);
    const caps = await provider.capabilities();
    const fx = this.makeFx({ cmd: new Set(), run: new Set() });
    const policy = taskPolicy(fx, run);
    const gate = gateNetworkPolicy(caps, policy, { allowUnenforced: fx.cfg.allowUnenforced });
    if (!gate.ok) return { kind: "config_fault", grant: g, code: gate.code, message: gate.message };
    if (gate.warning) console.warn("work: gate:", gate.warning, run.id);
    const { token, hash } = await mintBootstrap(this.teamId(), run.id);
    const arch = caps.arch[0] ?? "amd64";
    const task = buildTask(fx, run, g.session, token, g, {
      arch,
      egress: gate.egress,
      loopbackDaemonPorts: caps.loopbackDaemonPorts,
      runnerSha256: await runnerSha256(this.env, arch),
      environmentImage: await environmentImage(this.env, run),
    });
    const pre = await provider.preflight(task);
    if (!pre.ok) {
      const info = failureClass(pre.code);
      if (info === "user") return { kind: "config_fault", grant: g, code: pre.code, message: pre.message };
      return { kind: "retry_later", grant: g, message: pre.message };
    }
    if (pre.resources) task.resources = pre.resources;
    return { kind: "start", grant: g, task, bootstrapHash: hash };
  }

  // ---------- outbox destinations ----------

  protected override async deliverCustom(envelope: OutboxEnvelope, row: OutboxRow): Promise<Ack | void> {
    if (row.dest.startsWith("notice:") || row.dest === "svc:notify") {
      await noticeDeliverer(this.env, envelope);
      return { ok: true };
    }
    if (row.dest.startsWith("provider:")) return this.deliverProvider(envelope);
    throw new Error(`no delivery for destination ${row.dest}`);
  }

  private async deliverProvider(envelope: OutboxEnvelope): Promise<Ack> {
    const team = this.teamId();
    switch (envelope.kind) {
      case "provider.start": {
        const p = envelope.payload as OutboxPayloads["provider.start"];
        const provider = providerFor(this.env, team, p.providerId, p.workerId);
        try {
          await provider.start(p.task);
          this.txFx((fx) =>
            startSucceeded(
              fx,
              p.task.runId,
              p.task.session,
              p.workerId ? `worker:${p.workerId}` : p.providerId,
              p.workerId,
            ),
          );
        } catch (e) {
          this.txFx((fx) => startFailed(fx, p.task.runId, p.task.session, e));
        }
        return { ok: true };
      }
      case "provider.ack" as never: {
        const p = envelope.payload as unknown as OutboxPayloads["provider.destroy"];
        const prov = providerOf(this.env, team, p.providerId, p.workerId) as { ack?: (id: string) => Promise<void> };
        if (typeof prov.ack === "function") await ignoreNotFound(prov.ack(p.taskId));
        return { ok: true };
      }
      case "provider.cancel": {
        const p = envelope.payload as OutboxPayloads["provider.cancel"];
        await ignoreNotFound(providerOf(this.env, team, p.providerId, p.workerId).cancel(p.taskId, p.graceSeconds));
        return { ok: true };
      }
      case "provider.destroy": {
        const p = envelope.payload as OutboxPayloads["provider.destroy"];
        await ignoreNotFound(providerOf(this.env, team, p.providerId, p.workerId).destroy(p.taskId));
        return { ok: true };
      }
    }
    throw new Error(`unexpected provider kind ${envelope.kind}`);
  }

  // ---------- timers ----------

  private onDeadline(fx: Fx, t: TimerRow<{ runId: string; session: number }>): void {
    const s = fx.repo.getSession(t.payload.runId, t.payload.session);
    if (!s) return;
    const d = decideDeadline(s, fx.repo.stopSentMs(s.run_id, s.session), fx.now(), {
      maxSessionMs: fx.cfg.maxSessionMs,
      stopGraceMs: fx.cfg.stopGraceMs,
      lostAfterMs: fx.cfg.lostAfterMs,
    });
    switch (d.kind) {
      case "wait":
        fx.setTimer(t.name, "deadline", d.atMs, t.payload);
        return;
      case "stop":
        enqueueCommand(fx, s, "session.stop", { reason: "deadline", snapshot: true });
        fx.repo.setStopSent(s.run_id, s.session, fx.now());
        if (d.applyDeadlineEvent) transition(fx, s.run_id, { event: "session_deadline" });
        fx.setTimer(t.name, "deadline", fx.now() + fx.cfg.stopGraceMs + fx.cfg.lostAfterMs + 1, t.payload);
        return;
      case "lose":
        loseSession(fx, s, "deadline_exceeded", d.applyEvent, { cancel: true });
        return;
    }
  }

  private async onLiveness(t: TimerRow<{ runId: string; session: number }>): Promise<void> {
    const repo = new Repo(this.sql, Date.now);
    const s = repo.getSession(t.payload.runId, t.payload.session);
    if (!s || s.state === "ended") return;
    const cfg = engineConfig(this.env);
    let d = decideSilent(s, Date.now(), { lostAfterMs: cfg.lostAfterMs });
    if (d.kind === "ask_status") {
      let status: TaskStatus | "not_found" | "unavailable";
      try {
        status = await providerOf(this.env, this.teamId(), s.backend ?? "", s.worker_id).status(s.task_id);
      } catch (e) {
        status = parseProviderError(e)?.kind === "not_found" ? "not_found" : "unavailable";
      }
      const cur = repo.getSession(s.run_id, s.session);
      if (!cur || cur.state === "ended") return;
      d = decideSilent(cur, Date.now(), { lostAfterMs: cfg.lostAfterMs }, status);
    }
    const action = d;
    this.txFx((fx) => {
      const cur = fx.repo.getSession(s.run_id, s.session);
      if (!cur || cur.state === "ended") return;
      if (action.kind === "wait" || action.kind === "ask_status") {
        const at = action.kind === "wait" ? action.recheckAtMs : Date.now() + cfg.lostAfterMs;
        fx.setTimer(t.name, "liveness", Math.max(at, Date.now() + 1000), t.payload);
        return;
      }
      if (action.useReasonMap)
        loseCode(fx, cur, action.reason, failureForSandboxReason(action.reason) ?? "", true, { cancel: action.cancel });
      else loseCode(fx, cur, action.reason, action.code, true, { cancel: action.cancel });
    });
  }

  /** work.spendStop: spend_limit on every run of the tree, and stop live sessions (N7). */
  private spendStop(fx: Fx, rootRunId: string): void {
    const root = fx.repo.getRun(rootRunId);
    // The spend gate before a new turn of a live session (deliver.go spendStopsTurn).
    if (root) fx.repo.setMeta(`spend_exceeded:${root.factory_id}`, String(fx.now()));
    const queue = [rootRunId];
    const seen = new Set<string>();
    while (queue.length) {
      const id = queue.shift()!;
      if (seen.has(id)) continue;
      seen.add(id);
      const r = fx.repo.getRun(id);
      if (!r) continue;
      const { changed } = transition(fx, id, { event: "spend_limit" }, (run) => {
        if (run.state === "error" && run.failure_code === "spend_limit_exceeded") {
          run.error = "spend_limit_exceeded: the spend limit of the factory or team is reached";
        }
      });
      if (changed) stopSession(fx, id);
      for (const c of fx.repo.listRuns({ parentRunId: id })) queue.push(c.id);
    }
  }

  // ---------- tests and tools ----------

  /** Runs due timers and delivers due outbox rows now (the alarm body). */
  async pump(): Promise<void> {
    await this.alarm();
  }
}

// ---------- helpers ----------

function createRunOnly(fx: Fx, r: NonNullable<CreateRunInput["run"]>, meta: CallMeta): Run {
  const actor = r.actor ?? meta.principal?.userId ?? "api";
  return engineCreateRun(
    fx,
    {
      ...(r.runId ? { id: r.runId } : {}),
      factoryId: r.factoryId,
      config: r.config,
      prompt: r.prompt,
      ...(r.name ? { name: r.name } : {}),
      ...(r.metadata ? { metadata: r.metadata } : {}),
      triggeredBy: r.triggeredBy ?? "api",
    },
    actor,
  );
}

/** ResolveInbox (T12): resolution, timeline and the answer message to the run in one transaction. */
function resolveInbox(fx: Fx, id: string, body: unknown, meta: CallMeta): unknown {
  const it = fx.repo.getInbox(id);
  if (!it) throw notFound(`inbox item ${id}`);
  const b = (body ?? {}) as { resolution?: string; message?: string; via?: string; actor_label?: string };
  const resolution = (b.resolution ?? "").trim();
  if (!resolution) throw invalid("resolution is required");
  const actor = meta.principal?.userId ?? meta.principal?.id ?? "api";
  resolveInboxItem(fx, it, resolution, actor, b.via);
  workItemEvent(fx, it.work_item_id, it.kind === "approval" ? "approval_decided" : "answer", it.run_id ?? "", actor, {
    inbox_id: it.id,
    resolution,
  });
  if (it.run_id && resumesRun(it)) {
    const text = b.message ? `${resolution}\n\n${b.message}` : resolution;
    const rec = sendMessage(fx, it.run_id, resolutionMessage(it, actorLabel(actor, b.actor_label), text), actor);
    it.answer_message_id = rec.message_id;
    fx.repo.putInbox(it);
  }
  return it;
}

/** UnresolveWindow (engine/inbox.go). */
export const UNRESOLVE_WINDOW_MS = 10 * 60_000;

/**
 * UnresolveInbox: undoes a resolve within 10 minutes, while the run did not
 * consume the answer. The run gets a message that withdraws the answer.
 */
export function unresolveInbox(fx: Fx, id: string, actor: string, label?: string): InboxItem {
  const it = fx.repo.getInbox(id);
  if (!it) throw notFound(`inbox item ${id}`);
  if (!it.resolved) throw conflict("inbox item is not resolved");
  const at = it.resolved_at ? Date.parse(it.resolved_at) : NaN;
  if (!Number.isFinite(at) || fx.now() - at > UNRESOLVE_WINDOW_MS)
    throw conflict("the resolve is older than 10m0s and cannot be undone");
  if (it.resolved_via === "forge" || (!it.resolved_by && !it.resolved_via))
    throw conflict("the platform closed this item and it cannot be undone");
  if (it.run_id && it.answer_message_id) {
    const input = fx.repo.getInput(it.answer_message_id);
    if (input && input.state !== "pending") throw conflict("the run already read the answer");
  }
  const old = it.resolution ?? "";
  it.resolved = false;
  delete it.resolution;
  delete it.resolved_by;
  delete it.resolved_via;
  delete it.resolved_at;
  delete it.answer_message_id;
  fx.repo.putInbox(it);
  projectInbox(fx, it);
  if (it.work_item_id)
    workItemEvent(fx, it.work_item_id, "note", it.run_id ?? "", actor, { inbox_id: it.id, text: "resolve undone" });
  if (it.run_id && resumesRun(it)) {
    const short = [...old].length > 500 ? [...old].slice(0, 499).join("") + "…" : old;
    const msg = `[human correction] ${actorLabel(actor, label)} withdrew the answer to ${JSON.stringify(it.title)} (it was: ${short}). Ignore that answer. A new answer will arrive as a follow-up message.`;
    try {
      sendMessage(fx, it.run_id, msg, actor);
    } catch (e) {
      console.warn("work: deliver inbox unresolve", it.id, e);
    }
  }
  return it;
}

function actorOfMeta(meta: CallMeta): string {
  return meta.principal?.userId ?? meta.principal?.id ?? "api";
}

/** SessionView (api/runs_ext.go): one session of a run with the snapshot parts it saved. */
function sessionView(sql: SqlStorage, s: RunSession): Record<string, unknown> {
  const parts = new Set(
    sql
      .exec<{ part: string }>("SELECT part FROM snapshot_parts WHERE run_id = ? AND session = ?", s.run_id, s.session)
      .toArray()
      .map((r) => r.part),
  );
  const u = s.usage ?? { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 };
  return {
    session: s.session,
    number: s.session,
    state: s.state,
    assigned_at: s.assigned_at,
    ...(s.started_at ? { started_at: s.started_at } : {}),
    ...(s.ready_at ? { ready_at: s.ready_at } : {}),
    ...(s.ended_at ? { ended_at: s.ended_at } : {}),
    ...(s.end_reason ? { end_reason: s.end_reason } : {}),
    ...(s.failure_code ? { failure_code: s.failure_code } : {}),
    ...(s.sandbox_reason ? { sandbox_reason: s.sandbox_reason } : {}),
    usage: { ...u, coverage: usageCoverage(u) },
    snapshot: { workspace: parts.has("workspace"), harness_state: parts.has("harness_state") },
  };
}

function isDevVersion(v: string): boolean {
  const major = v.replace(/^v/, "").split(".")[0] ?? "";
  return major === "" || !/^\d+$/.test(major);
}

function failureClass(code: FailureCode | string): "user" | "platform" {
  const user = [
    "invalid_configuration",
    "environment_setup_failed",
    "harness_auth_failed",
    "external_authentication_required",
    "integration_disabled",
    "agent_failed",
    "out_of_memory",
  ];
  return user.includes(code) ? "user" : "platform";
}

function providerOf(env: Env, team: string, backend: string, workerId?: string) {
  if (workerId) return providerFor(env, team, "", workerId);
  return providerFor(
    env,
    team,
    backend.startsWith("worker:") ? "" : backend,
    backend.startsWith("worker:") ? backend.slice(7) : undefined,
  );
}

/**
 * The image of the environment of a run (taskImageFor in the Go server). A
 * failed lookup is logged and gives no image, so the next image applies.
 */
async function environmentImage(env: Env, run: Run): Promise<string | undefined> {
  const id = run.config.environment_id;
  if (!id) return undefined;
  try {
    const meta: CallMeta = {
      requestId: `env-image:${run.id}`,
      principal: { kind: "run", id: run.id, runId: run.id, teamId: run.team_id, role: "admin", factoryRoles: {} },
    };
    const out = (await env.TEAM.getByName(doName.team(run.team_id || "default")).api(
      meta,
      "getEnvironment",
      { id },
      null,
    )) as { image?: string; $error?: { detail: string } };
    if (out?.$error) throw new Error(out.$error.detail);
    return out?.image?.trim() || undefined;
  } catch (e) {
    console.warn("work: sandbox image: read environment failed, using the next image", run.id, id, e);
    return undefined;
  }
}

/** The checksum of the published runner binary, or undefined when it is not in R2. */
async function runnerSha256(env: Env, arch: string): Promise<string | undefined> {
  const v = env.OF_RUNNER_VERSION;
  if (!v) return undefined;
  try {
    const obj = await env.DATA.get(`runner/${v}/openfactory-runtime-linux-${arch}.sha256`);
    const text = obj ? (await obj.text()).trim() : "";
    return text.split(/\s+/)[0] || undefined;
  } catch {
    return undefined;
  }
}

async function ignoreNotFound(p: Promise<void>): Promise<void> {
  try {
    await p;
  } catch (e) {
    if (parseProviderError(e)?.kind === "not_found") return;
    throw e;
  }
}
