// The engine inside WorkDO: synchronous commit functions over the WorkDO
// tables (03 §12.1 "commit(tx, prepared, now): Effects"). Every function
// runs inside the caller's transactionSync; effects leave through
// fx.enqueue (outbox), fx.setTimer and fx.wake*.
//
// Ports internal/dispatch (transition, applyResult, deliver, send, cancel,
// lose, end) and internal/engine (createRun, Dispatch, RunAgents, cancel,
// lifecycle and turn-end reporting). The lifecycle message to the parent is
// written in the transaction of the child's transition (C1): there is no
// run.turn_ended job and no turn marker in KV.
import {
  conflict,
  forbidden,
  invalid,
  isTrackerIssue,
  ISSUE_STATE_DONE,
  ISSUE_STATE_IN_REVIEW,
  newRoutedId,
  notFound,
  OfError,
  OUTBOUND_ATTACH_PULL_REQUEST,
  OUTBOUND_ISSUE_STATE,
  REF_ISSUE,
  REF_SOURCE,
  workItemRefs,
  type PullRequestRef,
  type SourceRef,
} from "@openfactory/core";
import { failureForSandboxReason } from "@openfactory/sandbox";
import type { DispatchTarget } from "../../contracts/do-messages";
import type { EnqueueInput } from "../../lib/outbox/outbox";
import { dest } from "../../lib/outbox/dest";
import { decideDelivery, messageStartsTurn, outcomeFor, toItem } from "../../engine/deliver";
import { runPrincipals } from "../../engine/principal";
import { lifecycleDedupeKey, lifecycleEvent, lifecycleParts, runUrl, turnMarker } from "../../engine/lifecycle";
import {
  InvalidTransitionError,
  nextRunState as next,
  type RunStateInput as StateInput,
  type RunStateResult as RunResult,
} from "@openfactory/core";
import { applyResult, computeSeconds, iso, ms, priorRetries } from "../../engine/sessions";
import { recipientsOf, runInputSender, stageForAgent, truncate, firstLine } from "../../engine/text";
import { sessionEndedRow, taskIdOf, transitionRows, type TimelineRow } from "../../engine/timeline";
import {
  isTerminal,
  stageTerminal,
  waitsForInput,
  ZERO_USAGE,
  type AgentConfig,
  type InboxItem,
  type MessageReceipt,
  type Run,
  type RunInput,
  type RunSession,
  type SendOutcome,
  type Stage,
  type WorkItem,
} from "../../engine/types";
import type { Repo } from "./repo";
import { runView } from "../../engine/view";
import { compactionDueMs } from "../../compaction/compact";
import { checkMergeCompletion } from "./intake";

/** The root of a run tree (walks local parents). */
export function rootRunOf(fx: Fx, r: Run): string {
  let cur = r;
  for (let i = 0; i < 64 && cur.parent_run_id; i++) {
    const p = fx.repo.getRun(cur.parent_run_id);
    if (!p) break;
    cur = p;
  }
  return cur.id;
}

export interface EngineConfig {
  publicUrl: string;
  controlUrl: string;
  r2AccountId: string;
  queueTimeoutMs: number;
  maxSessionMs: number;
  stopGraceMs: number;
  startTimeoutMs: number;
  lostAfterMs: number;
  maxDepth: number;
  runnerVersion: string;
  allowUnenforced: string;
  /** Default task image (OF_SANDBOX_IMAGE); an environment or machine image wins (grant.ts selectImage). */
  sandboxImage: string;
  /** OF_OBJECT_STORE_RETENTION: run data in R2 older than this is pruned; 0 keeps it. */
  retentionMs?: number;
}

/** sandbox.DefaultImage of the Go server. */
export const DEFAULT_SANDBOX_IMAGE = "ghcr.io/openfactory-org/sandbox:latest";

export interface Fx {
  repo: Repo;
  now(): number;
  teamId: string;
  /** Partition token of this WorkDO. */
  token: string;
  doName: string;
  cfg: EngineConfig;
  target(workerHost: string | undefined): DispatchTarget;
  enqueue(input: EnqueueInput): string;
  setTimer(name: string, kind: string, dueMs: number, payload?: unknown): void;
  /** Sets the timer unless it is already due earlier. */
  setEarliestTimer?(name: string, kind: string, dueMs: number, payload?: unknown): void;
  clearTimer(name: string): void;
  wakeCommands(runId: string, session: number): void;
  wakeRun(runId: string): void;
}

// ---------- timer names ----------

export const timerName = {
  grant: (runId: string, n: number) => `grant:${runId}.${n}`,
  start: (task: string) => `start:${task}`,
  live: (task: string) => `live:${task}`,
  deadline: (task: string) => `deadline:${task}`,
  requeue: (runId: string) => `requeue:${runId}`,
  compact: (runId: string) => `compact:${runId}`,
  retention: "retention",
};

// ---------- helpers ----------

export function newMsgId(fx: Fx): string {
  return newRoutedId("msg", fx.token);
}

function writeTimeline(fx: Fx, runId: string, rows: TimelineRow[]): void {
  for (const r of rows) {
    fx.repo.appendTimeline({
      event_id: r.eventId,
      run_id: runId,
      session: r.session,
      type: r.type,
      at: iso(r.atMs),
      ...(r.data ? { data: r.data } : {}),
    });
  }
}

export function timeline(
  fx: Fx,
  runId: string,
  session: number,
  type: string,
  eventId: string,
  data?: Record<string, unknown>,
  atMs?: number,
): void {
  writeTimeline(fx, runId, [{ eventId, type, session, atMs: atMs ?? fx.now(), ...(data ? { data } : {}) }]);
}

/** e.timeline: a work-item event, projected to IndexDO for metrics. */
export function workItemEvent(
  fx: Fx,
  wiId: string | undefined,
  kind: string,
  runId: string,
  actor: string,
  data: Record<string, unknown>,
  id?: string,
): void {
  if (!wiId) return;
  const wi = fx.repo.getWorkItem(wiId);
  if (!wi) return;
  const eventId = id ?? newRoutedId("wie", fx.token);
  const at = iso(fx.now());
  fx.repo.appendWorkItemEvent({ id: eventId, work_item_id: wiId, kind, run_id: runId, actor, data, created_at: at });
  fx.enqueue({
    dest: dest.index(fx.teamId),
    kind: "index.workItemEvent",
    msgId: `wie:${eventId}`,
    payload: { wiId, factoryId: wi.factory_id, eventId, kind, atMs: fx.now(), data },
  });
}

