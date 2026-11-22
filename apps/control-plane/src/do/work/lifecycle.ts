// The run lifecycle inside WorkDO: state transitions, message delivery, the
// turn-end reports, cancellation, and the work item completion that a turn
// end or a merge can cause. These functions call each other in a cycle (a
// transition reports a turn end, which can complete the work item, which
// cancels runs, which is a transition), so they share one module.
//
// Every function runs inside the caller's transactionSync; effects leave
// through fx.enqueue (outbox), fx.setTimer and fx.wake*. The lifecycle
// message to the parent is written in the transaction of the child's
// transition: there is no turn-ended job and no turn marker in KV.
import {
  conflict,
  invalid,
  InvalidTransitionError,
  newRoutedId,
  nextRunState as next,
  notFound,
  OfError,
  type RunStateInput as StateInput,
  type RunStateResult as RunResult,
} from "@openfactory/core";
import { failureForSandboxReason } from "@openfactory/sandbox";
import { dest } from "../../lib/outbox/dest";
import { decideDelivery, messageStartsTurn, outcomeFor, toItem } from "../../engine/deliver";
import { lifecycleDedupeKey, lifecycleEvent, lifecycleParts, runUrl, turnMarker } from "../../engine/lifecycle";
import { applyResult, iso, ms, priorRetries } from "../../engine/sessions";
import { firstLine, recipientsOf, runInputSender, truncate } from "../../engine/text";
import { transitionRows } from "../../engine/timeline";
import {
  isTerminal,
  stageTerminal,
  waitsForInput,
  type InboxItem,
  type MessageReceipt,
  type Run,
  type RunInput,
  type RunSession,
  type SendOutcome,
  type WorkItem,
} from "../../engine/types";
import { compactionDueMs } from "../../compaction/compact";
import { newMsgId, timerName, type Fx } from "./fx";
import { notify, notifyTop, writeBackComplete } from "./writeback";
import { projectInbox, projectRun, workItemEvent, writeTimeline } from "./projections";
import { cancelSlotRequest, enqueueProvider, endSession, requestSlot, stopSession } from "./sessions";
import { resolveInboxItem, setStage } from "./workitems";

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
    fx.repo.deleteEventContexts(r.id);
    fx.repo.deleteMeta(`slot_after:${r.id}`);
    scheduleDataUpkeep(fx, r);
    fx.clearTimer(timerName.requeue(r.id));
  }
}

// ---------- lost sessions ----------

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

// ---------- delivery ----------

/** The internal field of an input item that names its event context in R2; the runner API expands it to files. */
export const EVENT_FILES_REF = "event_files_ref";

/** The event_context key of the first prompt of a run (eventctx.PromptKVKey). */
export const promptContextKey = (runId: string): string => `prompt:${runId}`;

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
  const key = m.dedupe_key === "prompt" ? promptContextKey(m.run_id) : m.message_id;
  const ref = fx.repo.eventContextKey(key);
  return ref !== undefined ? { ...item, [EVENT_FILES_REF]: ref } : item;
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

/** reportTurnEnd, folded into the transition transaction. Runs once per turn marker. */
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

// ---------- messages ----------

/** sendMessageReceipt: the conversation message and the queue entry in one transaction. */
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

/** cancelTree (BFS over local children), except the run `skip`. Returns the cancelled IDs. */
export function cancelTree(fx: Fx, runId: string, actor: string, skip = ""): Set<string> {
  const done = new Set<string>();
  const seen = new Set<string>();
  const queue = [runId];
  while (queue.length > 0) {
    const id = queue.shift()!;
    if (seen.has(id)) continue;
    seen.add(id);
    if (id !== skip && cancelOne(fx, id, actor)) done.add(id);
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

// ---------- work item completion ----------

interface PendingMergeCompletion {
  run_id: string;
  actor: string;
  note: string;
  turns: number;
  was_done?: boolean;
}

/** armMergeCompletion: the fallback completion for the turn that the merge message starts. */
export function armMergeCompletion(fx: Fx, wiId: string, root: Run, actor: string, note: string): void {
  const p: PendingMergeCompletion = {
    run_id: root.id,
    actor,
    note,
    turns: root.turns ?? 0,
    ...(isTerminal(root.state) ? { was_done: true } : {}),
  };
  if (!waitsForInput(root.state) && !isTerminal(root.state)) p.turns++;
  fx.repo.setMeta(`merge_complete:${wiId}`, JSON.stringify(p));
}

/** checkMergeCompletion: called in the root run's turn-end transition. */
export function checkMergeCompletion(fx: Fx, r: Run): void {
  if (r.parent_run_id || !r.work_item_id) return;
  const key = `merge_complete:${r.work_item_id}`;
  const raw = fx.repo.getMeta(key);
  if (!raw) return;
  let p: PendingMergeCompletion;
  try {
    p = JSON.parse(raw) as PendingMergeCompletion;
  } catch {
    return;
  }
  if (p.run_id !== r.id) return;
  const ended = (r.turns ?? 0) > p.turns || (isTerminal(r.state) && !p.was_done);
  if (!ended) return;
  fx.repo.setMeta(key, "");
  const wi = fx.repo.getWorkItem(r.work_item_id);
  if (!wi || stageTerminal(wi.stage)) return;
  completeWorkItem(
    fx,
    wi.id,
    p.actor,
    p.note + " The foreman did not complete the work item in its turn after the merge, so the platform completed it.",
  );
}

/**
 * completeWorkItem (Engine.CompleteWorkItem): the stage, the tracker
 * write-back, a stop of the child runs of the root run (the root run winds
 * down on its own), the open inbox items of the work item and the completion
 * notice. A completed work item is left as it is; a cancelled one is a
 * conflict.
 */
export function completeWorkItem(fx: Fx, wiId: string, actor0: string, note: string): WorkItem {
  const actor = actor0 || "system";
  const cur = fx.repo.getWorkItem(wiId);
  if (!cur) throw notFound(`work item ${wiId}`);
  if (cur.stage === "complete") return cur;
  if (cur.stage === "cancelled") throw conflict("work item was cancelled");
  // The stage changes first, so the cancelled children do not report to the root run.
  const wi = setStage(fx, wiId, "complete", actor, note);
  // Move the tracker issues to done.
  writeBackComplete(fx, wi);
  if (wi.root_run_id) cancelTree(fx, wi.root_run_id, actor, wi.root_run_id);
  for (const it of fx.repo.listInbox({ resolved: false })) {
    if (it.work_item_id === wi.id) resolveInboxItem(fx, it, "work item completed", actor);
  }
  const out = fx.repo.getWorkItem(wi.id) ?? wi;
  notifyTop(fx, out, "update", note ? `This work item is complete. ${note}` : "This work item is complete.");
  return out;
}
