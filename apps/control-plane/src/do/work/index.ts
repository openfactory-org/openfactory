// WorkDO (key team:<t>|work:<token>): one work item (or a run tree without
// one), its runs, sessions, runner protocol, inbox items, dispatch ledger and
// tool idempotency rows. Every request or alarm step is one transactionSync.
import { invalid, OfError, sourceKey } from "@openfactory/core";
import { failureForSandboxReason, type TaskStatus } from "@openfactory/sandbox";
import type { EndRequest, StartRequest } from "@openfactory/runner-proto";
import type { Ack, CallMeta, CoreEvent, OutboxEnvelope, Role } from "../../contracts/do-messages";
import { authorizeInTeam } from "../../auth/roles";
import type { Env } from "../../contracts/env";
import { BaseDO, type Migration } from "../../lib/base";
import { LiveHub, isUpgrade } from "../../lib/live/index";
import type { OutboxRow } from "../../lib/outbox/outbox";
import { DEFAULT_LOST_AFTER_MS } from "../../engine/liveness";
import { parseDuration } from "../../engine/sessions";
import { isTerminal, type Run, type RunSession } from "../../engine/types";
import { targetOf } from "../dispatch/providers";
import {
  createRun as engineCreateRun,
  workItemEvent,
  cancel,
  dispatchRoot,
  loseCode,
  requestSlot,
  DEFAULT_SANDBOX_IMAGE,
  runAgents,
  sendMessage,
  stopSession,
  transition,
  projectWorkItem,
  type ChildPlan,
  type DispatchPlan,
  type EngineConfig,
  type Fx,
} from "./engine";
import { onGrant, type Grant } from "./grant";
import { m0001 } from "./migrations/0001_init";
import { m0002 } from "./migrations/0002_drop_work_item_state";
import { m0003 } from "./migrations/0003_run_messages_role";
import { m0004 } from "./migrations/0004_drop_unread_tables";
import { compactRunMessages, type CompactionRpc } from "../../compaction/compact";
import { retentionMs, type RetentionRpc } from "../../retention/sweep";
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
import { applyAction as applyIntake, type ApplyActionInput, type ApplyActionResult } from "./intake";
import { actorOfMeta, apiOp, scopeFactory, type ApiCall } from "./api-handlers";
import {
  deliverNotice,
  deliverProvider,
  onDeadline,
  onLiveness,
  prepareAndCommitGrant,
  retentionStep,
  type WorkHost,
} from "./effects";