export function projectRun(fx: Fx, r: Run): void {
  fx.enqueue({
    dest: dest.index(fx.teamId),
    kind: "index.project",
    msgId: `proj:run:${r.id}:${r.version}`,
    payload: {
      entity: "run",
      id: r.id,
      version: r.version,
      summary: { ...runView(r, { publicUrl: fx.cfg.publicUrl }), root_run_id: rootRunOf(fx, r), work_do: fx.doName },
    },
  });
  fx.wakeRun(r.id);
}

export function projectWorkItem(fx: Fx, wi: WorkItem): void {
  fx.enqueue({
    dest: dest.index(fx.teamId),
    kind: "index.project",
    msgId: `proj:wi:${wi.id}:${wi.version}`,
    payload: { entity: "work_item", id: wi.id, version: wi.version, summary: { ...wi, work_do: fx.doName } },
  });
  // Router status lane (C6): RouterDO ignores lower versions.
  fx.enqueue({
    dest: dest.router(fx.teamId),
    kind: "router.status",
    lane: "router",
    msgId: `rstat:${wi.id}:${wi.version}`,
    payload: {
      wiId: wi.id,
      factoryId: wi.factory_id,
      stage: wi.stage,
      completedMs: ms(wi.completed_at) ?? null,
      version: wi.version,
      keys: [wi.source_key ?? "", ...(wi.references ?? []).map((r) => r.key)].filter((k) => k !== ""),
    },
  });
}

export function projectInbox(fx: Fx, it: InboxItem): void {
  fx.enqueue({
    dest: dest.index(fx.teamId),
    kind: "index.project",
    msgId: `proj:inbox:${it.id}:${it.version}`,
    payload: { entity: "inbox", id: it.id, version: it.version, summary: { ...it, owner: `work:${fx.token}` } },
  });
}

/** e.notify: a notice to where the work item started, ordered per work item (rule 11). */
export function notify(
  fx: Fx,
  wi: WorkItem | undefined,
  kind: string,
  text: string,
  inboxId?: string,
  options?: string[],
): void {
  if (!wi) return;
  const seq = Number(fx.repo.getMeta(`notice_seq:${wi.id}`) ?? "0") + 1;
  fx.repo.setMeta(`notice_seq:${wi.id}`, String(seq));
  fx.enqueue({
    dest: "notice:",
    kind: "notice.responder",
    lane: `notice:${wi.id}`,
    msgId: `notice:${wi.id}:${seq}`,
    payload: {
      message: {
        work_item_id: wi.id,
        source: wi.source as Record<string, unknown>,
        text,
        kind,
        factory_id: wi.factory_id,
        ...(inboxId ? { inbox_id: inboxId } : {}),
        ...(options?.length ? { options } : {}),
      },
    },
  });
}

// ---------- tracker write-back (engine/references.go) ----------

/** trackerIssues: the linked issues of a work item, and its source when it is a tracker issue. */
export function trackerIssues(wi: WorkItem): SourceRef[] {
  const out: SourceRef[] = [];
  for (const r of workItemRefs(wi as never)) {
    if (r.kind === REF_ISSUE || (r.kind === REF_SOURCE && isTrackerIssue(r.source))) out.push(r.source);
  }
  return out;
}

/**
 * trackerOp: one write-back to a tracker. It goes through the outbox with the
 * other outbound messages (notice.tracker), so it commits with the change
 * that caused it.
 */
export function trackerOp(
  fx: Fx,
  wi: WorkItem,
  issue: SourceRef,
  kind: string,
  state: string,
  pr?: PullRequestRef,
): void {
  const seq = Number(fx.repo.getMeta(`notice_seq:${wi.id}`) ?? "0") + 1;
  fx.repo.setMeta(`notice_seq:${wi.id}`, String(seq));
  fx.enqueue({
    dest: "notice:",
    kind: "notice.tracker",
    lane: `notice:${wi.id}`,
    msgId: `notice:${wi.id}:${seq}`,
    payload: {
      message: {
        work_item_id: wi.id,
        factory_id: wi.factory_id,
        source: issue as unknown as Record<string, unknown>,
        text: "",
        kind,
        ...(state ? { issue_state: state } : {}),
        ...(kind === OUTBOUND_ATTACH_PULL_REQUEST && pr ? { pull_request: pr } : {}),
      },
    },
  });
}

/** writeBackPullRequest: attaches a newly linked pull request to the tracker issues and moves them to in review. */
export function writeBackPullRequest(fx: Fx, wi: WorkItem, pr: PullRequestRef): void {
  for (const issue of trackerIssues(wi)) {
    trackerOp(fx, wi, issue, OUTBOUND_ATTACH_PULL_REQUEST, "", pr);
    if (pr.state !== "merged" && pr.state !== "closed")
      trackerOp(fx, wi, issue, OUTBOUND_ISSUE_STATE, ISSUE_STATE_IN_REVIEW);
  }
}

/** writeBackComplete: moves the tracker issues of a completed work item to done. */
export function writeBackComplete(fx: Fx, wi: WorkItem): void {
  for (const issue of trackerIssues(wi)) trackerOp(fx, wi, issue, OUTBOUND_ISSUE_STATE, ISSUE_STATE_DONE);
}

// ---------- transition ----------

export interface TransitionResult {
  run: Run;
  changed: boolean;
  result?: RunResult;
}

/**
 * Dispatcher.transition + applyResult + timelineTransition, and what Go did
 * after commit: projection, slot request, lifecycle and turn-end reports.
 * An invalid transition writes nothing. `mutate` runs also when the state
 * does not change.
 */
export function transition(fx: Fx, runId: string, input: StateInput, mutate?: (r: Run) => void): TransitionResult {
  const r = fx.repo.mustRun(runId);
  let res: RunResult;
  try {
    res = next(r.state, input);
  } catch (e) {
    if (e instanceof InvalidTransitionError) {
      console.warn("engine: run event refused", runId, input.event, r.state);
      return { run: r, changed: false };
    }
    throw e;
  }
  const prev = r.state;
  if (res.changed) applyResult(r, res, fx.now());
  if (!res.changed && !mutate) return { run: r, changed: false, result: res };
  mutate?.(r);
  fx.repo.putRun(r);
  if (res.changed) {
    const latest = fx.repo.latestSession(r.id);
    writeTimeline(fx, r.id, transitionRows(r, input, res, latest?.session ?? 0, fx.now()));
  }
  afterRunWrite(fx, r, prev, res.changed);
  return { run: r, changed: res.changed, result: res };
}

/**
 * After a run finished: compaction of its messages to R2 (compactionDueMs) and
 * the retention sweep of this WorkDO (both run as queue jobs).
 */
export function scheduleDataUpkeep(fx: Fx, r: Run): void {
  const finished = ms(r.finished_at) ?? fx.now();
  fx.setTimer(timerName.compact(r.id), "compact", compactionDueMs(finished), { runId: r.id });
  const keep = fx.cfg.retentionMs ?? 0;
  if (keep > 0) {
    const due = finished + keep + 60_000;
    if (fx.setEarliestTimer) fx.setEarliestTimer(timerName.retention, "retention", due);
    else fx.setTimer(timerName.retention, "retention", due);
  }
}

/** Fields of a run without a state event (Dispatcher.update). */
export function updateRun(fx: Fx, runId: string, mutate: (r: Run) => void): Run {
  const r = fx.repo.mustRun(runId);
  mutate(r);
  fx.repo.putRun(r);
  projectRun(fx, r);
  return r;
}

function afterRunWrite(fx: Fx, r: Run, prev: string, changed: boolean): void {
  projectRun(fx, r);
  if (!changed) return;
  if (r.state === "queued") requestSlot(fx, r);
  if (r.state === "running" && prev !== "running") {
    const [to, side] = lifecycleTarget(fx, r);
    if (to) sendLifecycle(fx, r, to, side, "started");
  }
  if (waitsForInput(r.state) || isTerminal(r.state)) reportTurnEnd(fx, r);
  if (isTerminal(r.state)) {
    fx.repo.sql.exec("DELETE FROM event_context WHERE run_id = ?", r.id);
    fx.repo.sql.exec("DELETE FROM meta WHERE k = ?", `slot_after:${r.id}`);
    scheduleDataUpkeep(fx, r);
    fx.clearTimer(timerName.requeue(r.id));
  }
}

// ---------- slots (DispatchDO) ----------

/** dispatch.request for the next session of a queued run (03 §7.2 step 1). */
export function requestSlot(fx: Fx, r: Run): void {
  const latest = fx.repo.latestSession(r.id);
  if (latest && latest.state !== "ended") return;
  // After a start that failed for capacity, wait before the next request
  // (the Go dispatcher retried on its next poll, not at once).
  const after = Number(fx.repo.getMeta(`slot_after:${r.id}`) ?? "0");
  if (after > fx.now()) {
    fx.setTimer(timerName.requeue(r.id), "requeue", after, { runId: r.id });
    return;
  }
  const n = latest ? latest.session + 1 : 1;
  const queuedMs = ms(r.queued_at) ?? ms(r.created_at) ?? fx.now();
  fx.enqueue({
    dest: dest.dispatch(fx.teamId),
    kind: "dispatch.request",
    lane: "dispatch",
    msgId: `dreq:${r.id}:${n}:${queuedMs}`,
    payload: {
      runId: r.id,
      session: n,
      factoryId: r.factory_id,
      target: fx.target(r.config?.worker_host),
      queuedMs,
      runCreatedMs: ms(r.created_at) ?? fx.now(),
      restricted: !!r.config?.network_policy?.restricted,
      workDO: fx.doName,
    },
  });
}

function cancelSlotRequest(fx: Fx, r: Run): void {
  const latest = fx.repo.latestSession(r.id);
  const n = latest ? latest.session + 1 : 1;
  fx.enqueue({
    dest: dest.dispatch(fx.teamId),
    kind: "dispatch.cancel",
    lane: "dispatch",
    msgId: `dcancel:${r.id}:${n}:${fx.now()}`,
    payload: { runId: r.id, session: n },
  });
}

function providerDest(s: Pick<RunSession, "backend">): string {
  return `provider:${s.backend ?? ""}`;
}

export function enqueueProvider(
  fx: Fx,
  s: RunSession,
  kind: "provider.cancel" | "provider.destroy",
  graceSeconds = 0,
): void {
  const base = { providerId: s.backend ?? "", taskId: s.task_id, ...(s.worker_id ? { workerId: s.worker_id } : {}) };
  if (kind === "provider.cancel") {
    fx.enqueue({
      dest: providerDest(s),
      kind,
      lane: `task:${s.task_id}`,
      msgId: `pcancel:${s.task_id}:${fx.now()}`,
      payload: { ...base, graceSeconds },
    });
  } else {
    fx.enqueue({
      dest: providerDest(s),
      kind,
      lane: `task:${s.task_id}`,
      msgId: `pdestroy:${s.task_id}`,
      payload: base,
    });
  }
}

// ---------- sessions ----------

/** Dispatcher.endSession: false when the session was already ended. Releases the lease. */
export function endSession(
  fx: Fx,
  s: RunSession,
  reason: string,
  sandboxReason: string,
  opts: { destroy?: boolean } = {},
): boolean {
  if (s.state === "ended") return false;
  const now = fx.now();
  s.state = "ended";
  s.end_reason = reason;
  if (sandboxReason) s.sandbox_reason = sandboxReason;
  s.ended_at = iso(now);
  s.compute_seconds = computeSeconds(s, now);
  s.active_turn_id = "";
  fx.repo.putSession(s);
  writeTimeline(fx, s.run_id, [sessionEndedRow(s.run_id, s.session, reason, sandboxReason, now)]);
  for (const t of [timerName.start(s.task_id), timerName.live(s.task_id), timerName.deadline(s.task_id)])
    fx.clearTimer(t);
  fx.enqueue({
    dest: dest.dispatch(fx.teamId),
    kind: "dispatch.release",
    lane: "dispatch",
    msgId: `drel:${s.task_id}`,
    payload: { runId: s.run_id, session: s.session, computeSeconds: s.compute_seconds ?? 0, endReason: reason },
  });
  if (opts.destroy) enqueueProvider(fx, s, "provider.destroy");
  fx.wakeCommands(s.run_id, s.session);
  return true;
}