export { setNoticeDeliverer, type NoticeDeliverer } from "./effects";

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
    maxDepth: Number(env.OF_MAX_ORCHESTRATION_DEPTH ?? "") || 1,
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
    this.onMessage("work.spendStop", (p) => this.withFx((fx) => spendStop(fx, p.rootRunId)));
    this.onTimerAsync<Grant>("grant", (t) => prepareAndCommitGrant(this.host(), t.payload));
    this.onTimer<{ runId: string }>("requeue", (t) =>
      this.withFx((fx) => {
        const r = fx.repo.getRun(t.payload.runId);
        if (r?.state === "queued") requestSlot(fx, r);
      }),
    );
    this.onTimer<{ runId: string; session: number }>("start_timeout", (t) =>
      this.withFx((fx) => {
        const s = fx.repo.getSession(t.payload.runId, t.payload.session);
        if (s?.state === "assigned")
          loseCode(fx, s, "deadline_exceeded", "sandbox_create_failed", true, { cancel: true });
      }),
    );
    this.onTimer<{ runId: string; session: number }>("deadline", (t) => this.withFx((fx) => onDeadline(fx, t)));
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
    this.onTimerAsync<{ runId: string; session: number }>("liveness", (t) => onLiveness(this.host(), t));
  }

  protected migrations(): readonly Migration[] {
    return [m0001, m0002, m0003, m0004];
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

  /**
   * Runs fn in the current step (message handlers, sync timers) and flushes
   * wakes after that step commits (BaseDO.onCommit). A rollback of the step,
   * from fn or from later work in the same transaction (the dedupe row of
   * BaseDO.receive), flushes nothing.
   */
  private withFx<T>(fn: (fx: Fx) => T): T {
    const wakes = { cmd: new Set<string>(), run: new Set<string>() };
    const out = fn(this.makeFx(wakes));
    this.onCommit(() => this.afterCommit(wakes));
    return out;
  }

  /**
   * One transaction with an Fx; live pushes and an outbox kick follow its
   * commit. Inside an outer step they wait for the outer commit.
   */
  private txFx<T>(fn: (fx: Fx) => T): T {
    return this.tx(() => this.withFx(fn));
  }

  /** The view of this DO that the asynchronous effects use (effects.ts). */
  private host(): WorkHost {
    return {
      env: this.env,
      teamId: this.teamId(),
      repo: () => new Repo(this.sql, Date.now),
      readFx: () => this.makeFx({ cmd: new Set(), run: new Set() }),
      txFx: (fn) => this.txFx(fn),
      tx: (fn) => this.tx(fn),
      setEarliestTimer: (n, k, d, p) => this.timers.setEarliest(n, k, d, p),
    };
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

  // ---------- runs (Worker -> WorkDO) ----------

  async runsCreate(
    meta: CallMeta,
    input: CreateRunInput,
  ): Promise<{ run: Run; workItemId?: string; followUp?: boolean }> {
    return this.rpc(() => {
      this.authorize(meta, input.dispatch?.factoryId ?? input.run?.factoryId ?? "", "user");
      return this.txFx((fx) => {
        if (input.dispatch) {
          const out = dispatchRoot(fx, input.dispatch);
          // saveWorkItemState: the pins and the starting repo refs of a trial apply to
          // every run of the work item, also the children that run_agents starts.
          const pins = input.dispatch.agentPins;
          if (pins && Object.keys(pins).length > 0)
            fx.repo.setMeta(`agent_pins:${out.workItem.id}`, JSON.stringify(pins));
          const refs = input.dispatch.startingRepoRefs;
          if (refs?.length) fx.repo.setMeta(`repo_refs:${out.workItem.id}`, JSON.stringify(refs));
          return { run: out.run, workItemId: out.workItem.id, followUp: out.followUp };
        }
        const r = input.run;
        if (!r) throw invalid("dispatch or run is required");
        if (r.runId && fx.repo.getRun(r.runId)) return { run: fx.repo.mustRun(r.runId) };
        const run = createRunOnly(fx, r, meta);
        if (r.startingRepoRefs?.length) fx.repo.setRunState(run.id, "repo_refs", JSON.stringify(r.startingRepoRefs));
        return { run };
      });
    });
  }

  async runsMessage(
    meta: CallMeta,
    runId: string,
    input: { message: string; actor: string; attachments?: string[]; messageId?: string },
  ) {
    return this.rpc(() => {
      this.authorize(meta, this.runFactory(runId), "user");
      return this.txFx((fx) =>
        sendMessage(fx, runId, input.message, input.actor, input.attachments ?? [], input.messageId),
      );
    });
  }

  async runsCancel(meta: CallMeta, runId: string, cascade: boolean): Promise<{ cancelled: string[]; run: Run }> {
    return this.rpc(() => {
      this.authorize(meta, this.runFactory(runId), "user");
      return this.txFx((fx) => {
        const actor = actorOfMeta(meta);
        const done = cancel(fx, runId, actor, cascade);
        return { cancelled: [...done], run: fx.repo.mustRun(runId) };
      });
    });
  }

  /** Dispatcher.Interrupt. Throws operation_not_supported with reason no_active_turn / unsupported_harness. */
  async runsInterrupt(meta: CallMeta, runId: string): Promise<{ ok: true }> {
    return this.rpc(() => {
      this.authorize(meta, this.runFactory(runId), "user");
      return this.txFx((fx) => {
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
      });
    });
  }

  /** run_agents: children in one transaction. Plans are resolved by the caller (TeamDO config). */
  async runAgents(parentRunId: string, plans: ChildPlan[], summary = ""): Promise<Run[]> {
    return this.rpc(() => this.txFx((fx) => runAgents(fx, parentRunId, plans, summary)));
  }

  /** Orchestration tool call with its idempotency row in the same transaction. */
  async toolCall(call: ToolCall): Promise<ToolResult> {
    return this.rpc(() => this.txFx((fx) => runTool(fx, call)));
  }

  /** Active turn of the latest session (the orchestration turn source). */
  async activeTurn(runId: string): Promise<string> {
    return this.rpc(() => new Repo(this.sql, Date.now).latestSession(runId)?.active_turn_id ?? "");
  }

  // ---------- data upkeep (compaction and retention) ----------

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
          readRows: (id, after, limit) => repo.messageRows(id, after, limit),
          deleteRows: (id, upto) =>
            this.tx(() => {
              const n = repo.deleteMessagesUpTo(id, upto);
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
    return this.rpc(() => retentionStep(this.host(), safeRetentionMs(this.env)));
  }

  // ---------- reads ----------

  async getRun(runId: string): Promise<Run> {
    return this.rpc(() => new Repo(this.sql, Date.now).mustRun(runId));
  }

  /**
   * Authorizes a caller that comes from a REST route on the factory of the
   * object of the call, like the Go route wrapper: the Worker does not check
   * the role of factory-scoped routes. A missing object checks the team role
   * (the op then answers 404). Internal calls carry no principal.
   */
  private authorize(meta: CallMeta | undefined, factoryId: string, min: Role): void {
    authorizeInTeam(meta?.principal, this.teamId(), factoryId, min);
  }

  /** The factory of a run of this DO, "" when the run does not exist. */
  private runFactory(runId: string): string {
    return new Repo(this.sql, Date.now).getRun(runId)?.factory_id ?? "";
  }

  /** One REST operation (api-handlers.ts), authorized on the factory of its object first. */
  async api(meta: CallMeta, op: string, params: Record<string, string>, body: unknown): Promise<unknown> {
    return this.rpc(() => {
      const repo = new Repo(this.sql, Date.now);
      const spec = apiOp(op);
      this.authorize(meta, scopeFactory(repo, spec?.scope ?? "run", params, body), spec?.role ?? "user");
      if (!spec) throw new OfError("feature_not_available", `WorkDO does not serve ${op}`);
      const call: ApiCall = {
        meta,
        params,
        body,
        runId: params.runId ?? params.id ?? "",
        repo,
        txFx: (fn) => this.txFx(fn),
        env: this.env,
        teamId: this.teamId(),
      };
      return spec.handle(call);
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
      // The refs of the run itself (a judge run), else the refs of its work item.
      const raw =
        repo.getRunState(runId, "repo_refs") ??
        (r.work_item_id ? repo.getMeta(`repo_refs:${r.work_item_id}`) : undefined);
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
    return this.rpc(() => new Repo(this.sql, Date.now).latestSnapshotParts(runId, before));
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
      return repo.grantState(runId, session) === "pending" ? "open" : "ended";
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
      this.tx(() => new Repo(this.sql, Date.now).claimBotAction(actionKey, Date.now(), 20, 3_600_000)),
    );
  }

  async reopen(input: { actionKey: string; event: CoreEvent }): Promise<Ack> {
    return this.rpc(() =>
      this.txFx((fx) => {
        const wi = fx.repo.getWorkItem();
        if (!wi || wi.stage !== "complete") return { ok: true } as const;
        if (fx.repo.hasDispatch(input.actionKey, "reopen") !== undefined) return { ok: true } as const;
        // references.go reopenWorkItem: the stage goes back to triage, with a
        // "reopened" timeline entry and the projection to IndexDO and RouterDO.
        const from = wi.stage;
        const completedAt = wi.completed_at;
        wi.stage = "triage";
        delete wi.completed_at;
        wi.updated_at = new Date(fx.now()).toISOString();
        fx.repo.putWorkItem(wi);
        fx.repo.recordDispatch(input.actionKey, "reopen", wi.id);
        const ev = input.event;
        workItemEvent(fx, wi.id, "reopened", "", "system", {
          from,
          to: "triage",
          reason: "event for a reference of the completed work item",
          provider: ev.provider,
          event: ev.type,
          key: sourceKey(ev.source),
          ...(completedAt ? { completed_at: completedAt } : {}),
        });
        projectWorkItem(fx, wi);
        return { ok: true } as const;
      }),
    );
  }

  // ---------- outbox destinations ----------

  protected override async deliverCustom(envelope: OutboxEnvelope, row: OutboxRow): Promise<Ack | void> {
    if (row.dest.startsWith("notice:") || row.dest === "svc:notify") return deliverNotice(this.env, envelope);
    if (row.dest.startsWith("provider:")) return deliverProvider(this.host(), envelope);
    throw new Error(`no delivery for destination ${row.dest}`);
  }

  // ---------- tests and tools ----------

  /** Runs due timers and delivers due outbox rows now (the alarm body). */
  async pump(): Promise<void> {
    await this.alarm();
  }
}

// ---------- helpers ----------

function createRunOnly(fx: Fx, r: NonNullable<CreateRunInput["run"]>, meta: CallMeta): Run {
  const actor = r.actor ?? (meta.principal?.userId ? actorOfMeta(meta) : "api");
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

/** work.spendStop: spend_limit on every run of the tree, and stop live sessions (N7). */
function spendStop(fx: Fx, rootRunId: string): void {
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

function isDevVersion(v: string): boolean {
  const major = v.replace(/^v/, "").split(".")[0] ?? "";
  return major === "" || !/^\d+$/.test(major);
}