/** Dispatcher.loseCode: end as lost, reset inputs, apply session_lost (when code and applyEvent), destroy. */
export function loseCode(
  fx: Fx,
  s: RunSession,
  reason: string,
  code: string,
  applyEvent: boolean,
  opts: { cancel?: boolean } = {},
): void {
  if (opts.cancel) enqueueProvider(fx, s, "provider.cancel", 0);
  if (!endSession(fx, s, "lost", reason, { destroy: true })) return;
  fx.repo.resetSessionInputs(s.run_id, s.session);
  if (code && applyEvent) {
    transition(fx, s.run_id, {
      event: "session_lost",
      failureCode: code,
      priorRetries: priorRetries(fx.repo.listSessions(s.run_id), s.session),
    });
  }
}

/** Dispatcher.loseSession: the failure code of a sandbox reason. */
export function loseSession(
  fx: Fx,
  s: RunSession,
  reason: string,
  applyEvent: boolean,
  opts: { cancel?: boolean } = {},
): void {
  loseCode(fx, s, reason, failureForSandboxReason(reason) ?? "", applyEvent, opts);
}

/** Dispatcher.cancelSession: session.cancel, and a task cancel before session.ready. */
export function cancelSession(fx: Fx, s: RunSession): void {
  if (s.state === "ended") return;
  fx.repo.appendCommand(s, "session.cancel", null);
  fx.repo.putSession(s);
  fx.wakeCommands(s.run_id, s.session);
  if (s.state === "assigned" || !s.ready_at) {
    enqueueProvider(fx, s, "provider.cancel", Math.round(fx.cfg.stopGraceMs / 1000));
    endSession(fx, s, "cancelled", "", { destroy: true });
  }
}

/** Dispatcher.StopSession. */
export function stopSession(fx: Fx, runId: string): void {
  const latest = fx.repo.latestSession(runId);
  if (latest && latest.state !== "ended") cancelSession(fx, latest);
}

// ---------- delivery ----------

/** The internal field of an input item that names its event context in R2; the runner API expands it to files. */
export const EVENT_FILES_REF = "event_files_ref";

/**
 * addEventFiles (dispatch/eventfiles.go): the event context of a message. The
 * first prompt of a run has its own key. The bundle is read from R2 when the
 * Worker serves the command (no network I/O in the transaction).
 */
function withEventFiles<T extends object>(
  fx: Fx,
  m: { run_id: string; message_id: string; dedupe_key?: string },
  item: T,
): T {
  const key = m.dedupe_key === "prompt" ? `prompt:${m.run_id}` : m.message_id;
  const row = fx.repo.sql
    .exec<{ r2_key: string }>("SELECT r2_key FROM event_context WHERE message_id = ?", key)
    .toArray()[0];
  return row ? { ...item, [EVENT_FILES_REF]: row.r2_key } : item;
}

/** Dispatcher.deliver: pending messages to the live session (one turn claim inside the tx). */
export function deliver(fx: Fx, runId: string): void {
  const s = fx.repo.latestSession(runId);
  const pending = fx.repo.listInputs(runId, 0, ["pending"]);
  const d = decideDelivery(s, pending);
  if (d.kind === "none" || !s) return;
  const items = pending.map((m) => withEventFiles(fx, m, toItem(m)));
  const ids = pending.map((m) => m.message_id);
  if (d.kind === "start_turn" && spendStopsTurn(fx, runId)) {
    transition(fx, runId, { event: "spend_limit" }, (run) => {
      if (run.state === "error" && run.failure_code === "spend_limit_exceeded") {
        run.error = "spend_limit_exceeded: the spend limit of the factory or team is reached";
      }
    });
    stopSession(fx, runId);
    return;
  }
  if (d.kind === "start_turn") {
    const turnId = newRoutedId("turn", fx.token);
    s.active_turn_id = turnId;
    fx.repo.markDelivered(runId, ids, s.session, turnId);
    fx.repo.appendCommand(s, "turn.start", { turn_id: turnId, input: items });
  } else {
    fx.repo.markDelivered(runId, ids, s.session, s.active_turn_id ?? "");
    fx.repo.appendCommand(s, "message.deliver", { input: items });
  }
  fx.repo.putSession(s);
  fx.wakeCommands(runId, s.session);
}

/** The spend gate cache: DispatchDO sent work.spendStop for the factory less than 10 minutes ago. */
export const SPEND_GATE_TTL_MS = 10 * 60_000;
export function spendStopsTurn(fx: Fx, runId: string): boolean {
  const r = fx.repo.getRun(runId);
  if (!r) return false;
  const at = Number(fx.repo.getMeta(`spend_exceeded:${r.factory_id}`) ?? "0");
  return at > 0 && fx.now() - at < SPEND_GATE_TTL_MS;
}

// ---------- send ----------

/** Dispatcher.Send: queue a message and deliver it, or start a new session. */
export function send(fx: Fx, m: Omit<RunInput, "seq" | "state" | "created_at">): SendOutcome {
  const { created, input } = fx.repo.appendInput(m);
  const run = fx.repo.mustRun(m.run_id);
  const latest = fx.repo.latestSession(m.run_id);
  let outcome = outcomeFor(run.state, latest);
  if (outcome === "new_session" && !messageStartsTurn(input)) outcome = "stored";
  if (!created) return outcome;
  if (outcome === "live_session") deliver(fx, m.run_id);
  else if (outcome === "new_session") requeue(fx, m.run_id);
  return outcome;
}

function requeue(fx: Fx, runId: string): void {
  const { run } = transition(fx, runId, { event: "message", sandboxRunning: false });
  if (run.state !== "queued") throw conflict(`run ${runId} is ${run.state} and cannot continue`);
}

// ---------- lifecycle and turn end ----------

/** lifecycleTarget: the parent of a child run, or the root run for a side run of an active work item. */
export function lifecycleTarget(fx: Fx, r: Run): [string, boolean] {
  if (r.parent_run_id) return [r.parent_run_id, false];
  if (!r.work_item_id) return ["", false];
  const wi = fx.repo.getWorkItem(r.work_item_id);
  if (!wi || stageTerminal(wi.stage) || !wi.root_run_id || wi.root_run_id === r.id) return ["", false];
  return [wi.root_run_id, true];
}

function sendLifecycle(fx: Fx, child: Run, to: string, side: boolean, event: string): void {
  const parts = lifecycleParts(child, side, event, runUrl(fx.cfg.publicUrl, child.id));
  if (!fx.repo.getRun(to)) return;
  try {
    send(fx, {
      message_id: newMsgId(fx),
      run_id: to,
      kind: "lifecycle",
      sender: { type: "run", id: child.id, ...(child.name ? { name: child.name } : {}) },
      content: parts.content,
      data: parts.data,
      dedupe_key: lifecycleDedupeKey(child, event),
    });
  } catch (e) {
    // A parent that cannot continue (conflict) keeps the message queued.
    if (!(e instanceof OfError && e.code === "conflict")) throw e;
  }
}

/** turnEndMessage: the recipient of the lifecycle message of a turn end, or none. */
function turnEndMessage(fx: Fx, r: Run): [string, boolean, string] {
  const [to, side] = lifecycleTarget(fx, r);
  if (!to) return ["", false, ""];
  const parent = fx.repo.getRun(to);
  if (!parent || parent.state === "cancelled") return ["", false, ""];
  if (r.work_item_id) {
    const wi = fx.repo.getWorkItem(r.work_item_id);
    if (wi && stageTerminal(wi.stage)) return ["", false, ""];
  }
  if (r.state === "cancelled" && r.cancelled_by === `run:${to}`) return ["", false, ""];
  const event = lifecycleEvent(r.state);
  if (!event) return ["", false, ""];
  return [to, side, event];
}

/** reportTurnEnd, folded into the transition transaction (C1). Runs once per turn marker. */
function reportTurnEnd(fx: Fx, r: Run): void {
  const marker = turnMarker(r);
  if (fx.repo.getRunState(r.id, "turn_reported") === marker) return;
  fx.repo.setRunState(r.id, "turn_reported", marker);
  if (r.work_item_id && r.state !== "cancelled") {
    workItemEvent(
      fx,
      r.work_item_id,
      "run_finished",
      r.id,
      "system",
      { state: r.state, name: r.name ?? "", agent: r.agent_name ?? "", error: r.error ?? "" },
      `wie_${r.id}_finished_${marker.replace(/:/g, "_")}`,
    );
  }
  if (!r.parent_run_id && (r.state === "failed" || r.state === "error") && r.work_item_id) reportRootFailure(fx, r);
  if (!r.parent_run_id && r.state === "blocked" && r.blocked_reason === "harness_needs_input" && r.work_item_id) {
    reportBlocked(fx, r);
  }
  const [to, side, event] = turnEndMessage(fx, r);
  if (event) sendLifecycle(fx, r, to, side, event);
  checkMergeCompletion(fx, r);
  const judge = r.metadata?.["judge"];
  if (judge) {
    // A judge run reports judge.result; the scoring service finishes the score from the judge run.
    const msgId = `judge:${r.id}:${marker}`;
    fx.enqueue({
      dest: dest.jobs(),
      kind: "judge.result",
      msgId,
      payload: {
        kind: "judge.result",
        msgId,
        runId: r.metadata?.["judge_scored_run"] ?? r.id,
        scorerId: r.metadata?.["judge_scorer"] ?? "",
        result: {
          judge_run_id: r.id,
          judge,
          state: r.state,
          result: r.result ?? "",
          error: r.error ?? "",
          turn_marker: marker,
        },
      },
    });
    return;
  }
  fx.enqueue({
    dest: dest.jobs(),
    kind: "scoring.runFinished",
    msgId: `scoring:${r.id}:${marker}`,
    payload: {
      kind: "scoring.runFinished",
      msgId: `scoring:${r.id}:${marker}`,
      runId: r.id,
      state: r.state,
      turnMarker: marker,
      workDO: fx.doName,
      ...(r.metadata ? { metadata: r.metadata } : {}),
    },
  });
}

function openInboxForWorkItem(fx: Fx): InboxItem[] {
  return fx.repo.listInbox({ resolved: false });
}

function reportRootFailure(fx: Fx, r: Run): void {
  const wi = fx.repo.getWorkItem(r.work_item_id);
  if (!wi || stageTerminal(wi.stage)) return;
  if (openInboxForWorkItem(fx).length === 0) {
    const it: InboxItem = {
      id: newRoutedId("inb", fx.token),
      factory_id: r.factory_id,
      work_item_id: wi.id,
      run_id: r.id,
      kind: "failure",
      title: "Run failed: " + wi.title,
      body: truncate(r.error ?? "", 4000),
      ...(recipientsOf(wi) ? { recipients: recipientsOf(wi) } : {}),
      resolved: false,
      created_at: iso(fx.now()),
      version: 0,
    };
    fx.repo.createInbox(it);
    projectInbox(fx, it);
  }
  notify(
    fx,
    wi,
    "error",
    "The factory hit an error while working on this request: " + truncate(firstLine(r.error ?? ""), 500),
  );
}

function reportBlocked(fx: Fx, r: Run): void {
  const wi = fx.repo.getWorkItem(r.work_item_id);
  if (!wi || stageTerminal(wi.stage)) return;
  // openItemFor: one open item for the same people is enough.
  const rcpt = recipientsOf(wi) ?? [];
  const open = openInboxForWorkItem(fx).filter((i) => !i.work_item_id || i.work_item_id === wi.id);
  if (
    open.some((i) => {
      const got = i.recipients ?? [];
      return rcpt.length === 0 ? got.length === 0 : rcpt.some((x) => got.includes(x));
    })
  )
    return;
  const it: InboxItem = {
    id: newRoutedId("inb", fx.token),
    factory_id: r.factory_id,
    work_item_id: wi.id,
    run_id: r.id,
    kind: "blocked",
    title: "Run waits for input: " + wi.title,
    body: truncate(
      (r.result ?? "").trim() || "The agent stopped and waits for input. Answer to continue the run.",
      4000,
    ),
    ...(recipientsOf(wi) ? { recipients: recipientsOf(wi) } : {}),
    resolved: false,
    created_at: iso(fx.now()),
    version: 0,
  };
  fx.repo.createInbox(it);
  projectInbox(fx, it);
}

// ---------- create runs ----------

export interface NewRun {
  id?: string;
  factoryId: string;
  workItemId?: string;
  parentRunId?: string;
  config: AgentConfig;
  name?: string;
  prompt: string;
  metadata?: Record<string, string>;
  triggeredBy?: string;
  creator?: Run["creator"];
  runAs?: Run["run_as"];
  source?: string;
  sourceRef?: string;
}

/**
 * createRunWith: the run (queued), its first message, the first queue entry
 * (dedupe key "prompt") and the slot request, in one transaction (T15).
 */
export function createRun(
  fx: Fx,
  nr: NewRun,
  actor: string,
  msgId?: string,
  ledger?: { actionKey: string; target: string },
): Run {
  if (!nr.prompt.trim()) throw invalid("prompt is required");
  const now = iso(fx.now());
  const pr = runPrincipals(nr.factoryId, nr.triggeredBy, nr.creator, nr.runAs);
  const r: Run = {
    id: nr.id ?? newRoutedId("run", fx.token),
    version: 0,
    team_id: fx.teamId,
    factory_id: nr.factoryId,
    ...(nr.workItemId ? { work_item_id: nr.workItemId } : {}),
    ...(nr.parentRunId ? { parent_run_id: nr.parentRunId } : {}),
    ...(nr.config.id ? { agent_id: nr.config.id } : {}),
    agent_name: nr.config.name,
    name: nr.name || nr.config.name,
    state: "queued",
    prompt: nr.prompt,
    config: nr.config,
    ...(nr.metadata ? { metadata: nr.metadata } : {}),
    consumed_seq: 0,
    usage: { ...ZERO_USAGE },
    triggered_by: nr.triggeredBy || "api",
    ...(nr.source ? { source: nr.source } : {}),
    ...(nr.sourceRef ? { source_ref: nr.sourceRef } : {}),
    ...(pr.creator ? { creator: pr.creator } : {}),
    run_as: pr.runAs,
    created_at: now,
  };
  fx.repo.createRun(r);
  const mid = msgId || newMsgId(fx);
  fx.repo.appendMessage({
    id: mid,
    run_id: r.id,
    role: "user",
    content: r.prompt,
    ...(actor ? { data: { actor } } : {}),
    created_at: now,
  });
  fx.repo.appendInput({
    message_id: mid,
    run_id: r.id,
    kind: "user",
    sender: runInputSender(actor),
    content: r.prompt,
    dedupe_key: "prompt",
  });
  if (ledger) fx.repo.recordDispatch(ledger.actionKey, ledger.target, r.id);
  workItemEvent(fx, r.work_item_id, "run_started", r.id, actor, {
    agent: r.agent_name ?? "",
    name: r.name ?? "",
    parent_run_id: r.parent_run_id ?? "",
    triggered_by: r.triggered_by ?? "",
  });
  projectRun(fx, r);
  requestSlot(fx, r);
  return r;
}

// ---------- work items ----------

export interface NewWorkItem {
  id?: string;
  factoryId: string;
  title: string;
  source?: WorkItem["source"];
  sourceKey?: string;
  automationId?: string;
  creatorId?: string;
}

export function createWorkItem(fx: Fx, n: NewWorkItem, actor: string, eventText?: string): WorkItem {
  const now = iso(fx.now());
  const wi: WorkItem = {
    id: n.id ?? newRoutedId("wi", fx.token),
    factory_id: n.factoryId,
    title: n.title,
    stage: "intake",
    source: n.source ?? {},
    ...(n.sourceKey ? { source_key: n.sourceKey } : {}),
    ...(n.automationId ? { automation_id: n.automationId } : {}),
    ...(n.creatorId ? { creator_id: n.creatorId } : {}),
    created_at: now,
    updated_at: now,
    version: 0,
  };
  fx.repo.createWorkItem(wi);
  fx.repo.setMeta("work_item_id", wi.id);
  workItemEvent(fx, wi.id, "stage_changed", "", actor, { to: "intake" });
  if (eventText) workItemEvent(fx, wi.id, "message_in", "", actor, { text: truncate(eventText, 4000) });
  projectWorkItem(fx, wi);
  return wi;
}

/** setStage: a work item stage change with its timeline and router status. */
export function setStage(fx: Fx, wiId: string, stage: Stage, actor: string, reason = ""): WorkItem {
  const wi = fx.repo.getWorkItem(wiId);
  if (!wi) throw notFound(`work item ${wiId}`);
  if (stageTerminal(wi.stage) && wi.stage !== stage) throw conflict(`work item is ${wi.stage}`);
  if (wi.stage === stage) return wi;
  const from = wi.stage;
  wi.stage = stage;
  wi.updated_at = iso(fx.now());
  if (stageTerminal(stage)) wi.completed_at = wi.updated_at;
  fx.repo.putWorkItem(wi);
  workItemEvent(fx, wi.id, "stage_changed", "", actor, { from, to: stage, ...(reason ? { reason } : {}) });
  projectWorkItem(fx, wi);
  return wi;
}

/** inferStage: move forward to a stage of a started stage agent; never backward. */
export function inferStage(fx: Fx, wiId: string, stage: string, actor: string): void {
  const order = ["intake", "triage", "planning", "awaiting_approval", "building", "reviewing", "handoff"];
  const wi = fx.repo.getWorkItem(wiId);
  if (!wi || stageTerminal(wi.stage)) return;
  if (order.indexOf(stage) > order.indexOf(wi.stage)) setStage(fx, wiId, stage as Stage, actor);
}

export function isSideRun(fx: Fx, r: Run): boolean {
  if (!r.work_item_id || r.parent_run_id) return false;
  const wi = fx.repo.getWorkItem(r.work_item_id);
  return !!wi && !!wi.root_run_id && wi.root_run_id !== r.id;
}

export interface DispatchPlan {
  factoryId: string;
  config: AgentConfig;
  prompt: string;
  actor: string;
  title: string;
  source?: WorkItem["source"];
  sourceKey?: string;
  automationId?: string;
  creatorId?: string;
  metadata?: Record<string, string>;
  triggeredBy?: string;
  creator?: Run["creator"];
  runAs?: Run["run_as"];
  eventText?: string;
  /** Work item ID chosen by the caller (router reservation). */
  workItemId?: string;
  ledger?: { actionKey: string; target: string };
  /** Repositories at commits the root run starts from (benchmarks, judges). */
  startingRepoRefs?: unknown[];
  /** Agent pins of a trial; the caller applied them to config already. */
  agentPins?: Record<string, unknown>;
  /** Message ID of a follow-up to the root run (the event context names it). */
  messageId?: string;
}

/**
 * Engine.Dispatch inside the WorkDO of the work item: a follow-up to the
 * root run when the work item has one, else a new work item and root run
 * (T16 + T15 + T17 in one transaction).
 */
export function dispatchRoot(fx: Fx, p: DispatchPlan): { workItem: WorkItem; run: Run; followUp: boolean } {
  if (p.ledger) {
    const ref = fx.repo.hasDispatch(p.ledger.actionKey, p.ledger.target);
    if (ref) {
      const run = fx.repo.mustRun(ref) ?? fx.repo.getRun(ref);
      const wi = fx.repo.getWorkItem(run.work_item_id);
      if (wi) return { workItem: wi, run, followUp: false };
    }
  }
  let wi = fx.repo.getWorkItem(p.workItemId);
  if (wi && stageTerminal(wi.stage)) throw conflict(`work item ${wi.id} is ${wi.stage}`);
  if (wi?.root_run_id) {
    const root = fx.repo.getRun(wi.root_run_id);
    if (root && root.state !== "cancelled") {
      const msgId = p.messageId || newMsgId(fx);
      sendMessage(fx, root.id, p.prompt, p.actor, [], msgId);
      if (p.ledger) fx.repo.recordDispatch(p.ledger.actionKey, p.ledger.target, root.id);
      return { workItem: wi, run: fx.repo.mustRun(root.id), followUp: true };
    }
  }
  if (!wi) {
    wi = createWorkItem(
      fx,
      {
        ...(p.workItemId ? { id: p.workItemId } : {}),
        factoryId: p.factoryId,
        title: p.title,
        ...(p.source ? { source: p.source } : {}),
        ...(p.sourceKey ? { sourceKey: p.sourceKey } : {}),
        ...(p.automationId ? { automationId: p.automationId } : {}),
        ...(p.creatorId ? { creatorId: p.creatorId } : {}),
      },
      p.actor,
      p.eventText,
    );
  }
  const run = createRun(
    fx,
    {
      factoryId: p.factoryId,
      workItemId: wi.id,
      config: p.config,
      prompt: p.prompt,
      ...(p.metadata ? { metadata: p.metadata } : {}),
      triggeredBy: p.triggeredBy || "api",
      ...(p.creator ? { creator: p.creator } : {}),
      ...(p.runAs ? { runAs: p.runAs } : {}),
    },
    p.actor,
    undefined,
    p.ledger,
  );
  if (!wi.root_run_id) {
    wi.root_run_id = run.id;
    wi.updated_at = iso(fx.now());
    fx.repo.putWorkItem(wi);
    projectWorkItem(fx, wi);
  }
  return { workItem: wi, run, followUp: false };
}

// ---------- messages ----------

/** sendMessageReceipt: the conversation message and the queue entry in one transaction (T18). */
export function sendMessage(
  fx: Fx,
  runId: string,
  message: string,
  actor: string,
  attachments: string[] = [],
  msgId?: string,
): MessageReceipt {
  if (!message.trim()) throw invalid("message is required");
  const r = fx.repo.mustRun(runId);
  const id = msgId || newMsgId(fx);
  if (!fx.repo.getMessage(id)) {
    fx.repo.appendMessage({
      id,
      run_id: r.id,
      role: "user",
      content: message,
      data: attachments.length ? { actor, attachments } : { actor },
      created_at: iso(fx.now()),
    });
    if (r.work_item_id && !actor.startsWith("run:") && actor !== "system") {
      workItemEvent(fx, r.work_item_id, "message_in", r.id, actor, { text: truncate(message, 4000) });
    }
  }
  const outcome = send(fx, {
    message_id: id,
    run_id: runId,
    kind: actor.startsWith("run:") ? "agent" : "user",
    sender: runInputSender(actor),
    content: message,
    ...(attachments.length ? { attachments } : {}),
    dedupe_key: "msg:" + id,
  });
  return { message_id: id, outcome };
}

// ---------- cancel ----------

/** cancelOne: true when this call cancelled the run. Stops a live session. */
export function cancelOne(fx: Fx, runId: string, actor: string): boolean {
  const r = fx.repo.mustRun(runId);
  if (isTerminal(r.state)) {
    if (r.state !== "cancelled") stopSession(fx, runId);
    return false;
  }
  const prev = r.state;
  r.state = "cancelled";
  r.cancelled_by = actor;
  delete r.blocked_reason;
  r.finished_at = iso(fx.now());
  fx.repo.putRun(r);
  workItemEvent(fx, r.work_item_id, "run_finished", r.id, actor, { state: "cancelled", name: r.name ?? "" });
  if (prev === "queued") cancelSlotRequest(fx, r);
  stopSession(fx, runId);
  afterRunWrite(fx, r, prev, true);
  return true;
}

/** cancelTree (BFS over local children). Returns the cancelled IDs. */
export function cancelTree(fx: Fx, runId: string, actor: string): Set<string> {
  const done = new Set<string>();
  const seen = new Set<string>();
  const queue = [runId];
  while (queue.length > 0) {
    const id = queue.shift()!;
    if (seen.has(id)) continue;
    seen.add(id);
    if (cancelOne(fx, id, actor)) done.add(id);
    for (const c of fx.repo.listRuns({ parentRunId: id })) queue.push(c.id);
  }
  return done;
}

/** Engine.cancel: the tree (or one run), the work item of a root run, and the open inbox items. */
export function cancel(fx: Fx, runId: string, actor: string, cascade: boolean): Set<string> {
  const root = fx.repo.mustRun(runId);
  const cancelled = cascade ? cancelTree(fx, root.id, actor) : new Set(cancelOne(fx, root.id, actor) ? [root.id] : []);
  if (root.work_item_id) {
    const wi = fx.repo.getWorkItem(root.work_item_id);
    if (wi && wi.root_run_id === root.id && !stageTerminal(wi.stage)) {
      const cur = setStage(fx, wi.id, "cancelled", actor, "root run cancelled");
      notify(fx, cur, "update", "Work on this request was cancelled.");
    }
  }
  for (const it of fx.repo.listInbox({ resolved: false })) {
    if (it.run_id && cancelled.has(it.run_id)) resolveInboxItem(fx, it, "cancelled", actor);
  }
  return cancelled;
}

export function resolveInboxItem(fx: Fx, it: InboxItem, resolution: string, actor: string, via?: string): InboxItem {
  if (it.resolved) throw conflict("inbox item already resolved");
  it.resolved = true;
  it.resolution = resolution;
  it.resolved_by = actor;
  if (via) it.resolved_via = via;
  it.resolved_at = iso(fx.now());
  fx.repo.putInbox(it);
  projectInbox(fx, it);
  return it;
}

/**
 * resumesRun (workitems.go): whether a resolve sends the resolution to the
 * run. A handoff, an information-only item, a failure notice, a spend limit
 * and the review of a self-improvement pull request are bookkeeping.
 */
export function resumesRun(it: Pick<InboxItem, "kind" | "options">): boolean {
  switch (it.kind) {
    case "handoff":
    case "answered":
    case "failure":
    case "spend_limit":
      return false;
    case "review":
      return (it.options ?? []).length > 0;
  }
  return true;
}

/**
 * actorLabel (workitems.go): a readable name of an actor for text that people
 * and the model read, never a raw user ID. name is the user's name or email
 * when the caller looked it up.
 */
export function actorLabel(actor: string, name?: string): string {
  const n = (name ?? "").trim();
  if (n) return n;
  const a = actor.trim();
  if (!a || a === "api") return "a team member";
  const i = a.indexOf(":");
  if (i < 0) return a.startsWith("usr_") ? "a team member" : a;
  switch (a.slice(0, i)) {
    case "user":
      return "a team member";
    case "webhook":
    case "automation":
    case "trigger":
    case "schedule":
      return "an automation";
    case "run":
    case "agent":
    case "system":
      return "the factory";
  }
  return a;
}

/** The follow-up message of a resolve (ResolveInbox). */
export function resolutionMessage(it: Pick<InboxItem, "kind" | "title">, who: string, resolution: string): string {
  switch (it.kind) {
    case "approval":
      return `[human decision] Approval request ${JSON.stringify(it.title)} was resolved by ${who}: ${resolution}\n\nIf approved, continue with the next stage. If rejected or changes were requested, revise accordingly and ask again before implementation.`;
    case "question":
      return `[human answer] ${who} answered your question ${JSON.stringify(it.title)}:\n\n${resolution}`;
  }
  return `[human response] ${who} responded to ${JSON.stringify(it.title)} (${it.kind}):\n\n${resolution}`;
}

// ---------- run_agents ----------

export interface ChildPlan {
  config: AgentConfig;
  name: string;
  prompt: string;
  metadata?: Record<string, string>;
}

export const MAX_RUN_AGENTS_BATCH = 20;

/** checkOrchestrationDepth: walks local parents. A root run has depth 0. */
export function checkOrchestrationDepth(fx: Fx, parent: Run): void {
  let depth = 0;
  let cur: Run | undefined = parent;
  while (cur?.parent_run_id) {
    depth++;
    if (depth >= fx.cfg.maxDepth) break;
    cur = fx.repo.getRun(cur.parent_run_id);
  }
  if (depth >= fx.cfg.maxDepth) {
    throw forbidden(
      `run ${parent.id} is at the orchestration depth limit and cannot start child runs (OF_MAX_ORCHESTRATION_DEPTH is ${fx.cfg.maxDepth}; a root run has depth 0)`,
    );
  }
}

/** RunAgents: children of a parent run in one transaction (the parent lock is the transaction). */
export function runAgents(fx: Fx, parentId: string, plans: ChildPlan[], summary = ""): Run[] {
  if (plans.length === 0) throw invalid("agent_run_configs must contain at least one child");
  if (plans.length > MAX_RUN_AGENTS_BATCH)
    throw invalid(`at most ${MAX_RUN_AGENTS_BATCH} children per run_agents call`);
  const parent = fx.repo.mustRun(parentId);
  if (isTerminal(parent.state)) throw conflict(`parent run is ${parent.state}`);
  checkOrchestrationDepth(fx, parent);
  if (parent.work_item_id) {
    const wi = fx.repo.getWorkItem(parent.work_item_id);
    if (wi && stageTerminal(wi.stage)) throw conflict(`work item is ${wi.stage}`);
  }
  const callerName = parent.agent_name || parent.config.name;
  const actor = "run:" + parent.id;
  const out: Run[] = [];
  let best = "";
  let bestRank = 0;
  for (const p of plans) {
    if (p.config.type === "FOREMAN" || !canSpawnCheck(p.config, callerName, parent.config.type)) {
      throw forbidden(
        `agent ${JSON.stringify(p.config.name)} may not be started by ${JSON.stringify(callerName)} (spawnableBy)`,
      );
    }
    const child = createRun(
      fx,
      {
        factoryId: parent.factory_id,
        ...(parent.work_item_id ? { workItemId: parent.work_item_id } : {}),
        parentRunId: parent.id,
        config: p.config,
        name: p.name,
        prompt: p.prompt,
        ...(p.metadata ? { metadata: p.metadata } : {}),
        triggeredBy: actor,
        ...(parent.creator ? { creator: parent.creator } : {}),
        ...(parent.run_as ? { runAs: parent.run_as } : {}),
      },
      actor,
    );
    out.push(child);
    const [st, rank] = stageForAgent(p.config.type);
    if (rank > bestRank) [best, bestRank] = [st, rank];
  }
  if (parent.work_item_id) {
    if (summary) workItemEvent(fx, parent.work_item_id, "note", parent.id, actor, { summary, children: out.length });
    if (best && !isSideRun(fx, parent)) inferStage(fx, parent.work_item_id, best, actor);
  }
  return out;
}

function canSpawnCheck(target: AgentConfig, callerName: string, callerType: string): boolean {
  if (target.type === "FOREMAN") return false;
  if (!target.spawnable_by) return callerType === "FOREMAN";
  return target.spawnable_by.includes(callerName);
}

export { taskIdOf };
